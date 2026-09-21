package playbook

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// TestConditionalAcceptsTemplateDelimiters pins `when: "{{ flag }}"`,
// which real ansible-core still accepts — with a deprecation warning
// saying it will be removed in 2.23 — and which this port FAILED
// outright.
//
// Every conditional here is evaluated by wrapping it in {{ }}, so one
// already wrapped became `{{ {{ flag }} }}` and died on a syntax error.
// The idiom is deprecated but widespread, and a hard failure on a
// playbook real Ansible runs is the worst way to meet it.
func TestConditionalAcceptsTemplateDelimiters(t *testing.T) {
	tests := []struct {
		when string
		want bool // did the task run?
	}{
		{`flag`, true},
		{`{{ flag }}`, true},
		{`{{ not flag }}`, false},
		{`  {{ flag }}  `, true},
		{`{{ n > 0 }}`, true},
		{`{{ n > 5 }}`, false},
		// Not a single wrapped expression: left exactly as written, so
		// it fails the same way it would in real Ansible.
		{`n > 0`, true},
		{`not flag`, false},
	}
	for _, tt := range tests {
		t.Run(tt.when, func(t *testing.T) {
			pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  vars: {flag: true, n: 1}
  tasks:
    - {name: t, debug: {msg: MARKER}, when: "` + tt.when + `"}
`))
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			ran, failed := false, false
			e := New(localhostInventory())
			e.OnResult = func(r Result) {
				mu.Lock()
				defer mu.Unlock()
				if r.Failed {
					failed = true
				} else if !r.Skipped {
					ran = true
				}
			}
			if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
				t.Fatal(err)
			}
			if failed {
				t.Fatalf("when: %q failed the task; real ansible-core evaluates it", tt.when)
			}
			if ran != tt.want {
				t.Errorf("when: %q ran=%v, real ansible-core gives ran=%v", tt.when, ran, tt.want)
			}
		})
	}
}

func TestStripConditionDelimiters(t *testing.T) {
	tests := []struct{ in, want string }{
		{"flag", "flag"},
		{"{{ flag }}", "flag"},
		{"  {{ flag }}  ", "flag"},
		{"{{flag}}", "flag"},
		// Two blocks are not one wrapped expression, so they are left
		// alone rather than mangled into something that only looks
		// valid.
		{"{{ a }} and {{ b }}", "{{ a }} and {{ b }}"},
		{"{{ a }}x", "{{ a }}x"},
		{"x{{ a }}", "x{{ a }}"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripConditionDelimiters(tt.in); got != tt.want {
			t.Errorf("stripConditionDelimiters(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// until: and changed_when: go through the same rule.
func TestOtherConditionalsAcceptDelimitersToo(t *testing.T) {
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - name: t
      command: echo hi
      changed_when: "{{ false }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var changed, failed bool
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		mu.Lock()
		defer mu.Unlock()
		changed = changed || r.Changed
		failed = failed || r.Failed
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if failed {
		t.Fatal("changed_when with delimiters failed the task")
	}
	if changed {
		t.Error("changed_when: {{ false }} must make the task unchanged")
	}
}

// TestConditionalsMustBeBoolean pins real ansible-core 2.21's own rule:
// a conditional whose result is not a boolean is an ERROR. The reason
// matters — such a conditional is usually a template used where one is
// not supported, and it then reads as always true, so the task runs
// every time, silently.
//
// This port applied ordinary truthiness and ran them.
func TestConditionalsMustBeBoolean(t *testing.T) {
	tests := []struct {
		when   string
		strict string // "run", "skip" or "fail"
		broken string // with AllowBrokenConditionals
	}{
		{`flag`, "run", "run"},       // a real bool is fine either way
		{`not flag`, "skip", "skip"}, //
		{`n > 0`, "run", "run"},      // an expression yielding a bool
		{`n`, "fail", "run"},         // int
		{`s`, "fail", "run"},         // string
		{`lst`, "fail", "run"},       // list
		{`dct`, "fail", "run"},       // dict
		{`nothing`, "fail", "skip"},  // null is falsy once allowed
		{`empty`, "fail", "skip"},    // and so is an empty string
	}
	for _, tt := range tests {
		for _, mode := range []string{"strict", "broken"} {
			want := tt.strict
			if mode == "broken" {
				want = tt.broken
			}
			t.Run(tt.when+"/"+mode, func(t *testing.T) {
				pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  vars: {flag: true, s: "yes", n: 1, lst: [1], dct: {a: 1}, nothing: null, empty: ""}
  tasks:
    - {name: t, debug: {msg: M}, when: "` + tt.when + `"}
`))
				if err != nil {
					t.Fatal(err)
				}
				e := New(localhostInventory())
				e.AllowBrokenConditionals = mode == "broken"
				var mu sync.Mutex
				got := "skip"
				e.OnResult = func(r Result) {
					mu.Lock()
					defer mu.Unlock()
					switch {
					case r.Failed:
						got = "fail"
					case !r.Skipped:
						got = "run"
					}
				}
				if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Errorf("when: %q (%s) = %s, real ansible-core gives %s", tt.when, mode, got, want)
				}
			})
		}
	}
}

// The refusal says what real Ansible's says, including the way out.
func TestBooleanConditionalErrorWording(t *testing.T) {
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  vars: {s: "yes"}
  tasks:
    - {name: t, debug: {msg: M}, when: s}
`))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var msg string
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		mu.Lock()
		defer mu.Unlock()
		if r.Failed {
			msg = r.Msg
		}
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`Conditional result (True) was derived from value of type "str"`,
		"Conditionals must have a boolean result",
		"ALLOW_BROKEN_CONDITIONALS",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must contain %q, got: %s", want, msg)
		}
	}
}
