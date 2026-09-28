package playbook

import (
	"context"
	"strings"
	"testing"
)

// runTasks runs a one-play playbook on localhost and returns every
// result in order.
func runTasks(t *testing.T, tasks string) []Result {
	t.Helper()
	pb, err := Parse([]byte("- hosts: all\n  gather_facts: false\n  tasks:\n" + tasks))
	if err != nil {
		t.Fatal(err)
	}
	var out []Result
	e := New(localhostInventory())
	e.OnResult = func(r Result) { out = append(out, r) }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestFailedWhenReplacesTheModuleVerdict pins the direction this port
// could not go. failed_when does not merely ADD a failure, it REPLACES
// the module's own verdict — and the port gated it on !result.Failed,
// so `failed_when: false`, the whole reason the keyword exists, did
// nothing at all on a task that had actually failed.
func TestFailedWhenReplacesTheModuleVerdict(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tasks       string
		wantFailed  bool
		wantChanged bool
		wantFWR     any
		wantMsg     string
	}{
		{
			// Real prints "changed: [localhost]" and the recap counts
			// no failure, while rc stays 1 and the module's own msg
			// SURVIVES being forgiven.
			name:        "false forgives a failing module",
			tasks:       "    - command: \"false\"\n      failed_when: false\n",
			wantFailed:  false,
			wantChanged: true,
			wantFWR:     false,
			wantMsg:     "The command exited with a non-zero return code.",
		},
		{
			// Here the expression is what failed the task, so real
			// REPLACES the module's (empty) msg with its own.
			name:        "true fails a succeeding module",
			tasks:       "    - command: \"true\"\n      failed_when: true\n      ignore_errors: true\n",
			wantFailed:  true,
			wantChanged: true,
			wantFWR:     true,
			wantMsg:     "Task failed: Action failed: A 'failed_when' expression evaluated to 'True'.",
		},
		{
			name:        "false on a succeeding module leaves it alone",
			tasks:       "    - command: \"true\"\n      failed_when: false\n",
			wantFailed:  false,
			wantChanged: true,
			wantFWR:     false,
			wantMsg:     "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := runTasks(t, tc.tasks)
			if len(rs) != 1 {
				t.Fatalf("got %d results, want 1", len(rs))
			}
			r := rs[0]
			if r.Failed != tc.wantFailed {
				t.Errorf("failed = %v, real gives %v", r.Failed, tc.wantFailed)
			}
			if r.Changed != tc.wantChanged {
				t.Errorf("changed = %v, real gives %v", r.Changed, tc.wantChanged)
			}
			if got := r.Extra["failed_when_result"]; got != tc.wantFWR {
				t.Errorf("failed_when_result = %#v, real gives %#v", got, tc.wantFWR)
			}
			if r.Msg != tc.wantMsg {
				t.Errorf("msg =\n %q\nreal gives\n %q", r.Msg, tc.wantMsg)
			}
		})
	}
}

// TestChangedWhenCarriesItsBoolean pins the other half: changed_when
// records its result too, and — measured — does NOT touch failed.
func TestChangedWhenCarriesItsBoolean(t *testing.T) {
	rs := runTasks(t, "    - command: \"true\"\n      changed_when: false\n")
	if got := rs[0].Extra["changed_when_result"]; got != false {
		t.Errorf("changed_when_result = %#v, real gives false", got)
	}
	if rs[0].Changed {
		t.Error("changed_when: false must leave the task unchanged")
	}

	// A FAILING command with changed_when: true is changed AND failed
	// in real, keeping the module's own msg.
	rs = runTasks(t, "    - command: \"false\"\n      changed_when: true\n      ignore_errors: true\n")
	r := rs[0]
	if !r.Changed || !r.Failed {
		t.Errorf("changed=%v failed=%v, real gives both true", r.Changed, r.Failed)
	}
	if got := r.Extra["changed_when_result"]; got != true {
		t.Errorf("changed_when_result = %#v, real gives true", got)
	}
	if r.Msg != "The command exited with a non-zero return code." {
		t.Errorf("msg = %q -- changed_when must not replace the module's own", r.Msg)
	}
}

// TestNoWhenModifiersCarryNoKeys pins the negative: a task without the
// keywords has neither key. Measured: `f.failed_when_result is
// defined` is False.
func TestNoWhenModifiersCarryNoKeys(t *testing.T) {
	r := runTasks(t, "    - command: \"true\"\n")[0]
	for _, k := range []string{"failed_when_result", "changed_when_result"} {
		if _, ok := r.Extra[k]; ok {
			t.Errorf("a task with no when-modifiers carries %s; real carries neither", k)
		}
	}
}

// TestFailedWhenThatCannotEvaluate pins the error path, which was
// already right and must stay so: the reason goes in
// failed_when_result and the task fails.
func TestFailedWhenThatCannotEvaluate(t *testing.T) {
	r := runTasks(t, "    - command: \"true\"\n      failed_when: \"no_such_var == 1\"\n      ignore_errors: true\n")[0]
	if !r.Failed {
		t.Error("an unevaluable failed_when must fail the task")
	}
	text, _ := r.Extra["failed_when_result"].(string)
	if !strings.Contains(text, "'no_such_var' is undefined") {
		t.Errorf("failed_when_result = %q, real carries the conditional's own error", text)
	}
	if want := "Task failed: Action failed: A 'failed_when' expression failed: Error while evaluating conditional: 'no_such_var' is undefined"; r.Msg != want {
		t.Errorf("msg =\n %q\nreal gives\n %q", r.Msg, want)
	}
}
