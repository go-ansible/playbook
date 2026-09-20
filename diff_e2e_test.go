package playbook

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiffModeEndToEnd runs a playbook under --diff --check and compares
// what DefaultCallback prints against a transcript measured from real
// ansible-core 2.21.4 running the same playbook (testdata/diff_e2e_expected.txt).
//
// The comparison covers the task banners, the diff blocks and the result
// lines, but not the banner padding: real Ansible pads every banner to a
// fixed width with asterisks and this port does not — a pre-existing
// divergence unrelated to --diff, so the banners are compared by their
// text alone rather than quietly asserting output this port does not
// produce.
func TestDiffModeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}

	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: copy new file
      copy: {content: "one\ntwo\n", dest: ` + filepath.Join(dir, "out/new.txt") + `}
    - name: edit existing file
      lineinfile: {path: ` + filepath.Join(dir, "seed.txt") + `, line: gamma}
    - name: replace in file
      replace: {path: ` + filepath.Join(dir, "seed.txt") + `, regexp: alpha, replace: ALPHA}
    - name: unchanged copy
      copy: {content: "alpha\nbeta\n", dest: ` + filepath.Join(dir, "seed.txt") + `}
`))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	e := New(localhostInventory())
	e.DiffMode = true
	e.CheckMode = true
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}

	want, err := os.ReadFile("testdata/diff_e2e_expected.txt")
	if err != nil {
		t.Fatal(err)
	}
	wantText := strings.ReplaceAll(string(want), "{{DIR}}", dir)
	// The engine reports on "localhost"; the measured run used an
	// inventory host named h1. Only the name differs.
	wantText = strings.ReplaceAll(wantText, "[h1]", "[localhost]")

	got := comparableTranscript(buf.String())
	if got != strings.TrimRight(wantText, "\n") {
		t.Errorf("transcript diverges from real Ansible.\n--- got ---\n%s\n--- want ---\n%s", got, wantText)
	}

	// The point of --check alongside it: nothing was written.
	if _, err := os.Stat(filepath.Join(dir, "out/new.txt")); !os.IsNotExist(err) {
		t.Error("check mode created the file the diff described")
	}
	seed, err := os.ReadFile(filepath.Join(dir, "seed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(seed) != "alpha\nbeta\n" {
		t.Errorf("check mode modified the seed file: %q", seed)
	}
}

// comparableTranscript drops the PLAY and PLAY RECAP banners and strips
// the asterisk padding this port does not emit, leaving the task
// banners, diff blocks and result lines.
func comparableTranscript(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(line, "PLAY"):
			continue
		case strings.HasPrefix(line, "localhost  "):
			continue // the recap row
		}
		if len(out) == 0 && strings.TrimSpace(line) == "" {
			continue // leading blank before the first banner
		}
		if strings.HasPrefix(line, "TASK [") {
			// Drop the blank line that precedes every banner, so the
			// transcript reads as one block per task.
			if n := len(out); n > 0 && out[n-1] == "" {
				out = out[:n-1]
			}
			line = strings.TrimRight(strings.TrimRight(line, "*"), " ")
		}
		out = append(out, line)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}
