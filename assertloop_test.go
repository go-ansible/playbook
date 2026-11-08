package playbook

import (
	"strings"
	"testing"
)

// go-ansible/playbook#196: `item` was undefined inside an assert's `that:`
// when the task had a loop, so an assertion ABOUT the item could not be
// written at all.
//
// The cause was runAssert resolving its own vars from the host's context
// (`ec.engine.resolvedFor(st.vc)`) instead of using the vars the task was
// resolved with. A loop runs each iteration in a per-item scope, and
// reaching past it skips exactly the variable the loop exists to provide.

func TestAssertCanReferenceItemInsideALoop(t *testing.T) {
	out := runTasks(t, ""+
		"    - assert:\n        that: \"item > 0\"\n      loop: [1, 2, 3]\n")
	if len(out) != 3 {
		t.Fatalf("got %d results, want one per item: %+v", len(out), out)
	}
	for i, r := range out {
		if r.Failed {
			t.Errorf("item %d failed: %q", i+1, r.Msg)
		}
	}
}

// The other direction, so the test above cannot pass on an assertion that
// silently evaluated to true: an item that violates the condition must FAIL,
// and must name the condition real reports under `assertion`.
func TestAssertFailsOnTheItemThatViolatesIt(t *testing.T) {
	out := runTasks(t, ""+
		"    - assert:\n        that: \"item > 1\"\n      loop: [2, 0]\n      ignore_errors: true\n")
	if len(out) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(out), out)
	}
	if out[0].Failed {
		t.Errorf("item 2 should pass `item > 1`: %q", out[0].Msg)
	}
	if !out[1].Failed {
		t.Fatal("item 0 should fail `item > 1`")
	}
	if got := out[1].Extra["assertion"]; got != "item > 1" {
		t.Errorf("assertion = %#v, want the condition real reports", got)
	}
	// The failure must be the ASSERTION, not an undefined-variable error —
	// which is how this looked before the fix.
	if strings.Contains(out[1].Msg, "undefined") {
		t.Errorf("msg = %q; `item` must be defined, so a failure is the assertion's", out[1].Msg)
	}
}

// loop_control: loop_var renames the variable, and `that:` must see the new
// name — proof the fix passes the task's own resolved vars rather than
// special-casing the word "item".
func TestAssertSeesARenamedLoopVar(t *testing.T) {
	out := runTasks(t, ""+
		"    - assert:\n        that: \"thing > 0\"\n      loop: [1, 2]\n"+
		"      loop_control:\n        loop_var: thing\n")
	if len(out) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(out), out)
	}
	for i, r := range out {
		if r.Failed {
			t.Errorf("iteration %d failed: %q", i, r.Msg)
		}
	}
}

// Control: an assert with no loop still sees ordinary play vars, so the
// change cannot have swapped one scope for a narrower one.
func TestAssertStillSeesPlayVarsWithoutALoop(t *testing.T) {
	out := runTasks(t, ""+
		"    - set_fact:\n        threshold: 5\n"+
		"    - assert:\n        that: \"threshold == 5\"\n")
	if len(out) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(out), out)
	}
	if out[1].Failed {
		t.Errorf("assert failed on a play var: %q", out[1].Msg)
	}
}
