package playbook

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-ansible/inventory"
)

// ansiblePlaybookBin locates a real ansible-playbook binary for
// cross-validation against this package's own engine. Tests using it
// skip cleanly when none is available (e.g. in CI, which does not
// install Python) — matching the same pattern already used by
// go-ansible/vault, go-ansible/inventory, and go-ansible/template's
// interop tests.
func ansiblePlaybookBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("ANSIBLE_PLAYBOOK_BIN"); p != "" {
		return p
	}
	p, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Skip("ansible-playbook not found in PATH; skipping cross-validation against the reference implementation")
	}
	return p
}

// runRealAnsiblePlaybook runs bin against invPath/pbPath and returns
// its exit code (0 unless the process itself failed to start, which
// fails the test outright — a non-zero exit from a real playbook
// failure is a normal, expected outcome for some scenarios below).
func runRealAnsiblePlaybook(t *testing.T, bin, invPath, pbPath string) (stdout string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bin, "-i", invPath, pbPath)
	cmd.Env = append(os.Environ(), "ANSIBLE_HOST_KEY_CHECKING=False", "ANSIBLE_GATHERING=explicit", "ANSIBLE_DEPRECATION_WARNINGS=False")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running real ansible-playbook: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	return string(out), code
}

// runGoAnsiblePlaybook runs the same inventory/playbook pair through
// this package's own engine.
func runGoAnsiblePlaybook(t *testing.T, invPath, pbPath string) *RunResult {
	t.Helper()
	inv, err := inventory.Load(invPath)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	e := New(inv)
	e.BaseDir = filepath.Dir(pbPath)
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	return rr
}

// localInventoryFile writes a minimal inventory both real
// ansible-playbook and this package's own engine accept identically:
// one host, "localhost", forced to a local connection (so this test
// needs no real SSH target on either side).
func localInventoryFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "inventory.yml")
	if err := os.WriteFile(path, []byte("all:\n  hosts:\n    localhost:\n      ansible_connection: local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeInteropPlaybook(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "site.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestInteropFileContentMatchesReference runs the same copy+template
// tasks through both engines and checks the resulting files are
// byte-for-byte identical — the strongest possible cross-validation,
// since it compares real side effects rather than internal result
// shapes the two engines don't share.
func TestInteropFileContentMatchesReference(t *testing.T) {
	bin := ansiblePlaybookBin(t)

	for _, engine := range []string{"real", "go"} {
		dir := t.TempDir()
		inv := localInventoryFile(t, dir)
		copyDest := filepath.Join(dir, "copy-out.txt")
		tmplSrc := filepath.Join(dir, "tmpl.j2")
		tmplDest := filepath.Join(dir, "tmpl-out.txt")
		if err := os.WriteFile(tmplSrc, []byte("value={{ myvar }}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		pb := writeInteropPlaybook(t, dir, `- hosts: all
  gather_facts: false
  vars:
    myvar: hello_interop
  tasks:
    - name: copy a file
      copy:
        content: "fixed content\n"
        dest: `+copyDest+`
    - name: render a template
      template:
        src: `+tmplSrc+`
        dest: `+tmplDest+`
`)
		if engine == "real" {
			_, code := runRealAnsiblePlaybook(t, bin, inv, pb)
			if code != 0 {
				t.Fatalf("real ansible-playbook exit = %d", code)
			}
		} else {
			rr := runGoAnsiblePlaybook(t, inv, pb)
			if rr.Failed() {
				t.Fatalf("go-ansible run failed: %+v", rr.Plays)
			}
		}

		copyGot, err := os.ReadFile(copyDest)
		if err != nil {
			t.Fatalf("[%s] copy output: %v", engine, err)
		}
		if string(copyGot) != "fixed content\n" {
			t.Fatalf("[%s] copy output = %q", engine, copyGot)
		}
		tmplGot, err := os.ReadFile(tmplDest)
		if err != nil {
			t.Fatalf("[%s] template output: %v", engine, err)
		}
		if string(tmplGot) != "value=hello_interop\n" {
			t.Fatalf("[%s] template output = %q", engine, tmplGot)
		}
	}
}

// TestInteropIdempotencyMatchesReference runs the same copy task
// twice through each engine and checks both agree that the second run
// makes no change — real Ansible's most basic idempotency contract,
// and one this port's copy module must uphold identically.
func TestInteropIdempotencyMatchesReference(t *testing.T) {
	bin := ansiblePlaybookBin(t)

	for _, engine := range []string{"real", "go"} {
		dir := t.TempDir()
		inv := localInventoryFile(t, dir)
		dest := filepath.Join(dir, "out.txt")
		pb := writeInteropPlaybook(t, dir, `- hosts: all
  gather_facts: false
  tasks:
    - name: write once
      copy:
        content: "stable\n"
        dest: `+dest+`
`)
		var secondRunChanged bool
		if engine == "real" {
			if _, code := runRealAnsiblePlaybook(t, bin, inv, pb); code != 0 {
				t.Fatalf("first real run failed")
			}
			out, code := runRealAnsiblePlaybook(t, bin, inv, pb)
			if code != 0 {
				t.Fatalf("second real run failed:\n%s", out)
			}
			secondRunChanged = containsChangedLine(out)
		} else {
			if rr := runGoAnsiblePlaybook(t, inv, pb); rr.Failed() {
				t.Fatalf("first go run failed")
			}
			rr := runGoAnsiblePlaybook(t, inv, pb)
			if rr.Failed() {
				t.Fatalf("second go run failed: %+v", rr.Plays)
			}
			for _, r := range resultsFor(rr, "localhost") {
				if r.Task == "write once" && r.Changed {
					secondRunChanged = true
				}
			}
		}
		if secondRunChanged {
			t.Fatalf("[%s] second run of an unchanged copy reported changed", engine)
		}
	}
}

func containsChangedLine(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "changed: [localhost]") {
			return true
		}
	}
	return false
}

// TestInteropWhenSkipMatchesReference checks a when: false task
// produces the same "nothing happened" outcome on both sides (the
// target file must not exist afterward).
func TestInteropWhenSkipMatchesReference(t *testing.T) {
	bin := ansiblePlaybookBin(t)

	for _, engine := range []string{"real", "go"} {
		dir := t.TempDir()
		inv := localInventoryFile(t, dir)
		dest := filepath.Join(dir, "should-not-exist.txt")
		pb := writeInteropPlaybook(t, dir, `- hosts: all
  gather_facts: false
  tasks:
    - name: conditionally write
      when: false
      copy:
        content: "should not appear"
        dest: `+dest+`
`)
		var exitCode int
		if engine == "real" {
			_, exitCode = runRealAnsiblePlaybook(t, bin, inv, pb)
		} else {
			rr := runGoAnsiblePlaybook(t, inv, pb)
			if rr.Failed() {
				exitCode = 2
			}
		}
		if exitCode != 0 {
			t.Fatalf("[%s] exit = %d, want 0", engine, exitCode)
		}
		if _, err := os.Stat(dest); err == nil {
			t.Fatalf("[%s] file exists despite when: false", engine)
		}
	}
}

// TestInteropFailedTaskExitCodeMatchesReference checks a failing task
// produces a non-zero exit on both sides.
func TestInteropFailedTaskExitCodeMatchesReference(t *testing.T) {
	bin := ansiblePlaybookBin(t)

	dir := t.TempDir()
	inv := localInventoryFile(t, dir)
	pb := writeInteropPlaybook(t, dir, `- hosts: all
  gather_facts: false
  tasks:
    - name: always fails
      fail:
        msg: interop failure
`)
	_, realCode := runRealAnsiblePlaybook(t, bin, inv, pb)
	if realCode == 0 {
		t.Fatal("real ansible-playbook: want non-zero exit for a failing task")
	}

	rr := runGoAnsiblePlaybook(t, inv, pb)
	if !rr.Failed() {
		t.Fatal("go-ansible: want the run to be marked failed")
	}
}

// TestInteropMagicVariablesMatchReference checks inventory_hostname
// and playbook_dir render identically on both sides — these were
// found missing from this engine entirely by a benchmarks run that
// happened to diff rendered template output against real ansible-core;
// this makes that comparison a permanent, automated check instead of
// a one-off finding.
func TestInteropMagicVariablesMatchReference(t *testing.T) {
	bin := ansiblePlaybookBin(t)

	for _, engine := range []string{"real", "go"} {
		dir := t.TempDir()
		inv := localInventoryFile(t, dir)
		out := filepath.Join(dir, "magic-vars.txt")
		pb := writeInteropPlaybook(t, dir, `- hosts: all
  gather_facts: false
  tasks:
    - name: record magic vars
      copy:
        content: "{{ inventory_hostname }}|{{ playbook_dir }}"
        dest: `+out+`
`)
		if engine == "real" {
			if _, code := runRealAnsiblePlaybook(t, bin, inv, pb); code != 0 {
				t.Fatalf("real ansible-playbook run failed")
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			realOut := string(data)
			if !strings.HasPrefix(realOut, "localhost|"+dir) {
				t.Fatalf("real ansible-playbook: magic vars = %q, want prefix %q", realOut, "localhost|"+dir)
			}
		} else {
			if rr := runGoAnsiblePlaybook(t, inv, pb); rr.Failed() {
				t.Fatalf("go-ansible run failed")
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			goOut := string(data)
			if goOut != "localhost|"+dir {
				t.Fatalf("go-ansible: magic vars = %q, want %q", goOut, "localhost|"+dir)
			}
		}
	}
}

// TestInteropImportPlaybookMatchesReference checks that a playbook
// importing another one via import_playbook: runs every task from
// both files, on both sides — found missing entirely (only mentioned
// in a comment, never implemented) by a docs-refresh pass that
// happened to re-derive the engine feature matrix from the actual
// code instead of trusting an earlier snapshot.
func TestInteropImportPlaybookMatchesReference(t *testing.T) {
	bin := ansiblePlaybookBin(t)

	for _, engine := range []string{"real", "go"} {
		dir := t.TempDir()
		inv := localInventoryFile(t, dir)
		subOut := filepath.Join(dir, "sub-ran.txt")
		writeInteropPlaybook(t, dir, `- hosts: all
  gather_facts: false
  tasks:
    - name: mark sub ran
      copy:
        content: "ran"
        dest: `+subOut+`
`)
		// writeInteropPlaybook always writes "site.yml"; the imported
		// file needs its own name.
		subPath := filepath.Join(dir, "sub.yml")
		if err := os.Rename(filepath.Join(dir, "site.yml"), subPath); err != nil {
			t.Fatal(err)
		}
		pb := writeInteropPlaybook(t, dir, `- import_playbook: sub.yml
`)

		if engine == "real" {
			if _, code := runRealAnsiblePlaybook(t, bin, inv, pb); code != 0 {
				t.Fatalf("real ansible-playbook run failed")
			}
		} else {
			if rr := runGoAnsiblePlaybook(t, inv, pb); rr.Failed() {
				t.Fatalf("go-ansible run failed: %+v", rr)
			}
		}
		if _, err := os.Stat(subOut); err != nil {
			t.Fatalf("[%s] imported playbook's task never ran: %v", engine, err)
		}
	}
}
