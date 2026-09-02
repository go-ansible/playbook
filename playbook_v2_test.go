package playbook

import (
	"os"
	"path/filepath"
	"testing"
)

func writePlaybookFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseImportPlaybookSplicesPlays(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "sub.yml", `
- hosts: web
  tasks: []
- hosts: db
  tasks: []
`)
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  tasks: []
- import_playbook: sub.yml
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(pb) != 3 {
		t.Fatalf("Playbook has %d plays, want 3 (1 own + 2 imported)", len(pb))
	}
	if pb[1].Hosts != "web" || pb[2].Hosts != "db" {
		t.Fatalf("imported plays = %q, %q, want web, db", pb[1].Hosts, pb[2].Hosts)
	}
}

func TestParseImportPlaybookRelativeToOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	// sub.yml lives in a subdirectory and itself imports a role — that
	// role must resolve relative to sub.yml's own directory, not
	// site.yml's, exactly like import_tasks/roles already do.
	writePlaybookFile(t, dir, "subdir/roles/r1/tasks/main.yml", "- debug: {}\n")
	writePlaybookFile(t, dir, "subdir/sub.yml", `
- hosts: all
  roles:
    - r1
`)
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- import_playbook: subdir/sub.yml
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(pb) != 1 || !pb[0].Tasks[0].IsBlock() {
		t.Fatalf("imported play = %+v", pb)
	}
}

func TestParseImportPlaybookMissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- import_playbook: nope.yml
`)
	if _, err := ParseFile(pbPath); err == nil {
		t.Fatal("want an error for a missing import_playbook target")
	}
}

func TestParseStrategyRejectsNonLinear(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  strategy: free
  tasks: []
`))
	if err == nil {
		t.Fatal("want an error for an unsupported strategy")
	}
}

func TestParseStrategyLinearAccepted(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  strategy: linear
  tasks: []
`))
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseVarsFilesLoadsAndInlineVarsWin(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "vars/extra.yml", "a: from_file\nb: from_file\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  vars_files:
    - vars/extra.yml
  vars:
    b: from_inline
  tasks: []
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	if pb[0].Vars["a"] != "from_file" {
		t.Fatalf("a = %v, want value loaded from vars_files", pb[0].Vars["a"])
	}
	if pb[0].Vars["b"] != "from_inline" {
		t.Fatalf("b = %v, want inline vars: to win over vars_files", pb[0].Vars["b"])
	}
}

func TestParseVarsFilesMissingErrors(t *testing.T) {
	dir := t.TempDir()
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  vars_files:
    - does/not/exist.yml
  tasks: []
`)
	if _, err := ParseFile(pbPath); err == nil {
		t.Fatal("want an error for a missing vars_files entry")
	}
}

func TestParseRolesLoadsTasksDefaultsVars(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/myrole/tasks/main.yml", `
- name: use role vars
  debug:
    msg: "{{ from_defaults }} {{ from_vars }}"
`)
	writePlaybookFile(t, dir, "roles/myrole/defaults/main.yml", "from_defaults: def_value\n")
	writePlaybookFile(t, dir, "roles/myrole/vars/main.yml", "from_vars: var_value\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  roles:
    - myrole
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(pb[0].Tasks) != 1 || !pb[0].Tasks[0].IsBlock() {
		t.Fatalf("expected one synthetic role block task, got %+v", pb[0].Tasks)
	}
	roleTask := pb[0].Tasks[0]
	if roleTask.RoleDefaults["from_defaults"] != "def_value" {
		t.Fatalf("RoleDefaults = %v", roleTask.RoleDefaults)
	}
	if roleTask.RoleVars["from_vars"] != "var_value" {
		t.Fatalf("RoleVars = %v", roleTask.RoleVars)
	}
	if len(roleTask.Block) != 1 || roleTask.Block[0].Module != "debug" {
		t.Fatalf("role tasks not spliced in: %+v", roleTask.Block)
	}
}

func TestParseRoleMissingDirectoryErrors(t *testing.T) {
	dir := t.TempDir()
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  roles:
    - nosuchrole
`)
	if _, err := ParseFile(pbPath); err == nil {
		t.Fatal("want an error for a role with no roles/<name> directory")
	}
}

func TestParseRoleWithMapFormAndExtraVars(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/myrole/tasks/main.yml", "- debug: {}\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  roles:
    - role: myrole
      extra_param: extra_value
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	if pb[0].Tasks[0].RoleVars["extra_param"] != "extra_value" {
		t.Fatalf("RoleVars = %v", pb[0].Tasks[0].RoleVars)
	}
}

func TestParseRoleHandlersFoldIntoPlayHandlers(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/myrole/tasks/main.yml", "- debug: {}\n")
	writePlaybookFile(t, dir, "roles/myrole/handlers/main.yml", `
- name: restart thing
  debug:
    msg: restarted
`)
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  roles:
    - myrole
  handlers:
    - name: own handler
      debug: {}
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(pb[0].Handlers) != 2 {
		t.Fatalf("Handlers = %+v, want role handler folded in ahead of the play's own", pb[0].Handlers)
	}
	if pb[0].Handlers[0].Name != "restart thing" {
		t.Fatalf("Handlers[0] = %q, want the role's handler first", pb[0].Handlers[0].Name)
	}
}

func TestParseIncludeTasksSplicesFile(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "included.yml", `
- name: included task
  debug: {}
`)
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  tasks:
    - name: the include
      include_tasks: included.yml
      when: some_cond
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	task := pb[0].Tasks[0]
	if !task.IsBlock() || len(task.Block) != 1 || task.Block[0].Name != "included task" {
		t.Fatalf("include_tasks not spliced: %+v", task)
	}
	if task.When == "" {
		t.Fatal("the include's own when should carry onto the synthetic block")
	}
}

func TestParseImportTasksSplicesFile(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "included.yml", "- debug: {}\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  tasks:
    - import_tasks: included.yml
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !pb[0].Tasks[0].IsBlock() || len(pb[0].Tasks[0].Block) != 1 {
		t.Fatalf("import_tasks not spliced: %+v", pb[0].Tasks[0])
	}
}

func TestParseIncludeTasksMissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  tasks:
    - include_tasks: nope.yml
`)
	if _, err := ParseFile(pbPath); err == nil {
		t.Fatal("want an error for a missing include_tasks file")
	}
}

func TestParseIncludeRoleTaskLevel(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/myrole/tasks/main.yml", "- debug: {}\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  tasks:
    - name: bring in the role
      include_role:
        name: myrole
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	task := pb[0].Tasks[0]
	if !task.IsBlock() || len(task.Block) != 1 {
		t.Fatalf("include_role not spliced: %+v", task)
	}
	if task.Name != "bring in the role" {
		t.Fatalf("Name = %q, want the include task's own name preserved", task.Name)
	}
}

func TestParseTagsPropagateFromPlayAndBlock(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tags: [playtag]
  tasks:
    - block:
        - name: leaf
          debug: {}
      tags: [blocktag]
`))
	if err != nil {
		t.Fatal(err)
	}
	leaf := pb[0].Tasks[0].Block[0]
	has := func(tag string) bool {
		for _, t := range leaf.Tags {
			if t == tag {
				return true
			}
		}
		return false
	}
	if !has("playtag") || !has("blocktag") {
		t.Fatalf("leaf.Tags = %v, want both playtag and blocktag inherited", leaf.Tags)
	}
}

func TestParseMetaAndAddHostAreOrdinaryModuleTasks(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tasks:
    - meta: flush_handlers
    - add_host:
        name: newhost
`)) // sanity: these parse as plain module tasks (engine special-cases them at run time)
	if err != nil {
		t.Fatal(err)
	}
	if pb[0].Tasks[0].Module != "meta" || pb[0].Tasks[1].Module != "add_host" {
		t.Fatalf("Tasks = %+v", pb[0].Tasks)
	}
}
