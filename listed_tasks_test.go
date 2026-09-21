package playbook

import (
	"strings"
	"testing"
)

// TestListedTasksMatchesRealAnsible pins which tasks --list-tasks shows.
// Measured against real ansible-core 2.21.4: a block's contents are
// listed, its rescue: and always: are NOT, and role tasks appear.
func TestListedTasksMatchesRealAnsible(t *testing.T) {
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - {name: before, debug: {msg: x}}
    - name: the block
      block:
        - {name: in-block, debug: {msg: x}}
        - name: nested
          block:
            - {name: deep, debug: {msg: x}}
      rescue:
        - {name: in-rescue, debug: {msg: x}}
      always:
        - {name: in-always, debug: {msg: x}}
      tags: [bt]
    - {name: after, debug: {msg: x}}
`))
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, task := range pb[0].ListedTasks() {
		names = append(names, task.Name)
	}
	want := "before,in-block,deep,after"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("ListedTasks = %q, real ansible-core lists %q", got, want)
	}

	// The tags pushed down from the block are on the listed tasks.
	for _, task := range pb[0].ListedTasks() {
		if task.Name == "in-block" || task.Name == "deep" {
			if !contains(task.Tags, "bt") {
				t.Errorf("%s lost its block's tag: %v", task.Name, task.Tags)
			}
		}
	}
}
