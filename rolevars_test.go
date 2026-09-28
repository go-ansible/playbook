package playbook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRole lays down a role with a default, a role var and a task that
// reports both, so precedence is observable.
func writeRole(t *testing.T, dir, name string) {
	t.Helper()
	for _, sub := range []string{"tasks", "defaults", "vars"} {
		if err := os.MkdirAll(filepath.Join(dir, "roles", name, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(dir, "roles", name, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tasks/main.yml", "- debug: msg=\"who={{ who }}\"\n")
	write("defaults/main.yml", "who: default\n")
	write("vars/main.yml", "who: rolevars\n")
}

func runPlaybookFile(t *testing.T, dir, src string) ([]Result, error) {
	t.Helper()
	path := filepath.Join(dir, "site.yml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pb, err := ParseFile(path)
	if err != nil {
		return nil, err
	}
	var out []Result
	e := New(localhostInventory())
	e.OnResult = func(r Result) { out = append(out, r) }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

// TestTaskVarsReachTheRole pins what this port dropped: the task's own
// vars: beside include_role:/import_role: reach the role, ABOVE both
// its vars/main.yml and its defaults. Measured against ansible-core
// 2.21.4 with who: in all three places -- the task's wins.
func TestTaskVarsReachTheRole(t *testing.T) {
	for _, key := range []string{"include_role", "import_role"} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			writeRole(t, dir, "prec")
			rs, err := runPlaybookFile(t, dir, `
- hosts: all
  gather_facts: false
  tasks:
    - name: with task vars
      `+key+`: {name: prec}
      vars: {who: taskvars}
`)
			if err != nil {
				t.Fatal(err)
			}
			var msgs []string
			for _, r := range rs {
				if r.Msg != "" {
					msgs = append(msgs, r.Msg)
				}
			}
			if len(msgs) != 1 || msgs[0] != "who=taskvars" {
				t.Errorf("got %v, real gives [who=taskvars] -- the task's own vars beat the role's vars/main.yml and defaults", msgs)
			}
		})
	}

	// Without them, the role's own vars/main.yml wins over defaults.
	dir := t.TempDir()
	writeRole(t, dir, "prec")
	rs, err := runPlaybookFile(t, dir, `
- hosts: all
  gather_facts: false
  tasks:
    - include_role: {name: prec}
`)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rs {
		if r.Msg == "who=rolevars" {
			found = true
		}
	}
	if !found {
		t.Error("without task vars, role vars/main.yml must win over defaults")
	}
}

// TestVarsInsideTheMappingIsRefused pins a refusal this port did not
// have -- and, worse, a form it HONOURED. Measured:
//
//	[ERROR]: Invalid options for include_role: vars
func TestVarsInsideTheMappingIsRefused(t *testing.T) {
	for _, key := range []string{"include_role", "import_role"} {
		dir := t.TempDir()
		writeRole(t, dir, "prec")
		_, err := runPlaybookFile(t, dir, `
- hosts: all
  gather_facts: false
  tasks:
    - `+key+`: {name: prec, vars: {who: innervars}}
`)
		if err == nil {
			t.Errorf("%s with an inner vars: was accepted; real refuses it", key)
			continue
		}
		if want := "Invalid options for " + key + ": vars"; !strings.Contains(err.Error(), want) {
			t.Errorf("%s:\n got %v\nwant a message containing %q", key, err, want)
		}
	}

	// Several bad options are listed together, comma-separated.
	dir := t.TempDir()
	writeRole(t, dir, "prec")
	_, err := runPlaybookFile(t, dir, `
- hosts: all
  gather_facts: false
  tasks:
    - include_role: {name: prec, vars: {a: 1}, nonsense: 2}
`)
	if err == nil {
		t.Fatal("two bad options were accepted")
	}
	if want := "Invalid options for include_role: nonsense,vars"; !strings.Contains(err.Error(), want) {
		t.Errorf("got %v, want a message containing %q (sorted, since a Go map has no order)", err, want)
	}

	// And the options real DOES accept are still accepted.
	dir = t.TempDir()
	writeRole(t, dir, "prec")
	if _, err := runPlaybookFile(t, dir, `
- hosts: all
  gather_facts: false
  tasks:
    - include_role: {name: prec, tasks_from: main, public: true}
`); err != nil {
		t.Errorf("a valid option set was refused: %v", err)
	}
}

// TestIncludeRoleAnnouncesItself pins the banner: a DYNAMIC include
// announces "included: <role> for <host>" and counts toward ok, while
// a static import_role announces nothing.
func TestIncludeRoleAnnouncesItself(t *testing.T) {
	dir := t.TempDir()
	writeRole(t, dir, "prec")
	rs, err := runPlaybookFile(t, dir, `
- hosts: all
  gather_facts: false
  tasks:
    - name: the include
      include_role: {name: prec}
    - name: the import
      import_role: {name: prec}
`)
	if err != nil {
		t.Fatal(err)
	}
	var announced []string
	for _, r := range rs {
		if r.Included != "" {
			announced = append(announced, r.Task+"/"+r.Included)
		}
	}
	if len(announced) != 1 || announced[0] != "the include/prec" {
		t.Errorf("announcements = %v, real announces the include only, naming the ROLE", announced)
	}
}
