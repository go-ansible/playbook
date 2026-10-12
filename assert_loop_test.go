package playbook

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// runAssertResults runs a one-play playbook on localhost and returns
// every result the task named "a" produced, in order, plus the whole
// RunResult. Collected by task name so a setup task in the same play
// (a register, a set_fact) does not shift the indexes.
func runAssertResults(t *testing.T, src string) ([]Result, *RunResult) {
	t.Helper()
	pb, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var got []Result
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if r.Task == "a" {
			got = append(got, r)
		}
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	return got, rr
}

// noUndefined fails the test when any result carries the message
// go-ansible/playbook#196 was about. Checked separately from pass/fail
// because a failing assert and an assert that could not evaluate its
// condition both come out Failed, and only one of them is right.
func noUndefined(t *testing.T, results []Result) {
	t.Helper()
	for i, r := range results {
		if strings.Contains(r.Msg, "is undefined") {
			t.Errorf("result %d: %q -- `that` was evaluated without the loop's variables", i, r.Msg)
		}
	}
}

// TestAssertLoopIssue196 is the reproduction from the issue, verbatim:
// both items are positive, so both iterations pass. Before the fix both
// failed with "'item' is undefined", because `that` was evaluated
// against the host's variables rather than the iteration's.
func TestAssertLoopIssue196(t *testing.T) {
	results, rr := runAssertResults(t, `
- hosts: all
  gather_facts: false
  tasks:
    - name: a
      ansible.builtin.assert:
        that:
          - item > 0
        fail_msg: "{{ item }} is not positive"
      loop: [1, 2]
`)
	noUndefined(t, results)
	if rr.Failed() {
		t.Fatalf("looped assert over positive items failed: %+v", results)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want one per item (2): %+v", len(results), results)
	}
	for i, r := range results {
		if r.Msg != "All assertions passed" {
			t.Errorf("item %d msg = %q, want the default success_msg", i, r.Msg)
		}
		if fmt.Sprint(r.Item) != fmt.Sprint(i+1) {
			t.Errorf("result %d item = %v, want %d", i, r.Item, i+1)
		}
	}
}

// TestAssertLoopFailingItem: a loop where only one item breaks the
// condition fails that iteration, and the failure names it -- both on
// the result's Item (what the "(item=...)" on the failed line comes
// from) and through fail_msg, which renders from the same per-item
// scope `that` now uses. The passing items around it still pass.
func TestAssertLoopFailingItem(t *testing.T) {
	results, _ := runAssertResults(t, `
- hosts: all
  gather_facts: false
  tasks:
    - name: a
      assert:
        that: item > 0
        fail_msg: "{{ item }} is not positive"
        success_msg: "{{ item }} is fine"
      loop: [1, -3, 2]
      ignore_errors: true
`)
	noUndefined(t, results)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3: %+v", len(results), results)
	}
	for i, want := range []struct {
		item   string
		failed bool
		msg    string
	}{
		{"1", false, "1 is fine"},
		{"-3", true, "-3 is not positive"},
		{"2", false, "2 is fine"},
	} {
		r := results[i]
		if fmt.Sprint(r.Item) != want.item {
			t.Errorf("result %d item = %v, want %s", i, r.Item, want.item)
		}
		if r.Failed != want.failed {
			t.Errorf("item %s failed = %v, want %v (msg %q)", want.item, r.Failed, want.failed, r.Msg)
		}
		if r.Msg != want.msg {
			t.Errorf("item %s msg = %q, want %q", want.item, r.Msg, want.msg)
		}
	}
	// The failing one reports WHICH condition failed, as a non-looped
	// assert does.
	if got := results[1].Extra["assertion"]; got != "item > 0" {
		t.Errorf("assertion = %v, want %q", got, "item > 0")
	}
}

// TestAssertLoopControlVars: the renamed loop variable, index_var, the
// two names real publishes for them, and ansible_loop under
// extended: true are all visible to `that`, because they are all set
// on the iteration scope `that` is now evaluated against.
func TestAssertLoopControlVars(t *testing.T) {
	for _, tc := range []struct {
		name, task string
		items      int
	}{
		{
			name: "loop_var",
			task: `      assert:
        that:
          - thing.n > 0
          - ansible_loop_var == "thing"
      loop: [{n: 1}, {n: 2}]
      loop_control: {loop_var: thing}`,
			items: 2,
		},
		{
			name: "index_var",
			task: `      assert:
        that:
          - idx == item - 10
          - ansible_index_var == "idx"
      loop: [10, 11, 12]
      loop_control: {index_var: idx}`,
			items: 3,
		},
		{
			name: "extended",
			task: `      assert:
        that:
          - ansible_loop.index == item
          - ansible_loop.length == 2
      loop: [1, 2]
      loop_control: {extended: true}`,
			items: 2,
		},
		{
			// A registered result of an earlier looped task, iterated
			// over: the shape of the task the issue was reduced from.
			name: "registered results",
			task: `      assert:
        that: item.rc == 0
      loop: "{{ ids.results }}"`,
			items: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results, rr := runAssertResults(t, `
- hosts: all
  gather_facts: false
  tasks:
    - command: "echo {{ item }}"
      loop: [x, y]
      register: ids
    - name: a
`+tc.task+"\n")
			noUndefined(t, results)
			if rr.Failed() {
				t.Fatalf("run failed: %+v", results)
			}
			if len(results) != tc.items {
				t.Fatalf("got %d results, want %d: %+v", len(results), tc.items, results)
			}
		})
	}
}

// TestAssertLoopQuiet: quiet: true still drops the verbose-always mark
// per iteration, and its absence still sets it, so a looped assert
// prints the way a non-looped one does.
func TestAssertLoopQuiet(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		results, rr := runAssertResults(t, fmt.Sprintf(`
- hosts: all
  gather_facts: false
  tasks:
    - name: a
      assert:
        that: item
        quiet: %v
      loop: [true, true]
`, quiet))
		noUndefined(t, results)
		if rr.Failed() {
			t.Fatalf("quiet=%v: run failed: %+v", quiet, results)
		}
		for i, r := range results {
			v, _ := r.Extra[verboseAlwaysKey].(bool)
			if v == quiet {
				t.Errorf("quiet=%v item %d: verbose-always = %v, want %v", quiet, i, v, !quiet)
			}
		}
	}
}

// TestAssertNonLoopedScope: a non-looped assert still sees everything
// it saw before (play vars, a registered result, set_fact), and `item`
// stays undefined outside a loop -- the fix gives `that` the task's
// scope, it does not invent a loop variable. It now also sees the
// task's own `vars:`, which the host-level scope it used to read did
// not include.
func TestAssertNonLoopedScope(t *testing.T) {
	results, rr := runAssertResults(t, `
- hosts: all
  gather_facts: false
  vars: {n: 5}
  tasks:
    - command: echo hi
      register: out
    - set_fact: {f: 7}
    - name: a
      assert:
        that:
          - n == 5
          - out.stdout == "hi"
          - f == 7
          - t == 3
      vars: {t: 3}
`)
	if rr.Failed() || len(results) != 1 {
		t.Fatalf("non-looped assert: %+v", results)
	}

	results, _ = runAssertResults(t, `
- hosts: all
  gather_facts: false
  tasks:
    - name: a
      assert: {that: item > 0}
      ignore_errors: true
`)
	if len(results) != 1 || !results[0].Failed || !strings.Contains(results[0].Msg, "'item' is undefined") {
		t.Errorf("item outside a loop must still be undefined: %+v", results)
	}
}
