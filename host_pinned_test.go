package playbook

import (
	"context"
	"strings"
	"testing"
)

// TestHostPinnedRunsOneHostAtATime pins the ONE thing that separates
// host_pinned from free, measured against ansible-core 2.21.4 over two
// hosts at -f 1:
//
//	linear       one[h1 h2] two[h1 h2] three[h1 h2]
//	free         one[h1 h2] two[h1 h2] three[h1 h2]   (free IS linear here)
//	host_pinned  one[h1] two[h1] three[h1] one[h2] two[h2] three[h2]
//
// A host does not start until a fork slot is free and keeps it until the
// play is done, so "the number of hosts with an active play does not
// exceed the number of forks" -- real's own wording for the strategy.
func TestHostPinnedRunsOneHostAtATime(t *testing.T) {
	e := New(twoLocalHosts(t))
	e.Forks = 1
	var buf strings.Builder
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}

	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  strategy: host_pinned
  tasks:
    - {name: one, command: echo 1}
    - {name: two, command: echo 2}
    - {name: three, command: echo 3}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}

	// The host order of the result lines IS the property: each host's
	// three results must be contiguous.
	var hosts []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "changed: [") {
			hosts = append(hosts, strings.TrimSuffix(strings.TrimPrefix(line, "changed: ["), "]"))
		}
	}
	want := []string{"h1", "h1", "h1", "h2", "h2", "h2"}
	if len(hosts) != len(want) {
		t.Fatalf("got %d result lines, want %d: %v", len(hosts), len(want), hosts)
	}
	for i := range want {
		if hosts[i] != want[i] {
			t.Fatalf("host order %v, want %v -- a host must finish before the next starts", hosts, want)
		}
	}
}

// The control that tells this test apart from one that would pass under
// ANY strategy: the same playbook under free at one fork interleaves,
// because free collapses to linear there. If this ever matches the
// pinned order, the test above is asserting nothing.
func TestFreeAtOneForkInterleavesWhereHostPinnedDoesNot(t *testing.T) {
	e := New(twoLocalHosts(t))
	e.Forks = 1
	var buf strings.Builder
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}

	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  strategy: free
  tasks:
    - {name: one, command: echo 1}
    - {name: two, command: echo 2}
    - {name: three, command: echo 3}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "changed: [") {
			hosts = append(hosts, strings.TrimSuffix(strings.TrimPrefix(line, "changed: ["), "]"))
		}
	}
	if len(hosts) >= 2 && hosts[0] == hosts[1] {
		t.Errorf("free at one fork ran a host twice in a row (%v); it should interleave, "+
			"and if it does not, the host_pinned test proves nothing", hosts)
	}
}

// host_pinned is accepted by the parser; a strategy with no
// implementation still is not. `debug` is real's linear plus its
// interactive debugger, which this port does not have -- running it as
// linear would silently drop the only thing it asks for.
func TestStrategyNamesAcceptedAndRefused(t *testing.T) {
	for _, tc := range []struct {
		strategy string
		ok       bool
	}{
		{"linear", true},
		{"free", true},
		{"host_pinned", true},
		{"debug", false},
		{"nonesuch", false},
	} {
		_, err := Parse([]byte("- {name: p, hosts: all, gather_facts: false, strategy: " + tc.strategy +
			", tasks: [{name: t, debug: {msg: x}}]}\n"))
		if tc.ok && err != nil {
			t.Errorf("strategy %q was refused: %v", tc.strategy, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("strategy %q parsed; it has no implementation and must be refused", tc.strategy)
			} else if !strings.Contains(err.Error(), tc.strategy) {
				t.Errorf("the refusal of %q does not name it: %v", tc.strategy, err)
			}
		}
	}
}
