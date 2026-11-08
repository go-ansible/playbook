package playbook

import (
	"testing"
)

// go-ansible/playbook#191: a host the block's own `when:` skipped was
// DROPPED FROM THE PLAY, so every task after the block never ran for it.
//
// The mechanism: originalActive was captured after the when filter had
// already removed those hosts, so the list runBlock returned could not
// contain them -- and the caller assigns that list back to `active`.
// With the condition false for every host, the play simply stopped.

func TestASkippedBlockDoesNotEndThePlay(t *testing.T) {
	out := runTasks(t, ""+
		"    - block:\n        - command: echo inside\n      when: false\n"+
		"    - command: echo after\n")
	if !ranCommand(out, "after") {
		t.Errorf("the task after a skipped block never ran: %s", labelResults(out))
	}
	if ranCommand(out, "inside") {
		t.Errorf("the block's own task ran despite when: false: %s", labelResults(out))
	}
}

// The same with a condition that is TRUE, so the test above cannot pass on a
// block that is simply never gated.
func TestATakenBlockRunsBothTheBlockAndWhatFollows(t *testing.T) {
	out := runTasks(t, ""+
		"    - block:\n        - command: echo inside\n      when: true\n"+
		"    - command: echo after\n")
	if !ranCommand(out, "inside") {
		t.Errorf("the block did not run under when: true: %s", labelResults(out))
	}
	if !ranCommand(out, "after") {
		t.Errorf("the task after the block did not run: %s", labelResults(out))
	}
}

// A block whose when cannot be EVALUATED is a failure, and a failed host does
// leave the play. The fix must not restore those hosts too -- that would turn
// a broken conditional into a silent skip.
func TestABlockWhoseWhenCannotEvaluateStillFailsTheHost(t *testing.T) {
	out := runTasks(t, ""+
		"    - block:\n        - command: echo inside\n      when: nosuchvar.member\n"+
		"    - command: echo after\n")
	if ranCommand(out, "after") {
		t.Errorf("the host continued past a block whose when could not evaluate: %s", labelResults(out))
	}
	var sawFailure bool
	for _, r := range out {
		if r.Failed {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Errorf("no failure reported for an unevaluatable when: %s", labelResults(out))
	}
}

// A task AFTER a block that failed inside it must still not run: the fix is
// about hosts the block SKIPPED, not about forgiving failures.
func TestAFailedBlockStillEndsThePlayForTheHost(t *testing.T) {
	out := runTasks(t, ""+
		"    - block:\n        - command: \"false\"\n"+
		"    - command: echo after\n")
	if ranCommand(out, "after") {
		t.Errorf("the host continued past a failed block: %s", labelResults(out))
	}
}

// ranCommand reports whether an `echo <word>` task ran and succeeded. The
// fixtures echo rather than naming a bare word: a first version used
// `command: inside`, which tries to EXECUTE a binary called "inside" and
// fails -- so both the block's task and the one after it reported failed and
// the test said the fix had not worked. The fixture was wrong, not the fix.
func ranCommand(out []Result, word string) bool {
	for _, r := range out {
		if r.Skipped || r.Failed {
			continue
		}
		if s, ok := r.Extra["stdout"].(string); ok && s == word {
			return true
		}
	}
	return false
}

func labelResults(out []Result) string {
	s := ""
	for _, r := range out {
		s += "{" + r.Module
		if r.Skipped {
			s += " skipped"
		}
		if r.Failed {
			s += " failed"
		}
		if cmd, ok := r.Extra["cmd"].(string); ok {
			s += " cmd=" + cmd
		}
		s += "}"
	}
	return s
}
