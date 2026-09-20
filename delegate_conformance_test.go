package playbook

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/go-ansible/inventory"
)

// TestRunOnceAlwaysPicksTheFirstHost pins run_once to the play's FIRST
// host. Real ansible-core ran it on h1 in four runs out of four; this
// port ran it on h2,h2,h2,h1, because the active-host list was rebuilt
// in goroutine completion order rather than inventory order.
//
// It matters beyond tidiness: a run_once task that registers a variable
// would otherwise record a different inventory_hostname from one run to
// the next.
func TestRunOnceAlwaysPicksTheFirstHost(t *testing.T) {
	pb, err := Parse([]byte(`
- name: once
  hosts: all
  gather_facts: false
  tasks:
    - name: warmup
      debug: {msg: "{{ inventory_hostname }}"}
    - name: single
      debug: {msg: "{{ inventory_hostname }}"}
      run_once: true
`))
	if err != nil {
		t.Fatal(err)
	}

	// Repeated, because the defect this pins was a race: one run of the
	// old code could pick the right host by luck.
	for i := 0; i < 20; i++ {
		e := New(fiveHostInventory())
		var ranOn string
		e.OnResult = func(r Result) {
			if r.Task == "single" {
				ranOn = r.Host
			}
		}
		if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
			t.Fatal(err)
		}
		if ranOn != "h1" {
			t.Fatalf("run %d: run_once ran on %q, real ansible-core always uses the first host (h1)", i, ranOn)
		}
	}
}

// TestDelegateToIsReportedInTheResultLine pins how real ansible-core
// names the target of a delegated task. All four shapes below were
// measured from a real run.
func TestDelegateToIsReportedInTheResultLine(t *testing.T) {
	tests := []struct {
		name   string
		result Result
		want   string
	}{{
		name:   "ok delegated",
		result: Result{Host: "h1", Delegate: "h5", Task: "t"},
		want:   "ok: [h1 -> h5]",
	}, {
		name:   "changed delegated",
		result: Result{Host: "h1", Delegate: "h5", Task: "t", Changed: true},
		want:   "changed: [h1 -> h5]",
	}, {
		name:   "failure delegated",
		result: Result{Host: "h1", Delegate: "bad5", Task: "t", Failed: true, Unreachable: true},
		want:   "fatal: [h1 -> bad5]: UNREACHABLE!",
	}, {
		// Real ansible-core prints a bare "skipping: [h1]" for a
		// delegated task skipped by when:, because it never connected.
		name:   "skipped delegated is NOT annotated",
		result: Result{Host: "h1", Delegate: "h5", Task: "t", Skipped: true},
		want:   "skipping: [h1]",
	}, {
		name:   "undelegated is unchanged",
		result: Result{Host: "h1", Task: "t"},
		want:   "ok: [h1]",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			NewDefaultCallback(&buf, false).OnTaskResult(tt.result)
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("got:\n%s\nwant it to contain %q", buf.String(), tt.want)
			}
		})
	}
}

// A delegated task really does run against the delegate's connection,
// not the origin host's — proved by delegating to a host that cannot be
// reached, which must fail.
func TestDelegateToActuallyChangesTheConnection(t *testing.T) {
	pb, err := Parse([]byte(`
- name: deleg
  hosts: h1
  gather_facts: false
  tasks:
    - name: must fail
      shell: echo hello
      delegate_to: unreachable
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(unreachableDelegateInventory())
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err == nil && (rr == nil || !rr.Failed()) {
		t.Fatal("delegating to an unreachable host succeeded, so delegate_to was ignored")
	}
}

func unreachableDelegateInventory() *inventory.Inventory {
	inv, err := inventory.ParseYAML([]byte(`
all:
  hosts:
    h1: {ansible_connection: local}
    unreachable:
      ansible_connection: ssh
      ansible_host: 192.0.2.1
      ansible_port: 1
`))
	if err != nil {
		panic(err)
	}
	return inv
}
