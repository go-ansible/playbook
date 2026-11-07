package playbook

import (
	"testing"
)

// go-ansible/playbook#190: a play using both `pre_tasks:` and `roles:` ran
// the roles FIRST. The parser appended the role tasks and then looped
// pre_tasks/tasks/post_tasks, so the one thing pre_tasks exists for --
// preparing a host for the roles that follow -- happened afterwards.
//
// Real's order, from Play.compile() (ansible-core 2.21.4,
// playbook/play.py:352-358):
//
//	block_list.extend(self.pre_tasks)
//	block_list.append(flush_block)
//	block_list.extend(self._compile_roles())
//	block_list.extend(self.tasks)
//	block_list.append(flush_block)
//	block_list.extend(self.post_tasks)
//	block_list.append(flush_block)
func TestPreTasksRunBeforeRoles(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/myrole/tasks/main.yml", "- command: role\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  gather_facts: false
  pre_tasks:
    - command: pre
  roles:
    - myrole
  tasks:
    - command: main
  post_tasks:
    - command: post
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pre", "role: myrole", "main", "post"}
	got := playLabels(pb[0].Tasks)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("task %d = %q, want %q (order is pre_tasks, roles, tasks, post_tasks)", i, got[i], want[i])
		}
	}
	// The role is a container; its own task must still be inside it, so
	// the assertion above cannot pass on an empty role.
	if role := pb[0].Tasks[1]; len(role.Block) != 1 || role.Block[0].Args["_raw_params"] != "role" {
		t.Errorf("role block = %+v, want the role's own one task", role.Block)
	}
}

// playLabels names each top-level task the way the parser shapes them: a
// role becomes ONE container task called "role: <name>" holding the role's
// own tasks in Block, while a plain task carries its command in
// Args["_raw_params"]. A first version of this test asserted _raw_params on
// the role and read nil, which says nothing about ORDER -- the thing under
// test.
func playLabels(tasks []Task) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if raw, ok := t.Args["_raw_params"].(string); ok && raw != "" {
			out = append(out, raw)
			continue
		}
		out = append(out, t.Name)
	}
	return out
}

// The same order with no roles at all, so the fix cannot have been achieved
// by dropping the roles on the floor.
func TestPreTasksOrderWithoutRoles(t *testing.T) {
	pb, err := Parse([]byte("" +
		"- hosts: all\n  gather_facts: false\n" +
		"  post_tasks:\n    - command: post\n" +
		"  pre_tasks:\n    - command: pre\n" +
		"  tasks:\n    - command: main\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pre", "main", "post"}
	if len(pb[0].Tasks) != len(want) {
		t.Fatalf("got %d tasks, want %d", len(pb[0].Tasks), len(want))
	}
	for i, w := range want {
		if got := pb[0].Tasks[i].Args["_raw_params"]; got != w {
			t.Errorf("task %d = %v, want %q — the YAML order of the keys must not matter", i, got, w)
		}
	}
}

// A play with roles and NO pre_tasks must be unchanged: the role tasks come
// first because there is nothing before them, not because of the old bug.
func TestRolesComeFirstWhenThereAreNoPreTasks(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/myrole/tasks/main.yml", "- command: role\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  gather_facts: false
  roles:
    - myrole
  tasks:
    - command: main
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"role: myrole", "main"}
	got := playLabels(pb[0].Tasks)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("task %d = %q, want %q", i, got[i], want[i])
		}
	}
}
