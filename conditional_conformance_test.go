package playbook

import (
	"context"
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
