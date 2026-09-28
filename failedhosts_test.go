package playbook

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-ansible/inventory"
)

// fiveHosts is the shape the multi-host measurements were taken on.
func fiveHosts(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.ParseYAML([]byte(`
all:
  hosts:
    h1: {ansible_connection: local}
    h2: {ansible_connection: local}
    h3: {ansible_connection: local}
    h4: {ansible_connection: local}
    h5: {ansible_connection: local}
`))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func runOn(t *testing.T, inv *inventory.Inventory, src string) []Result {
	t.Helper()
	pb, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var out []Result
	e := New(inv)
	e.OnResult = func(r Result) { out = append(out, r) }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAFailedHostLeavesTheRun pins real's host-removal rule: a host
// that fails is dropped for the REST of the playbook. Measured -- a
// play after one where h1 failed runs on h2..h5 only, and the recap
// shows h1 doing nothing more. This port kept running it, so a host
// that had just failed its own prerequisites received everything
// after them.
func TestAFailedHostLeavesTheRun(t *testing.T) {
	rs := runOn(t, fiveHosts(t), `
- hosts: all
  gather_facts: false
  tasks:
    - name: fails on h1
      command: "false"
      when: "inventory_hostname == 'h1'"
- hosts: all
  gather_facts: false
  tasks:
    - name: later
      debug: msg="later"
`)
	var later []string
	for _, r := range rs {
		if r.Task == "later" {
			later = append(later, r.Host)
		}
	}
	if len(later) != 4 {
		t.Fatalf("the later play ran on %v, real runs it on the four hosts that did not fail", later)
	}
	for _, h := range later {
		if h == "h1" {
			t.Error("h1 failed in the first play and must not receive the second")
		}
	}
}

// TestAnyErrorsFatalStopsThePlaybook pins the stronger brake: it ends
// the RUN, not just its play. Measured -- the second play is never
// bannered at all.
func TestAnyErrorsFatalStopsThePlaybook(t *testing.T) {
	rs := runOn(t, fiveHosts(t), `
- hosts: all
  gather_facts: false
  any_errors_fatal: true
  tasks:
    - name: fails on h1
      command: "false"
      when: "inventory_hostname == 'h1'"
- hosts: all
  gather_facts: false
  tasks:
    - name: must not run
      debug: msg="continued"
`)
	for _, r := range rs {
		if r.Task == "must not run" {
			t.Fatalf("the playbook continued to %q on %s; any_errors_fatal ends the RUN", r.Task, r.Host)
		}
	}
}

// TestMaxFailPercentageUnderItsThreshold is the case that does NOT
// trip: 1 of 5 is 20%, under 50, so nothing stops. It is here because
// an earlier reading of exactly this run concluded that
// max_fail_percentage stops only its play -- it stops NOTHING here,
// and TestMaxFailPercentageTripped below shows what it does when it
// actually trips.
func TestMaxFailPercentageUnderItsThreshold(t *testing.T) {
	rs := runOn(t, fiveHosts(t), `
- hosts: all
  gather_facts: false
  max_fail_percentage: 50
  tasks:
    - name: fails on h1
      command: "false"
      when: "inventory_hostname == 'h1'"
    - name: same play
      debug: msg="same play"
- hosts: all
  gather_facts: false
  tasks:
    - name: next play
      debug: msg="next play"
`)
	counts := map[string]int{}
	for _, r := range rs {
		counts[r.Task]++
	}
	if counts["same play"] != 4 {
		t.Errorf("the rest of the play ran on %d hosts, real continues on the four that did not fail", counts["same play"])
	}
	if counts["next play"] != 4 {
		t.Errorf("the next play ran on %d hosts, real runs it on four -- the brake never tripped", counts["next play"])
	}
}

// TestMaxFailPercentageTripped is the other half, and the one that
// distinguishes the two readings. Measured: 1 of 5 failing under
// max_fail_percentage: 10 is 20% > 10%, and real skips the rest of the
// play AND the next play entirely.
func TestMaxFailPercentageTripped(t *testing.T) {
	rs := runOn(t, fiveHosts(t), `
- hosts: all
  gather_facts: false
  max_fail_percentage: 10
  tasks:
    - name: fails on h1
      command: "false"
      when: "inventory_hostname == 'h1'"
    - name: same play
      debug: msg="same play"
- hosts: all
  gather_facts: false
  tasks:
    - name: next play
      debug: msg="next play"
`)
	for _, r := range rs {
		if r.Task == "same play" || r.Task == "next play" {
			t.Errorf("%q ran on %s; a tripped max_fail_percentage ends the RUN", r.Task, r.Host)
		}
	}
}

// TestAFatalBrakeStopsHostsThatNeverFailed is the discriminating input
// for "ends the run" versus "removes the failed hosts": the second
// play targets hosts that were never in the first one, so dropping
// failed hosts alone would let it run. Measured: real does not banner
// it at all.
func TestAFatalBrakeStopsHostsThatNeverFailed(t *testing.T) {
	rs := runOn(t, fiveHosts(t), `
- hosts: h1,h2
  gather_facts: false
  any_errors_fatal: true
  tasks:
    - name: fails on h1
      command: "false"
      when: "inventory_hostname == 'h1'"
- hosts: h4,h5
  gather_facts: false
  tasks:
    - name: other hosts
      debug: msg="ran"
`)
	for _, r := range rs {
		if r.Task == "other hosts" {
			t.Errorf("a play on %s ran after any_errors_fatal; that host never failed, so only ending the RUN can stop it", r.Host)
		}
	}
}

// TestAnIgnoredFailureIsNotFatal pins the boundary between them: an
// ignored failure does NOT trip any_errors_fatal. Measured -- the play
// continues and so does the playbook.
func TestAnIgnoredFailureIsNotFatal(t *testing.T) {
	rs := runOn(t, fiveHosts(t), `
- hosts: all
  gather_facts: false
  any_errors_fatal: true
  tasks:
    - name: fails but ignored
      command: "false"
      when: "inventory_hostname == 'h1'"
      ignore_errors: true
    - name: reached
      debug: msg="reached"
- hosts: all
  gather_facts: false
  tasks:
    - name: second play
      debug: msg="second play"
`)
	counts := map[string]int{}
	for _, r := range rs {
		counts[r.Task]++
	}
	if counts["reached"] != 5 {
		t.Errorf("the next task ran on %d hosts, real reaches all five", counts["reached"])
	}
	if counts["second play"] != 5 {
		t.Errorf("the second play ran on %d hosts, real runs it on all five -- an ignored failure is not a failure", counts["second play"])
	}
}

// TestPlayHostVariables pins the three, which this port had as two.
// Measured under serial: 2 over five hosts:
//
//	batch=['h1','h2']  hosts=[all five]  all=[all five]
//
// and, after a failure, hosts shrinks while all does not.
func TestPlayHostVariables(t *testing.T) {
	rs := runOn(t, fiveHosts(t), `
- hosts: all
  gather_facts: false
  serial: 2
  tasks:
    - name: report
      debug: msg="batch={{ ansible_play_batch | length }} hosts={{ ansible_play_hosts | length }} all={{ ansible_play_hosts_all | length }}"
      run_once: true
`)
	var got []string
	for _, r := range rs {
		if r.Task == "report" {
			got = append(got, r.Msg)
		}
	}
	want := []string{
		"batch=2 hosts=5 all=5",
		"batch=2 hosts=5 all=5",
		"batch=1 hosts=5 all=5",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d batches %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("batch %d = %q, real gives %q -- the batch is NOT the play's host list", i, got[i], want[i])
		}
	}
}

func TestPlayHostsShrinksAfterAFailure(t *testing.T) {
	rs := runOn(t, fiveHosts(t), `
- hosts: all
  gather_facts: false
  tasks:
    - name: before
      debug: msg="hosts={{ ansible_play_hosts | length }} all={{ ansible_play_hosts_all | length }}"
      run_once: true
    - name: h1 fails
      command: "false"
      when: "inventory_hostname == 'h1'"
    - name: after
      debug: msg="hosts={{ ansible_play_hosts | length }} all={{ ansible_play_hosts_all | length }}"
      run_once: true
`)
	got := map[string]string{}
	for _, r := range rs {
		if r.Task == "before" || r.Task == "after" {
			got[r.Task] = r.Msg
		}
	}
	if got["before"] != "hosts=5 all=5" {
		t.Errorf("before = %q, real gives %q", got["before"], "hosts=5 all=5")
	}
	if got["after"] != "hosts=4 all=5" {
		t.Errorf("after = %q, real gives %q -- ansible_play_hosts shrinks, ansible_play_hosts_all does not", got["after"], "hosts=4 all=5")
	}
}

// TestMaxFailPercentageBoundary pins the comparison itself. The engine
// compares failed/batch STRICTLY GREATER than pct/100, and a comment
// has long asserted that "20% failed with max_fail_percentage: 20
// CONTINUES, and 19 does not" -- asserted, and until now not tested: a
// neuter changing > to >= passed, because no case sat on the boundary.
//
// Measured, 1 of 5 failing:
//
//	max_fail_percentage: 20  -> the play and the next one continue
//	max_fail_percentage: 19  -> both stop
func TestMaxFailPercentageBoundary(t *testing.T) {
	for _, tc := range []struct {
		pct       int
		wantAfter bool
	}{
		{20, true},
		{19, false},
	} {
		t.Run(fmt.Sprintf("pct=%d", tc.pct), func(t *testing.T) {
			rs := runOn(t, fiveHosts(t), fmt.Sprintf(`
- hosts: all
  gather_facts: false
  max_fail_percentage: %d
  tasks:
    - name: fails on h1
      command: "false"
      when: "inventory_hostname == 'h1'"
    - name: same play
      debug: msg="same play"
- hosts: all
  gather_facts: false
  tasks:
    - name: next play
      debug: msg="next play"
`, tc.pct))
			ran := false
			for _, r := range rs {
				if r.Task == "same play" || r.Task == "next play" {
					ran = true
				}
			}
			if ran != tc.wantAfter {
				if tc.wantAfter {
					t.Errorf("nothing ran after the failure; 20%% is NOT over a threshold of %d", tc.pct)
				} else {
					t.Errorf("work ran after the failure; 20%% IS over a threshold of %d", tc.pct)
				}
			}
		})
	}
}
