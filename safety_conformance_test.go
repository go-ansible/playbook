package playbook

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The play every case below runs: h1 fails, the rest succeed, and a
// second task records which hosts got that far.
func safetyPlaybook(keywords string) string {
	return `
- name: p
  hosts: all
  gather_facts: false
` + keywords + `
  tasks:
    - name: fail on h1 only
      command: sh -c "test '{{ inventory_hostname }}' != h1"
    - name: after
      debug: {msg: "reached-{{ inventory_hostname }}"}
`
}

// TestSafetyLimitsMatchRealAnsible pins any_errors_fatal and
// max_fail_percentage, which this port PARSED and then ignored — the
// worst shape for a safety brake. A play saying "if any host fails,
// stop" carried on deploying to the rest of the fleet, and said
// nothing.
//
// Every expectation is measured from real ansible-core 2.21.4 on five
// hosts where exactly one fails, i.e. 20%.
func TestSafetyLimitsMatchRealAnsible(t *testing.T) {
	tests := []struct {
		name     string
		keywords string
		reached  string
	}{{
		name:    "no limit: the healthy hosts carry on",
		reached: "h2,h3,h4,h5",
	}, {
		name:     "any_errors_fatal stops everyone",
		keywords: "  any_errors_fatal: true",
		reached:  "",
	}, {
		name:     "max_fail_percentage: 0 stops on the first failure",
		keywords: "  max_fail_percentage: 0",
		reached:  "",
	}, {
		// Strictly greater: 20% failed against a limit of 20 CONTINUES.
		name:     "max_fail_percentage at the line continues",
		keywords: "  max_fail_percentage: 20",
		reached:  "h2,h3,h4,h5",
	}, {
		name:     "max_fail_percentage just under stops",
		keywords: "  max_fail_percentage: 19",
		reached:  "",
	}, {
		// The brake has to stop the PLAY, not just the batch: a rolling
		// update that gives up must not roll on.
		name:     "serial with any_errors_fatal stops the whole play",
		keywords: "  serial: 2\n  any_errors_fatal: true",
		reached:  "",
	}, {
		name:     "serial with max_fail_percentage stops the whole play",
		keywords: "  serial: 2\n  max_fail_percentage: 0",
		reached:  "",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pb, err := Parse([]byte(safetyPlaybook(tt.keywords)))
			if err != nil {
				t.Fatal(err)
			}
			e := New(fiveHostInventory())
			var mu sync.Mutex
			var reached []string
			e.OnResult = func(r Result) {
				if r.Skipped || r.Failed || !strings.HasPrefix(r.Msg, "reached-") {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				reached = append(reached, r.Host)
			}
			if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
				t.Fatal(err)
			}
			sort.Strings(reached)
			if got := strings.Join(reached, ","); got != tt.reached {
				t.Errorf("reached %q, real ansible-core reaches %q", got, tt.reached)
			}
		})
	}
}

// A task's own any_errors_fatal overrides the play's absence of one.
func TestTaskLevelAnyErrorsFatal(t *testing.T) {
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - name: fail on h1 only
      command: sh -c "test '{{ inventory_hostname }}' != h1"
      any_errors_fatal: true
    - name: after
      debug: {msg: "reached-{{ inventory_hostname }}"}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(fiveHostInventory())
	var mu sync.Mutex
	var reached int
	e.OnResult = func(r Result) {
		if strings.HasPrefix(r.Msg, "reached-") && !r.Skipped && !r.Failed {
			mu.Lock()
			reached++
			mu.Unlock()
		}
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if reached != 0 {
		t.Errorf("%d hosts continued past a task marked any_errors_fatal", reached)
	}
}

// max_fail_percentage must survive being absent, which is NOT the same
// as being zero: absent means no limit, zero means any failure stops
// everything.
func TestMaxFailPercentageAbsentIsNotZero(t *testing.T) {
	for _, tt := range []struct {
		yaml string
		want string
	}{
		{"", "<nil>"},
		{"  max_fail_percentage: 0", "0"},
		{"  max_fail_percentage: 30", "30"},
		{`  max_fail_percentage: "30"`, "30"},
	} {
		pb, err := Parse([]byte(safetyPlaybook(tt.yaml)))
		if err != nil {
			t.Fatal(err)
		}
		got := "<nil>"
		if p := pb[0].MaxFailPercentage; p != nil {
			got = fmt.Sprintf("%g", *p)
		}
		if got != tt.want {
			t.Errorf("%q parsed to %s, want %s", tt.yaml, got, tt.want)
		}
	}
}
