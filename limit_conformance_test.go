package playbook

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
)

// TestLimitMatchesRealAnsible pins --limit to hosts measured from real
// ansible-core 2.21.4. It takes the same pattern language as a play's
// own hosts:, and INTERSECTS with it rather than replacing it.
func TestLimitMatchesRealAnsible(t *testing.T) {
	tests := []struct {
		limit string
		want  string
	}{
		{"", "h1,h2,h3,h4,h5"},
		{"h1", "h1"},
		{"h1,h2", "h1,h2"},
		{"h1:h2", "h1,h2"},
		{"web", "h1,h2,h3,h4,h5"},
		{"!h1", "h2,h3,h4,h5"},
		{"web:!h1", "h2,h3,h4,h5"},
		{"h*", "h1,h2,h3,h4,h5"},
		// A limit naming an unknown host alongside a known one keeps
		// the known one and is not an error.
		{"h1,nosuch", "h1"},
		// A limit matching nothing leaves the run with no hosts. The
		// ERROR for that belongs to the CLI, which checks once against
		// "all" before any play runs; the engine simply has nothing to
		// do.
		{"nosuch", ""},
	}
	for _, tt := range tests {
		name := tt.limit
		if name == "" {
			name = "(unset)"
		}
		t.Run(name, func(t *testing.T) {
			if got := runLimited(t, "all", tt.limit); got != tt.want {
				t.Errorf("--limit %q ran on %q, real ansible-core runs on %q", tt.limit, got, tt.want)
			}
		})
	}
}

// The limit can only ever REMOVE hosts: a play already narrower than
// the limit keeps its own narrower set.
func TestLimitIntersectsRatherThanReplaces(t *testing.T) {
	if got := runLimited(t, "h1", "h1,h2,h3"); got != "h1" {
		t.Errorf("play hosts h1 with --limit h1,h2,h3 ran on %q, want h1", got)
	}
	if got := runLimited(t, "h1,h2", "h2,h3"); got != "h2" {
		t.Errorf("intersection = %q, want h2", got)
	}
	if got := runLimited(t, "h1", "h2"); got != "" {
		t.Errorf("disjoint play and limit ran on %q, want nothing", got)
	}
}

func runLimited(t *testing.T, hosts, limit string) string {
	t.Helper()
	pb, err := Parse([]byte(`
- name: p
  hosts: ` + hosts + `
  gather_facts: false
  tasks:
    - {name: m, debug: {msg: x}}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(fiveHostInventory())
	e.Limit = limit
	// OnResult is called from one goroutine per host, concurrently, so
	// collecting without a lock loses entries at random — this test
	// flaked 2 runs in 5 before the mutex.
	var mu sync.Mutex
	var ran []string
	e.OnResult = func(r Result) {
		if r.Skipped {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, r.Host)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	// Sorted because hosts run concurrently and report as they finish:
	// which SET ran is the question here, not in what order.
	sort.Strings(ran)
	return strings.Join(ran, ",")
}
