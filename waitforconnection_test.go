package playbook

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	remoteexec "github.com/go-remoteexec/transport"
)

// go-ansible/playbook#195: the engine reported UNREACHABLE before
// wait_for_connection ever ran, so the one task whose whole purpose is to
// wait for a host that is not up yet could never do it. The module's own
// retry loop was already correct and never reached.
//
// Real retries on the CONTROLLER, in its action plugin
// (plugins/action/wait_for_connection.py:44-60), swallowing the connection
// failure for the whole timeout window.
//
// Every test here uses a timeout of 1-2 seconds. The default is 600, and a
// test that waited it out would be indistinguishable from a hang.

// failingConnect makes the first `failures` dials fail, then succeeds.
func failingConnect(e *Engine, failures int) (attempts *int) {
	var mu sync.Mutex
	n := 0
	e.Connect = func(_ context.Context, _ string, _ map[string]any) (remoteexec.Connection, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n <= failures {
			return nil, errors.New("connection refused")
		}
		return remoteexec.NewLocal(), nil
	}
	return &n
}

func TestWaitForConnectionRetriesInsteadOfReportingUnreachable(t *testing.T) {
	pb, err := Parse([]byte("" +
		"- hosts: all\n  gather_facts: false\n  tasks:\n" +
		"    - wait_for_connection: {timeout: 10, sleep: 1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := New(bareInventory(t))
	// The play-start dial fails, then the retry succeeds: exactly the
	// rebooting-host shape.
	attempts := failingConnect(e, 1)
	var got []Result
	e.OnResult = func(r Result) { got = append(got, r) }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no result reported")
	}
	last := got[len(got)-1]
	if last.Unreachable {
		t.Errorf("reported UNREACHABLE: %q — the task is meant to WAIT for exactly this", last.Msg)
	}
	if last.Failed {
		t.Errorf("failed: %q", last.Msg)
	}
	if *attempts < 2 {
		t.Errorf("dialled %d times; the retry never happened", *attempts)
	}
}

// When the host never comes back, the task must FAIL with real's own
// sentence rather than being reported unreachable — a different outcome from
// both the old behaviour and from success.
func TestWaitForConnectionTimesOutWithRealsSentence(t *testing.T) {
	pb, err := Parse([]byte("" +
		"- hosts: all\n  gather_facts: false\n  tasks:\n" +
		"    - wait_for_connection: {timeout: 1, sleep: 1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := New(bareInventory(t))
	failingConnect(e, 1000) // never comes back
	var got []Result
	e.OnResult = func(r Result) { got = append(got, r) }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no result reported")
	}
	last := got[len(got)-1]
	if !last.Failed {
		t.Error("a host that never returns must fail the task")
	}
	if !strings.Contains(last.Msg, "timed out waiting for connection port up") {
		t.Errorf("msg = %q, want real's own wording", last.Msg)
	}
	if !strings.Contains(last.Msg, "connection refused") {
		t.Errorf("msg = %q, want the last dial's error carried in it", last.Msg)
	}
}

// Control: every OTHER module still reports UNREACHABLE at once. The
// exemption must be for this one task, not a general softening of the
// unreachable path — which would hide a genuinely dead host behind a wait.
func TestAnotherModuleStillReportsUnreachableImmediately(t *testing.T) {
	pb, err := Parse([]byte("" +
		"- hosts: all\n  gather_facts: false\n  tasks:\n" +
		"    - command: echo x\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := New(bareInventory(t))
	attempts := failingConnect(e, 1000)
	var got []Result
	e.OnResult = func(r Result) { got = append(got, r) }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no result reported")
	}
	if !got[len(got)-1].Unreachable {
		t.Errorf("command should report UNREACHABLE, got %+v", got[len(got)-1])
	}
	// It must not have sat in a retry loop either.
	if *attempts > 3 {
		t.Errorf("dialled %d times for a plain command; it should not retry", *attempts)
	}
}

func TestWaitForConnectionTimingDefaultsAndClamp(t *testing.T) {
	// Real's own defaults (wait_for_connection.py:66-69).
	delay, sleep, timeout := waitForConnectionTiming(map[string]any{})
	if delay.Seconds() != 0 || sleep.Seconds() != 1 || timeout.Seconds() != 600 {
		t.Errorf("defaults = %v/%v/%v, want 0/1s/600s", delay, sleep, timeout)
	}
	// A string is accepted, as real's int() would.
	if _, _, tmo := waitForConnectionTiming(map[string]any{"timeout": "5"}); tmo.Seconds() != 5 {
		t.Errorf("timeout from a string = %v, want 5s", tmo)
	}
	// sleep: 0 is clamped. Real would spin a core for the whole timeout;
	// that is declined rather than reproduced, and the clamp is the one
	// place this knowingly differs.
	if _, s, _ := waitForConnectionTiming(map[string]any{"sleep": 0}); s.Seconds() != 1 {
		t.Errorf("sleep 0 = %v, want it clamped to 1s", s)
	}
}
