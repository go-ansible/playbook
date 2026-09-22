package playbook

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-ansible/inventory"
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

// TestPlayConnectionKeywords pins connection:, remote_user: and port:
// at play level, and the precedence real ansible-core applies: a HOST
// variable beats the play keyword.
//
// Ignoring connection: was not cosmetic — a play saying
// `connection: local` was connected to over SSH, so every task on it
// came back UNREACHABLE.
func TestPlayConnectionKeywords(t *testing.T) {
	tests := []struct {
		name     string
		hostVars map[string]any
		play     Play
		want     map[string]any
	}{{
		name: "the play's keywords apply",
		play: Play{Connection: "local", RemoteUser: "deploy", Port: 2222},
		want: map[string]any{"ansible_connection": "local", "ansible_user": "deploy", "ansible_port": 2222},
	}, {
		// Measured: a host var of ssh against a play keyword of local,
		// and the host var won.
		name:     "a host variable beats the play keyword",
		hostVars: map[string]any{"ansible_connection": "ssh"},
		play:     Play{Connection: "local"},
		want:     map[string]any{"ansible_connection": "ssh"},
	}, {
		name:     "each key is decided on its own",
		hostVars: map[string]any{"ansible_user": "root"},
		play:     Play{Connection: "local", RemoteUser: "deploy"},
		want:     map[string]any{"ansible_connection": "local", "ansible_user": "root"},
	}, {
		name: "a play with no keywords changes nothing",
		play: Play{},
		want: map[string]any{},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := map[string]any{}
			for k, v := range tt.hostVars {
				in[k] = v
			}
			got := withPlayConnection(tt.play, in)
			for k, want := range tt.want {
				if got[k] != want {
					t.Errorf("%s = %#v, want %#v", k, got[k], want)
				}
			}
			// The caller's map is never modified in place.
			if len(tt.hostVars) != len(in) {
				t.Errorf("the input map was mutated: %v", in)
			}
		})
	}
}

// End to end: a play whose only route to the host is `connection: local`
// runs, where it used to come back UNREACHABLE.
func TestPlayConnectionLocalRuns(t *testing.T) {
	inv, err := inventory.ParseYAML([]byte("all:\n  hosts:\n    not-a-real-host:\n"))
	if err != nil {
		t.Fatal(err)
	}
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  connection: local
  tasks:
    - {name: t, command: echo ran}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(inv)
	var mu sync.Mutex
	var unreachable, ran bool
	e.OnResult = func(r Result) {
		mu.Lock()
		defer mu.Unlock()
		unreachable = unreachable || r.Unreachable
		ran = ran || (r.Task == "t" && !r.Failed && !r.Skipped)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if unreachable {
		t.Error("connection: local was ignored, so the host was tried over SSH")
	}
	if !ran {
		t.Error("the task did not run")
	}
}

// TestCheckModeTriState pins check_mode: at play and task level. The
// tri-state matters in BOTH directions, and this port honoured neither:
// a play saying `check_mode: true` wrote the file it promised not to,
// and a task saying `check_mode: false` had no way to run for real
// under --check.
func TestCheckModeTriState(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name string
		flag bool  // the --check flag
		play *bool // the play's check_mode:
		task *bool // the task's own
		want bool  // is this task a dry run?
	}{
		{"nothing set", false, nil, nil, false},
		{"--check alone", true, nil, nil, true},
		// A play can ask for a dry run without the flag.
		{"play true", false, &yes, nil, true},
		// And can force a real run despite the flag.
		{"play false under --check", true, &no, nil, false},
		// A task overrides its play either way.
		{"task true under a false play", false, &no, &yes, true},
		{"task false under a true play", false, &yes, &no, false},
		// The case real playbooks use: read state for real while the
		// rest of the run is predicting.
		{"task false under --check", true, nil, &no, false},
		{"task true without the flag", false, nil, &yes, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(localhostInventory())
			e.CheckMode = tt.flag
			ec := &execCtx{engine: e, play: Play{CheckMode: tt.play}}
			if got := ec.inCheckMode(Task{CheckMode: tt.task}); got != tt.want {
				t.Errorf("inCheckMode = %v, real ansible-core gives %v", got, tt.want)
			}
		})
	}
}

// End to end: a play that says check_mode: true must not write.
func TestPlayCheckModeWritesNothing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  check_mode: true
  tasks:
    - {name: t, copy: {content: "written\n", dest: ` + target + `}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(localhostInventory()).RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("check_mode: true on the play was ignored — the file was written")
	}
}
