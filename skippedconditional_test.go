package playbook

import (
	"context"
	"testing"

	"github.com/go-ansible/modules"
)

// Real evaluates changed_when/failed_when only when the task was not
// skipped:
//
//	# if we didn't skip this task, use the helpers to evaluate the
//	# changed/failed_when properties
//	if not utr.skipped:
//
// (ansible-core 2.21.4, executor/task_executor.py:589-591.) This engine
// evaluated them regardless, so a task the check-mode gate declined to run
// came back CHANGED on `changed_when: true`, or FAILED on
// `failed_when: true`, for work that never happened.
//
// The fixture needs a module with genuinely no check-mode support, and the
// support list grows: asserting that premise is what keeps this test from
// silently stopping to test anything, the same guard conformance_test.go
// uses for its own fixture.
func TestASkippedTaskDoesNotEvaluateItsConditionals(t *testing.T) {
	if modules.SupportsCheckMode(unportedModule) {
		t.Fatalf("%s now supports check mode; pick another module for this fixture", unportedModule)
	}
	dir := t.TempDir()

	for _, tc := range []struct {
		name, keyword string
	}{
		{"changed_when", "      changed_when: true\n"},
		{"failed_when", "      failed_when: true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pb, err := Parse([]byte("" +
				"- hosts: all\n  gather_facts: false\n  tasks:\n" +
				"    - name: skipped\n" +
				"      " + unportedModule + ": {src: " + dir + ", dest: " + dir + "/out}\n" +
				tc.keyword))
			if err != nil {
				t.Fatal(err)
			}
			e := New(localhostInventory())
			e.CheckMode = true
			var got Result
			e.OnResult = func(r Result) { got = r }
			if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
				t.Fatal(err)
			}
			if !got.Skipped {
				t.Errorf("skipped = false; the check-mode gate declined to run %s", unportedModule)
			}
			if got.Changed {
				t.Error("changed = true on a task that never ran")
			}
			if got.Failed {
				t.Errorf("failed = true on a task that never ran: %q", got.Msg)
			}
			// Real records no result for a conditional it never evaluated.
			if _, ok := got.Extra[tc.name+"_result"]; ok {
				t.Errorf("%s_result = %#v is present; the condition was not evaluated, so there is nothing to report",
					tc.name, got.Extra[tc.name+"_result"])
			}
		})
	}
}

// Control: the gate is about being SKIPPED, not about check mode. A module
// that DOES support a dry run still runs, so its conditionals still apply —
// otherwise the fix above would have silenced changed_when for every
// --check run.
func TestACheckModeTaskThatStillRunsKeepsItsConditionals(t *testing.T) {
	pb, err := Parse([]byte("" +
		"- hosts: all\n  gather_facts: false\n  tasks:\n" +
		"    - name: runs\n      debug: {msg: hi}\n      changed_when: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !modules.SupportsCheckMode("debug") {
		t.Skip("debug no longer supports check mode; this control needs a module that runs under --check")
	}
	e := New(localhostInventory())
	e.CheckMode = true
	var got Result
	e.OnResult = func(r Result) { got = r }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if got.Skipped {
		t.Fatal("debug was skipped under --check; it supports a dry run")
	}
	if !got.Changed {
		t.Error("changed = false; changed_when: true must still apply to a task that ran")
	}
}
