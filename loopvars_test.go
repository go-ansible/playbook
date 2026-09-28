package playbook

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// runAndCollect runs a one-play playbook on localhost and returns the
// msg of every result, in order, plus the whole RunResult.
func runAndCollect(t *testing.T, src string) ([]string, *RunResult) {
	t.Helper()
	pb, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	e := New(localhostInventory())
	e.OnResult = func(r Result) { msgs = append(msgs, r.Msg) }
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	return msgs, rr
}

// TestLoopVariablesRealPublishes pins the three variables real sets on
// a loop iteration. All three values were measured against ansible-core
// 2.21.4; before this change ansible_loop_var and ansible_index_var
// were never set at all and ansible_loop did not exist, so a task
// naming any of them failed with "is undefined".
func TestLoopVariablesRealPublishes(t *testing.T) {
	for _, tc := range []struct {
		name, tasks string
		want        []string
	}{
		{
			// Real: var=item
			name: "ansible_loop_var names the loop variable",
			tasks: `    - debug: msg="var={{ ansible_loop_var }}"
      loop: [x]`,
			want: []string{"var=item"},
		},
		{
			// Real: var=thing -- it follows a rename, which is the
			// point: a role cannot know what its caller named it.
			name: "and follows loop_var",
			tasks: `    - debug: msg="var={{ ansible_loop_var }}"
      loop: [y]
      loop_control: {loop_var: thing}`,
			want: []string{"var=thing"},
		},
		{
			// Real: ivar=idx i=0 then ivar=idx i=1
			name: "ansible_index_var names the index variable",
			tasks: `    - debug: msg="ivar={{ ansible_index_var }} i={{ idx }}"
      loop: [p, q]
      loop_control: {index_var: idx}`,
			want: []string{"ivar=idx i=0", "ivar=idx i=1"},
		},
		{
			// Real: defined=False -- the variable is ABSENT, not
			// empty. Interpolated into a string rather than left bare,
			// because a bare expression keeps its NATIVE type and
			// prints as JSON false; measured both ways.
			name: "ansible_index_var is absent without index_var",
			tasks: `    - debug: msg="defined={{ ansible_index_var is defined }}"
      loop: [z]`,
			want: []string{"defined=False"},
		},
		{
			name: "ansible_loop is absent without extended",
			tasks: `    - debug: msg="defined={{ ansible_loop is defined }}"
      loop: [q]`,
			want: []string{"defined=False"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs, rr := runAndCollect(t, "- hosts: all\n  gather_facts: false\n  tasks:\n"+tc.tasks+"\n")
			if rr.Failed() {
				t.Fatalf("run failed: %+v", rr.Plays)
			}
			if len(msgs) != len(tc.want) {
				t.Fatalf("got %d results %v, real gives %d", len(msgs), msgs, len(tc.want))
			}
			for i := range tc.want {
				if msgs[i] != tc.want[i] {
					t.Errorf("iteration %d = %q, real gives %q", i, msgs[i], tc.want[i])
				}
			}
		})
	}
}

// TestExtendedLoopVars pins the ansible_loop dict, measured cell by
// cell against real for a three-item loop.
func TestExtendedLoopVars(t *testing.T) {
	msgs, rr := runAndCollect(t, `
- hosts: all
  gather_facts: false
  tasks:
    - debug: msg="i={{ ansible_loop.index }} i0={{ ansible_loop.index0 }} first={{ ansible_loop.first }} last={{ ansible_loop.last }} len={{ ansible_loop.length }} rev={{ ansible_loop.revindex }} rev0={{ ansible_loop.revindex0 }}"
      loop: [a, b, c]
      loop_control: {extended: true}
`)
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	want := []string{
		"i=1 i0=0 first=True last=False len=3 rev=3 rev0=2",
		"i=2 i0=1 first=False last=False len=3 rev=2 rev0=1",
		"i=3 i0=2 first=False last=True len=3 rev=1 rev0=0",
	}
	for i := range want {
		if i >= len(msgs) || msgs[i] != want[i] {
			t.Errorf("iteration %d:\n got %q\nwant %q (real's own)", i, safeAt(msgs, i), want[i])
		}
	}

	// previtem and nextitem are ABSENT at the ends, not null: real
	// catches the IndexError and guards index-1 >= 0. Measured:
	// prev=<none> next=b, then prev=a next=<none>.
	msgs, rr = runAndCollect(t, `
- hosts: all
  gather_facts: false
  tasks:
    - debug: msg="prev={{ ansible_loop.previtem | default('<none>') }} next={{ ansible_loop.nextitem | default('<none>') }} allitems={{ ansible_loop.allitems }}"
      loop: [a, b]
      loop_control: {extended: true}
`)
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	for i, w := range []string{
		"prev=<none> next=b allitems=['a', 'b']",
		"prev=a next=<none> allitems=['a', 'b']",
	} {
		if i >= len(msgs) || msgs[i] != w {
			t.Errorf("neighbour %d:\n got %q\nwant %q", i, safeAt(msgs, i), w)
		}
	}

	// The key is ABSENT at the end rather than present-and-null, and
	// these are the values real gives: prevdef=False nextdef=True,
	// then prevdef=True nextdef=False.
	//
	// Said plainly: NEITHER this check nor the one above can currently
	// tell the two apart in this port, and a neuter setting previtem
	// to nil passes both. The reason is a separate divergence in the
	// template package, measured against ansible-core 2.21.4:
	//
	//	d.null is defined        real True   here false
	//	d.null | default('X')    real ''     here 'X'
	//
	// A null value IS defined in real, and default() does not fire on
	// it. Until that is fixed, absence and null are indistinguishable
	// from a template, so this assertion pins real's values without
	// being able to prove the distinction. It is written down rather
	// than left as a silent hole.
	msgs, rr = runAndCollect(t, `
- hosts: all
  gather_facts: false
  tasks:
    - debug: msg="prevdef={{ ansible_loop.previtem is defined }} nextdef={{ ansible_loop.nextitem is defined }}"
      loop: [a, b]
      loop_control: {extended: true}
`)
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	for i, w := range []string{"prevdef=False nextdef=True", "prevdef=True nextdef=False"} {
		if i >= len(msgs) || msgs[i] != w {
			t.Errorf("neighbour presence %d:\n got %q\nwant %q (real's own)", i, safeAt(msgs, i), w)
		}
	}

	// extended_allitems: false drops exactly one key.
	msgs, rr = runAndCollect(t, `
- hosts: all
  gather_facts: false
  tasks:
    - debug: msg="allitems={{ ansible_loop.allitems is defined }}"
      loop: [a, b]
      loop_control: {extended: true, extended_allitems: false}
`)
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	for i := range msgs {
		if msgs[i] != "allitems=False" {
			t.Errorf("extended_allitems false: iteration %d = %q, real gives allitems=False", i, msgs[i])
		}
	}
}

// TestBreakWhen pins loop_control.break_when: it ends the loop AFTER
// the iteration that triggered it, and a list of conditions is AND-ed.
func TestBreakWhen(t *testing.T) {
	for _, tc := range []struct {
		name, cond string
		wantItems  int
	}{
		// Real shows 1 and 2 and never reaches 3 -- the triggering
		// item has already run, which is the whole difference from
		// when:.
		{"stops after the triggering item", `["item == 2"]`, 2},
		// AND-ed, so these never hold together and all three run.
		{"a list is AND-ed", `["item == 2", "item == 3"]`, 3},
		{"two that agree", `["item >= 2", "item is even"]`, 2},
		{"never true runs them all", `["item == 99"]`, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs, rr := runAndCollect(t, fmt.Sprintf(`
- hosts: all
  gather_facts: false
  tasks:
    - debug: msg="item {{ item }}"
      loop: [1, 2, 3]
      loop_control: {break_when: %s}
`, tc.cond))
			if rr.Failed() {
				t.Fatalf("run failed: %+v", rr.Plays)
			}
			if len(msgs) != tc.wantItems {
				t.Errorf("ran %d iterations %v, real runs %d", len(msgs), msgs, tc.wantItems)
			}
		})
	}
}

// TestBreakWhenResult pins where the outcome goes: into the REGISTERED
// per-iteration result on success, and into the printed one as well
// when the expression could not be evaluated.
func TestBreakWhenResult(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: looped
      debug: msg="item {{ item }}"
      loop: [1, 2, 3]
      loop_control: {break_when: ["item == 2"]}
      register: r
    - name: report
      debug: msg="v={{ r.results | map(attribute='break_when_result') | list }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	var printed []map[string]any
	var reported string
	e := New(localhostInventory())
	e.OnResult = func(res Result) {
		switch res.Task {
		case "looped":
			printed = append(printed, res.Extra)
		case "report":
			reported = res.Msg
		}
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	// Real: [False, True] -- one per iteration that ran.
	if want := "v=[False, True]"; reported != want {
		t.Errorf("registered break_when_result = %q, real gives %q", reported, want)
	}
	// And NOT on the printed line: real's debug iteration prints msg
	// and nothing else.
	for i, extra := range printed {
		if _, ok := extra["break_when_result"]; ok {
			t.Errorf("iteration %d printed break_when_result; real prints it only when the expression FAILED", i)
		}
	}
}

// TestBreakWhenCannotSeeTheResult pins a measured NEGATIVE: unlike
// until:, break_when does not get the task's result in scope. Real
// fails with "'stdout' is undefined" and puts that text in
// break_when_result rather than raising a separate result.
func TestBreakWhenCannotSeeTheResult(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: looped
      command: "echo {{ item }}"
      loop: [go, stop]
      loop_control: {break_when: ["stdout == 'stop'"]}
      ignore_errors: true
`))
	if err != nil {
		t.Fatal(err)
	}
	var results []Result
	e := New(localhostInventory())
	e.OnResult = func(r Result) { results = append(results, r) }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	// One iteration only: the loop stops at the failure.
	if len(results) != 1 {
		t.Fatalf("got %d results, real reports exactly one (the loop stops)", len(results))
	}
	r := results[0]
	if !r.Failed {
		t.Error("an unevaluable break_when must fail the iteration")
	}
	text, _ := r.Extra["break_when_result"].(string)
	if !strings.Contains(text, "'stdout' is undefined") {
		t.Errorf("break_when_result = %q, real carries the conditional's own error", text)
	}
	// The module's own keys survive alongside it -- real's failed line
	// carries rc, stdout and the rest.
	if _, ok := r.Extra["rc"]; !ok {
		t.Error("the module's own result keys must survive: real's failed line carries rc, stdout and the rest")
	}
}

func safeAt(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "<missing>"
}
