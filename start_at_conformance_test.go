package playbook

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
)

const startAtPlaybook = `
- name: p1
  hosts: all
  gather_facts: false
  tasks:
    - {name: one,   debug: {msg: 1}}
    - {name: two,   debug: {msg: 2}}
    - {name: three, debug: {msg: 3}}
- name: p2
  hosts: all
  gather_facts: false
  tasks:
    - {name: four, debug: {msg: 4}}
`

// TestStartAtTaskMatchesRealAnsible pins --start-at-task to runs
// measured from real ansible-core 2.21.4, including its glob matching
// and its case sensitivity.
func TestStartAtTaskMatchesRealAnsible(t *testing.T) {
	tests := []struct {
		start string
		want  string
	}{
		{"", "one,two,three,four"},
		{"two", "two,three,four"},
		// The started state persists ACROSS plays: starting at a task
		// in the second play skips the whole first one.
		{"four", "four"},
		{"one", "one,two,three,four"},
		// Globs match; a bare prefix does not, and the match is
		// case-sensitive.
		{"tw*", "two,three,four"},
		{"*wo", "two,three,four"},
		{"tw", ""},
		{"TWO", ""},
		// A name that never matches runs nothing at all, and is not an
		// error.
		{"nosuch", ""},
	}
	for _, tt := range tests {
		name := tt.start
		if name == "" {
			name = "(unset)"
		}
		t.Run(name, func(t *testing.T) {
			if got := runStartAt(t, tt.start); got != tt.want {
				t.Errorf("--start-at-task %q ran %q, real ansible-core runs %q", tt.start, got, tt.want)
			}
		})
	}
}

// The started state persists across RunPlaybook calls on one Engine,
// which is what makes `ansible-playbook a.yml b.yml --start-at-task x`
// run all of b.yml when x is in a.yml.
func TestStartAtTaskPersistsAcrossPlaybooks(t *testing.T) {
	first, err := Parse([]byte(startAtPlaybook))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse([]byte(`
- name: p3
  hosts: all
  gather_facts: false
  tasks:
    - {name: five, debug: {msg: 5}}
`))
	if err != nil {
		t.Fatal(err)
	}

	e := New(localhostInventory())
	e.StartAtTask = "three"
	var mu sync.Mutex
	var ran []string
	e.OnResult = func(r Result) {
		if r.Skipped {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, r.Task)
	}
	for _, pb := range []Playbook{first, second} {
		if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(ran, ","); got != "three,four,five" {
		t.Errorf("ran %q, want three,four,five — the second playbook must not re-skip", got)
	}
}

func runStartAt(t *testing.T, start string) string {
	t.Helper()
	pb, err := Parse([]byte(startAtPlaybook))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.StartAtTask = start

	var mu sync.Mutex
	var ran []string
	e.OnResult = func(r Result) {
		if r.Skipped {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, r.Task)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	return strings.Join(ran, ",")
}

// A task skipped by --start-at-task leaves no trace at all, exactly as
// one excluded by tags does.
func TestStartAtTaskSkipsSilently(t *testing.T) {
	pb, err := Parse([]byte(startAtPlaybook))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.StartAtTask = "three"
	var mu sync.Mutex
	var seen []string
	e.OnResult = func(r Result) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Task)
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(seen)
	if got := strings.Join(seen, ","); got != "four,three" {
		t.Errorf("results = %q; skipped tasks must produce none at all", got)
	}
	if s := rr.Summary()["localhost"]; s.Skipped != 0 {
		t.Errorf("recap counts %d skipped; a task before the start point is not reported", s.Skipped)
	}
}

// TestForceHandlersRunsHandlersOnFailedHosts pins --force-handlers.
// Measured against real ansible-core 2.21.4 with two hosts where one
// fails: without the flag the failed host's notified handler does not
// run, with it, it does.
func TestForceHandlersRunsHandlersOnFailedHosts(t *testing.T) {
	src := `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - {name: notifier, command: echo x, notify: hup}
    - name: boom
      command: sh -c "exit 1"
      when: "inventory_hostname == 'h1'"
    - {name: after, debug: {msg: after}}
  handlers:
    - {name: hup, debug: {msg: HANDLER}}
`
	for _, tt := range []struct {
		force bool
		want  string
	}{
		// h1 fails, so only h2 reaches its handler.
		{false, "h2"},
		// With the flag, the failed host runs it too.
		{true, "h1,h2"},
	} {
		pb, err := Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		e := New(fiveHostInventory())
		e.Limit = "h1,h2"
		e.ForceHandlers = tt.force
		var mu sync.Mutex
		var ran []string
		e.OnResult = func(r Result) {
			if r.Task != "hup" || r.Skipped {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ran = append(ran, r.Host)
		}
		if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
			t.Fatal(err)
		}
		sort.Strings(ran)
		if got := strings.Join(ran, ","); got != tt.want {
			t.Errorf("ForceHandlers=%v: handler ran on %q, real ansible-core runs it on %q", tt.force, got, tt.want)
		}
	}
}
