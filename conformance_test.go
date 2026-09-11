package playbook

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-ansible/inventory"
)

// The fixtures under testdata/conformance are run through THIS engine and
// compared against output recorded from real ansible-core 2.21.4. The
// golden files are not hand-written: each was produced by running the
// same fixture through a real ansible-playbook.
//
// To re-record after deliberately changing a fixture (needs pkgx):
//
//	cd testdata/conformance/roles
//	W=$(mktemp -d); mkdir -p "$W/out"
//	pkgx +ansible.com -- ansible-playbook -i inventory.yml site.yml \
//	    -e log="$W/run.log" -e out_dir="$W/out" -e v_extra=from_extra_vars
//	cp "$W/run.log" expected/run.log && cp "$W"/out/* expected/out/
//
// Re-record only when the fixture changed on purpose. A golden that moves
// because the ENGINE changed is the suite reporting a real divergence,
// which is the whole point of keeping it in the repo — the differential
// harnesses that found these defects lived in a scratch directory and
// were lost between sessions twice.

// TestConformanceRoles covers roles, role dependencies, the include and
// import family, role files/templates, handlers, and the variable
// precedence ladder in one run. Five real defects were found here.
func TestConformanceRoles(t *testing.T) {
	const dir = "testdata/conformance/roles"

	work := t.TempDir()
	logPath := filepath.Join(work, "run.log")
	outDir := filepath.Join(work, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	pb, err := ParseFile(filepath.Join(dir, "site.yml"))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := inventory.Load(filepath.Join(dir, "inventory.yml"))
	if err != nil {
		t.Fatal(err)
	}

	e := New(inv)
	e.BaseDir = dir
	e.ExtraVars = map[string]any{
		"log": logPath, "out_dir": outDir, "v_extra": "from_extra_vars",
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		for _, p := range rr.Plays {
			for _, r := range p.Results {
				if r.Failed {
					t.Errorf("task %q failed: %s", r.Task, r.Msg)
				}
			}
		}
		t.FailNow()
	}

	assertMatchesGolden(t, filepath.Join(dir, "expected", "run.log"), logPath)
	golden := filepath.Join(dir, "expected", "out")
	entries, err := os.ReadDir(golden)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		assertMatchesGolden(t,
			filepath.Join(golden, entry.Name()),
			filepath.Join(outDir, entry.Name()))
	}
	produced, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(produced) != len(entries) {
		t.Errorf("produced %d files, real Ansible produced %d", len(produced), len(entries))
	}
}

func assertMatchesGolden(t *testing.T, goldenPath, gotPath string) {
	t.Helper()
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("golden %s: %v", goldenPath, err)
	}
	got, err := os.ReadFile(gotPath)
	if err != nil {
		t.Errorf("%s: %v", filepath.Base(goldenPath), err)
		return
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from real ansible-core\n--- real ---\n%s\n--- this engine ---\n%s",
			filepath.Base(goldenPath), want, got)
	}
}

// TestConformanceRecap pins the PLAY RECAP line and the exit-code
// question against real ansible-core 2.21.4, measured for a play that
// has one of each outcome:
//
//	h1 : ok=4 changed=1 unreachable=0 failed=0 skipped=1 rescued=1 ignored=1
//
// The counting is not obvious and was wrong here in three ways: a
// changed task also counts under ok, an IGNORED failure counts under ok
// AND ignored (not failed), and a RESCUED one counts under rescued (not
// failed). So a run whose only failures were ignored or rescued reports
// failed=0 — and exits 0, which this port did not.
func TestConformanceRecap(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: ok task
      debug: {msg: fine}
    - name: changed task
      command: echo hi
    - name: skipped task
      debug: {msg: nope}
      when: false
    - name: ignored failure
      command: /bin/false
      ignore_errors: true
    - name: rescued failure
      block:
        - command: /bin/false
      rescue:
        - debug: {msg: rescued}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}

	got := rr.Summary()["localhost"]
	want := &HostSummary{Ok: 4, Changed: 1, Unreachable: 0, Failed: 0, Skipped: 1, Rescued: 1, Ignored: 1}
	if *got != *want {
		t.Errorf("summary = %+v, want %+v (real ansible-core 2.21.4)", *got, *want)
	}

	// The exit-code question: nothing here failed outright.
	if rr.Failed() {
		t.Error("Failed() = true, want false — every failure was ignored or rescued")
	}

	var buf bytes.Buffer
	NewDefaultCallback(&buf, false).OnStats(rr)
	wantLine := "localhost                : ok=4    changed=1    unreachable=0    failed=0    skipped=1    rescued=1    ignored=1   \n"
	if !strings.HasSuffix(buf.String(), wantLine) {
		t.Errorf("recap line =\n%q\nwant it to end with\n%q", buf.String(), wantLine)
	}
}

// TestConformanceRecapRealFailureStillFails guards the other direction:
// making ignored and rescued failures stop counting must not make a
// genuine failure stop counting too.
func TestConformanceRecapRealFailureStillFails(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - command: /bin/false
`))
	if err != nil {
		t.Fatal(err)
	}
	rr, err := New(localhostInventory()).RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if !rr.Failed() {
		t.Error("Failed() = false for a genuine failure, want true")
	}
	if got := rr.Summary()["localhost"].Failed; got != 1 {
		t.Errorf("failed = %d, want 1", got)
	}
}

// TestConformanceDebugAndBanners pins four display behaviours measured
// against real ansible-core 2.21.4, each of which this port got wrong:
// a debug task printed nothing at all, `debug: {var: X}` reported the
// NAME of the variable rather than its value, an unnamed play bannered
// as bare "PLAY", and an unnamed task got no banner at all.
func TestConformanceDebugAndBanners(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  vars:
    d: {a: 1}
  tasks:
    - name: debug msg
      debug: {msg: hello}
    - name: debug var
      debug: {var: d}
    - debug: {msg: unnamed}
`))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	e := New(localhostInventory())
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	for _, want := range []string{
		// An unnamed play is named after its hosts pattern.
		"\nPLAY [all]\n",
		// A debug task dumps its result, pretty-printed at four spaces.
		"\nTASK [debug msg]\nok: [localhost] => {\n    \"msg\": \"hello\"\n}\n",
		// var: reports the variable's VALUE, keyed by its own name, and
		// sets no msg.
		"\nTASK [debug var]\nok: [localhost] => {\n    \"d\": {\n        \"a\": 1\n    }\n}\n",
		// An unnamed task banners under its module.
		"\nTASK [debug]\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestConformanceIgnoringLine covers the line real Ansible prints after a
// failure ignore_errors swallowed, so a red line that did not stop the
// run is not mistaken for one that did.
func TestConformanceIgnoringLine(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: ignored
      command: /bin/false
      ignore_errors: true
    - name: plain
      command: echo ok
`))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	e := New(localhostInventory())
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "...ignoring\n") {
		t.Errorf("output has no ...ignoring line:\n%s", got)
	}
	// Only the ignored failure gets one.
	if n := strings.Count(got, "...ignoring"); n != 1 {
		t.Errorf("...ignoring appears %d times, want 1", n)
	}
}

// TestConformanceFatalLine pins the shape of a failure line against real
// ansible-core 2.21.4:
//
//	fatal: [h1]: FAILED! => {"changed": true, "cmd": "...", "rc": 3, ...}
//
// This port printed only "failed: [h] => msg", so a failing shell task
// showed a return code and nothing else — not its stderr, not its
// command. The whole result is inlined instead, which is how a reader
// sees WHY it failed.
func TestConformanceFatalLine(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: fails loudly
      shell: echo to-stdout; echo to-stderr >&2; exit 3
`))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	e := New(localhostInventory())
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	if !strings.Contains(got, "fatal: [localhost]: FAILED! => {") {
		t.Fatalf("no fatal line:\n%s", got)
	}
	// Everything a reader needs to see why it failed, in real Ansible's
	// own JSON spelling — note ", " and ": ", and > left unescaped.
	for _, want := range []string{
		`"changed": true`,
		`"msg": "The command exited with a non-zero return code."`,
		`"rc": 3`,
		`"stderr": "to-stderr"`,
		`"stderr_lines": ["to-stderr"]`,
		`"stdout": "to-stdout"`,
		`"cmd": "echo to-stdout; echo to-stderr >&2; exit 3"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fatal line missing %s\n--- got ---\n%s", want, got)
		}
	}
}

// TestConformanceUnreachableLine covers the other prefix real Ansible
// uses, measured against a host whose connection is refused:
//
//	fatal: [nope]: UNREACHABLE! => {..., "unreachable": true}
func TestConformanceUnreachableLine(t *testing.T) {
	var buf bytes.Buffer
	cb := NewDefaultCallback(&buf, false)
	cb.OnTaskResult(Result{
		Host: "nope", Task: "t", Failed: true, Unreachable: true,
		Msg: "Failed to connect to the host via ssh",
	})
	got := buf.String()
	if !strings.Contains(got, "fatal: [nope]: UNREACHABLE! => {") {
		t.Errorf("unreachable line = %q", got)
	}
	if !strings.Contains(got, `"unreachable": true`) {
		t.Errorf("unreachable line has no unreachable field: %q", got)
	}
}
