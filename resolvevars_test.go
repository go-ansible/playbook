package playbook

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Measured against ansible-core 2.21.4: a variable whose value is a
// template resolves when it is referenced, and this port handed back
// the raw text -- for play vars, for group_vars, and inside nested
// lists and dicts.
func TestPlayVarsThatAreTemplatesResolve(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  vars:
    a: hello
    b: "{{ a }}-world"
    n: "{{ 1 + 1 }}"
    lst: ["{{ a }}", plain]
    dct: {k: "{{ a }}"}
  tasks:
    - {name: t, debug: {msg: "b={{ b }} n={{ n }} lst={{ lst }} dct={{ dct }}"}}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var got string
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if !r.BannerOnly && r.Msg != "" {
			got = r.Msg
		}
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	want := "b=hello-world n=2 lst=['hello', 'plain'] dct={'k': 'hello'}"
	if got != want {
		t.Errorf("msg = %q, want %q", got, want)
	}
}

// A variable holding an expression resolves to the VALUE, not to its
// text: "{{ 1 + 1 }}" is the number 2, so a later comparison against a
// number works.
func TestResolvedKeepsTypes(t *testing.T) {
	e := New(localhostInventory())
	got := e.resolved(map[string]any{
		"one": 1,
		"n":   "{{ 1 + 1 }}",
		"s":   "{{ 'x' }}",
	})
	if got["n"] != 2 {
		t.Errorf("n = %#v (%T), want the number 2", got["n"], got["n"])
	}
	if got["s"] != "x" {
		t.Errorf("s = %#v", got["s"])
	}
}

// A chain resolves however deep it is written, because the walk
// repeats until the set settles.
// Repeated, because Go randomises map iteration order: a chain can
// resolve in ONE pass if the walk happens to visit a, then b, then c.
// A single run therefore proves nothing about the repetition -- with
// the pass limit set to 1 it passed, which is how this was caught.
// Fifty runs make the unlucky orders certain.
func TestResolvedFollowsAChain(t *testing.T) {
	e := New(localhostInventory())
	for i := 0; i < 50; i++ {
		got := e.resolved(map[string]any{
			"a": "base",
			"b": "{{ a }}/b",
			"c": "{{ b }}/c",
			"d": "{{ c }}/d",
		})
		if got["d"] != "base/b/c/d" {
			t.Fatalf("run %d: d = %#v, want base/b/c/d", i, got["d"])
		}
	}
}

// Two variables citing each other never settle. Real reports a
// recursion error; this stops after a bounded number of passes and
// hands back what it has, so one silly variable does not take the
// playbook down.
func TestResolvedStopsOnACycle(t *testing.T) {
	e := New(localhostInventory())
	done := make(chan map[string]any, 1)
	go func() {
		done <- e.resolved(map[string]any{"a": "{{ b }}", "b": "{{ a }}"})
	}()
	select {
	case <-done:
	case <-context.Background().Done():
	}
	// Reaching here at all is the assertion: an unbounded walk would
	// hang the test binary rather than fail it.
}

// A value that cannot be resolved is LEFT AS IT IS. Real only fails on
// a variable that is actually referenced, so failing here would break
// a playbook that merely defines one it never uses.
func TestResolvedLeavesWhatItCannotRender(t *testing.T) {
	e := New(localhostInventory())
	got := e.resolved(map[string]any{
		"good": "{{ 1 + 1 }}",
		"bad":  "{{ nothing_defines_this }}",
	})
	if got["good"] != 2 {
		t.Errorf("good = %#v", got["good"])
	}
	if s, ok := got["bad"].(string); !ok || !strings.Contains(s, "nothing_defines_this") {
		t.Errorf("bad = %#v, want the template text kept", got["bad"])
	}
}

// A set with no templates in it is returned AS IS, not copied: the
// merged variables carry hostvars and groups for every host, and
// copying that per task would cost more than the feature is worth.
func TestResolvedDoesNotCopyWhenNothingChanges(t *testing.T) {
	e := New(localhostInventory())
	in := map[string]any{"a": "plain", "n": 1, "l": []any{"x"}}
	got := e.resolved(in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %#v", got)
	}
	got["marker"] = true
	if _, ok := in["marker"]; !ok {
		t.Error("the input was copied although nothing needed resolving")
	}
	delete(in, "marker")
}
