package playbook

import (
	"testing"
)

// go-ansible/playbook#194: `failed_when`/`changed_when` could not reference
// the task's OWN register. The result was bound only as `result`, so the
// documented form -- naming the register -- evaluated against an undefined
// variable.
//
// Real applies its REGISTER_VARS layer to task_vars before resolving either
// conditional (ansible-core 2.21.4, executor/task_executor.py:582-583, then
// :593 and :599), so the name a later task reads is the name these see.

func TestFailedWhenCanReferenceItsOwnRegister(t *testing.T) {
	// `false` exits 1. Naming the register, the task is forgiven; the
	// module's own msg survives, as it does for a literal `failed_when:
	// false` (see failedwhen_test.go).
	out := runTasks(t, "    - command: \"false\"\n      register: out\n      failed_when: out.rc == 0\n")
	if len(out) != 1 {
		t.Fatalf("got %d results, want 1", len(out))
	}
	if out[0].Failed {
		t.Errorf("failed = true: the condition `out.rc == 0` was false, so the task is forgiven. msg = %q", out[0].Msg)
	}
	if got := out[0].Extra["failed_when_result"]; got != false {
		t.Errorf("failed_when_result = %#v, want false — the expression must have EVALUATED", got)
	}
}

func TestFailedWhenReferencingItsOwnRegisterCanFailTheTask(t *testing.T) {
	// The other direction, so the test above cannot pass on a condition
	// that merely errored into a falsy value: here the same reference must
	// make a SUCCEEDING command fail.
	out := runTasks(t, "    - command: \"true\"\n      register: out\n      failed_when: out.rc == 0\n      ignore_errors: true\n")
	if len(out) != 1 {
		t.Fatalf("got %d results, want 1", len(out))
	}
	if !out[0].Failed {
		t.Error("failed = false; `out.rc == 0` is true for `true`, so failed_when must fail the task")
	}
	if got := out[0].Extra["failed_when_result"]; got != true {
		t.Errorf("failed_when_result = %#v, want true", got)
	}
}

func TestChangedWhenCanReferenceItsOwnRegister(t *testing.T) {
	out := runTasks(t, "    - command: \"echo hello\"\n      register: out\n      changed_when: \"'hello' in out.stdout\"\n")
	if len(out) != 1 {
		t.Fatalf("got %d results, want 1", len(out))
	}
	if out[0].Failed {
		t.Fatalf("task failed: %q", out[0].Msg)
	}
	if !out[0].Changed {
		t.Error("changed = false; `'hello' in out.stdout` is true")
	}
	if got := out[0].Extra["changed_when_result"]; got != true {
		t.Errorf("changed_when_result = %#v, want true", got)
	}

	// Control, same shape: a condition that is FALSE must leave the task
	// unchanged. Without it, a `changed_when` that failed to evaluate and
	// was treated as true would look identical to one that worked.
	off := runTasks(t, "    - command: \"echo hello\"\n      register: out\n      changed_when: \"'goodbye' in out.stdout\"\n")
	if off[0].Changed {
		t.Error("changed = true on a false condition")
	}
	if got := off[0].Extra["changed_when_result"]; got != false {
		t.Errorf("changed_when_result = %#v, want false", got)
	}
}

// `result` keeps working: it is what this port bound before #194 and
// playbooks in the wild now depend on it. Real has no such name, so this is
// a deliberate extra rather than parity -- worth a test precisely because
// removing it would be the kind of silent break a fix can cause.
func TestTheResultNameStillWorksAlongsideTheRegister(t *testing.T) {
	out := runTasks(t, "    - command: \"false\"\n      register: out\n      failed_when: result.rc == 0\n")
	if out[0].Failed {
		t.Errorf("failed = true; `result.rc` must still resolve. msg = %q", out[0].Msg)
	}
}

// A register name that shadows an existing variable wins for the
// conditional, because real's register layer is applied over task_vars.
func TestTheRegisterShadowsAnEarlierVariableInTheConditional(t *testing.T) {
	out := runTasks(t, ""+
		"    - set_fact:\n        out: {rc: 99}\n"+
		"    - command: \"true\"\n      register: out\n      failed_when: out.rc == 99\n")
	if len(out) != 2 {
		t.Fatalf("got %d results, want 2", len(out))
	}
	if out[1].Failed {
		t.Errorf("failed = true: out.rc must be the COMMAND's 0, not the set_fact's 99. msg = %q", out[1].Msg)
	}
}

// With a retry loop real makes `attempts` available to the conditionals,
// and only then: "Make attempts and retries available early to allow their
// use in changed/failed_when; if retries > 1" (task_executor.py:585-587).
func TestAttemptsIsVisibleToTheConditionalsUnderARetryLoop(t *testing.T) {
	out := runTasks(t, ""+
		"    - command: \"true\"\n      register: out\n      retries: 2\n      delay: 0\n"+
		"      until: true\n      failed_when: out.attempts != 1\n")
	if len(out) != 1 {
		t.Fatalf("got %d results, want 1", len(out))
	}
	if out[0].Failed {
		t.Errorf("failed = true; attempts should be 1 on the first success. msg = %q", out[0].Msg)
	}
}
