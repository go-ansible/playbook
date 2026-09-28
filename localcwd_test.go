package playbook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-ansible/inventory"
)

// TestALocalModuleRunsInThePlaybookDirectory pins where a local module
// runs. Measured against ansible-core 2.21.4 from three different
// working directories, inside a role and out, with and without an
// explicit connection: keyword -- always the PLAYBOOK's own directory.
//
// This port used the process's working directory, so a playbook doing
//
//	read_csv: {path: files/data.csv}
//
// found the file or did not depending on where ansible-playbook had
// been run from. The test proves the property the way the transport's
// own does: by reading a file by a RELATIVE name, not by comparing
// path spellings.
func TestALocalModuleRunsInThePlaybookDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("here"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "site.yml")
	if err := os.WriteFile(path, []byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: read it by a relative name
      command: cat marker.txt
      register: r
    - name: report
      debug: msg="{{ r.stdout }}"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Deliberately run from somewhere ELSE, which is the whole point.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	pb, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := inventory.ParseYAML([]byte("all:\n  hosts:\n    localhost:\n      ansible_connection: local\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := New(inv)
	e.BaseDir = dir
	var reported string
	e.OnResult = func(r Result) {
		if r.Task == "report" {
			reported = r.Msg
		}
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	if strings.TrimSpace(reported) != "here" {
		t.Errorf("read %q, want %q -- a local module must run in the playbook's directory, not the process's", reported, "here")
	}
}
