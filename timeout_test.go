package playbook

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestTaskTimeout pins the task-level timeout: keyword. Every value
// below was measured against ansible-core 2.21.4.
func TestTaskTimeout(t *testing.T) {
	// A timeout that IS reached discards the module's result entirely:
	// changed false, no rc, no cmd, and real's own wording.
	rs := runTasks(t, "    - command: sleep 3\n      timeout: 1\n      ignore_errors: true\n")
	if len(rs) != 1 {
		t.Fatalf("got %d results, want 1", len(rs))
	}
	r := rs[0]
	if !r.Failed {
		t.Error("a task past its timeout must fail")
	}
	if r.Changed {
		t.Error("real reports changed=false on a timeout, however far the module got")
	}
	if want := "Task failed: Timed out after 1 second(s)."; r.Msg != want {
		t.Errorf("msg = %q, real gives %q", r.Msg, want)
	}
	td, ok := r.Extra["timedout"].(map[string]any)
	if !ok {
		t.Fatalf("no timedout key; real carries one: %#v", r.Extra)
	}
	if td["period"] != 1 {
		t.Errorf("timedout.period = %#v, want 1", td["period"])
	}
	if td["frame"] != timeoutFrame {
		t.Errorf("timedout.frame = %#v, want real's own text", td["frame"])
	}
	// The module's own keys are GONE, which is the point of the
	// keyword: nothing half-done is reported.
	for _, k := range []string{"rc", "cmd", "stdout"} {
		if _, present := r.Extra[k]; present {
			t.Errorf("a timed-out result carries %q; real discards the module's result", k)
		}
	}

	// A timeout that is NOT reached leaves no trace at all.
	r = runTasks(t, "    - command: echo quick\n      timeout: 5\n")[0]
	if r.Failed {
		t.Error("a task inside its timeout must not fail")
	}
	if _, present := r.Extra["timedout"]; present {
		t.Error("a task that did not time out carries no timedout key")
	}
	if r.Extra["stdout"] != "quick" {
		t.Errorf("stdout = %#v, want quick -- the module's own result survives", r.Extra["stdout"])
	}

	// timeout: 0 means NO limit. Measured: a two-second sleep finishes.
	start := time.Now()
	r = runTasks(t, "    - command: sleep 2\n      timeout: 0\n")[0]
	if r.Failed {
		t.Errorf("timeout: 0 must not time anything out: %v", r.Msg)
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Errorf("elapsed = %v; the sleep did not actually run, so this proves nothing", elapsed)
	}
}

// TestTaskTimeoutIsPerIteration pins that a loop gets the timeout per
// ITEM rather than for the whole task: three one-second sleeps under
// timeout: 2 all pass.
func TestTaskTimeoutIsPerIteration(t *testing.T) {
	rs := runTasks(t, "    - command: sleep 1\n      loop: [1, 2, 3]\n      timeout: 2\n")
	for i, r := range rs {
		if r.Failed {
			t.Errorf("iteration %d failed (%q); the timeout is per item, not for the whole loop", i, r.Msg)
		}
	}
}

// TestThrottleLimitsOneTask pins throttle:, which this port used to
// REFUSE outright -- a playbook using it did not run at all. With five
// hosts, five forks and throttle: 1, the task serialises, so five
// half-second sleeps take at least two and a half seconds; without the
// cap they overlap and take about one.
func TestThrottleLimitsOneTask(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: throttled
      command: sleep 0.5
      throttle: 1
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(fiveHosts(t))
	e.Forks = 5
	start := time.Now()
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Errorf("elapsed = %v; five serialised half-second sleeps take at least 2.5s, so throttle is not capping anything", elapsed)
	}
}

// TestThrottleIsNotRefused is the parse-level half: the keyword used to
// kill the whole playbook.
func TestThrottleIsNotRefused(t *testing.T) {
	if _, err := Parse([]byte("- {name: p, hosts: all, gather_facts: false, tasks: [{name: t, debug: {msg: x}, throttle: 2}]}\n")); err != nil {
		if strings.Contains(err.Error(), "does not support") {
			t.Fatalf("throttle is still refused: %v", err)
		}
		t.Fatal(err)
	}
}
