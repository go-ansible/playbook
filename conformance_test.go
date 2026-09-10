package playbook

import (
	"context"
	"os"
	"path/filepath"
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
