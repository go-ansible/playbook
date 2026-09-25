package playbook

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/go-ansible/inventory"
)

func twoLocalHosts(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.ParseYAML([]byte(`
all:
  hosts:
    h1:
      ansible_connection: local
    h2:
      ansible_connection: local
`))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// Measured against ansible-core 2.21.4: a free play run with -f 1
// produces a transcript indistinguishable from a linear one -- one
// banner per task with both hosts under it, hosts alternating task by
// task -- because a single worker can only have one task in flight, so
// real's free scheduler ends up offering the hosts strictly in turn.
//
// This port ran each host's WHOLE task list before the next one's,
// repeating every banner, because its fork cap was bypassed entirely
// when Forks == 1.
func TestFreeWithOneForkRunsHostsInTurn(t *testing.T) {
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  strategy: free
  tasks:
    - {name: one, debug: {msg: "one {{ inventory_hostname }}"}}
    - {name: two, debug: {msg: "two {{ inventory_hostname }}"}}
    - {name: three, debug: {msg: "three {{ inventory_hostname }}"}}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Repeated, because the defect this replaces was a RACE: one run
	// proves nothing about an ordering that used to come out three
	// different ways.
	want := "one/h1 one/h2 two/h1 two/h2 three/h1 three/h2"
	for i := 0; i < 20; i++ {
		var seen []string
		e := New(twoLocalHosts(t))
		e.Forks = 1
		e.OnResult = func(r Result) {
			if r.BannerOnly {
				return
			}
			seen = append(seen, r.Task+"/"+r.Host)
		}
		if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if got := strings.Join(seen, " "); got != want {
			t.Fatalf("run %d order = %q, want %q", i, got, want)
		}
	}
}

// With more than one fork, free keeps its own scheduler: a host is not
// held behind another, which is the whole point of the strategy. The
// ORDER is then genuinely up to the hosts, so this asserts only that
// every host ran every task -- asserting an order here would be
// asserting a race.
func TestFreeWithSeveralForksStillRunsEveryTask(t *testing.T) {
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  strategy: free
  tasks:
    - {name: one, debug: {msg: x}}
    - {name: two, debug: {msg: x}}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e := New(twoLocalHosts(t))
	e.Forks = 5
	// OnResult is called from whichever goroutine produced the result
	// — the Engine says so and does not serialise for the caller — so
	// the caller must. Under free with several forks that is several
	// goroutines at once.
	var mu sync.Mutex
	seen := map[string]bool{}
	e.OnResult = func(r Result) {
		if r.BannerOnly {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		seen[r.Task+"/"+r.Host] = true
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"one/h1", "one/h2", "two/h1", "two/h2"} {
		if !seen[want] {
			t.Errorf("%s did not run: %v", want, seen)
		}
	}
}
