package playbook

import (
	"context"
	"strings"
	"testing"
)

// A task scope must not outlive its task: `item` from a loop, and a
// task's own `vars:`, are both gone by the next task. Measured against
// real ansible-core 2.21.4 with this exact playbook -- it prints
// UNDEFINED for both, and so does this port.
//
// What actually scopes them is NOT Context.Child(). runTaskOnHost calls
// scope.Set(vars.TaskVars, task.Vars) for EVERY task, and vars.Set
// replaces the whole layer with a fresh map -- so a later task that has
// no `vars:` of its own passes nil and empties TaskVars, taking `item`
// with it.
//
// That was worth establishing rather than assuming, and the way to
// establish it was to break Child(): neutering it to return its parent
// instead of a copy leaves THIS test green, and playbook's entire suite
// green with it (24.7s of it), while vars' own TestChildIsolation goes
// red immediately. The engine's two Child() calls are therefore
// defensive -- no behaviour asserted anywhere depends on their copying.
// They are kept: removing defensive copying because nothing currently
// observes it is how the next defect gets in.
//
// So this test pins the observable parity, which nothing asserted
// before, and it is honest about not being a guard for Child().
func TestATaskScopeDoesNotOutliveItsTask(t *testing.T) {
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - name: a looping task
      debug: {msg: "loop sees {{ item }}"}
      loop: [a, b]
    - name: after the loop
      debug: {msg: "AFTER-LOOP item=>{{ item | default('UNDEFINED') }}<"}
    - name: a task with its own vars
      debug: {msg: "scoped sees {{ scoped }}"}
      vars: {scoped: i-am-task-scoped}
    - name: after the scoped task
      debug: {msg: "AFTER-VARS scoped=>{{ scoped | default('UNDEFINED') }}<"}
`))
	if err != nil {
		t.Fatal(err)
	}

	var msgs []string
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if r.Msg != "" {
			msgs = append(msgs, r.Msg)
		}
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(msgs, "\n")

	// The loop must actually have run, or the leak checks below are
	// vacuous: nothing would have been in scope to leak.
	for _, want := range []string{"loop sees a", "loop sees b", "scoped sees i-am-task-scoped"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the scoped values were never in scope at all (missing %q), so this test "+
				"cannot say anything about them leaking; got:\n%s", want, joined)
		}
	}

	for _, c := range []struct{ what, want string }{
		{"a loop's item", "AFTER-LOOP item=>UNDEFINED<"},
		{"a task's own vars", "AFTER-VARS scoped=>UNDEFINED<"},
	} {
		if !strings.Contains(joined, c.want) {
			t.Errorf("%s outlived its task: wanted %q in\n%s", c.what, c.want, joined)
		}
	}
}
