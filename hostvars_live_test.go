package playbook

import (
	"context"
	"strings"
	"testing"
)

// hostvarsRun renders `expr` on h1 only and returns what it printed.
func hostvarsRun(t *testing.T, strategy, expr string, gather bool) string {
	t.Helper()
	e := New(twoLocalHosts(t))
	var buf strings.Builder
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}

	src := "- name: p\n  hosts: all\n  gather_facts: " + map[bool]string{true: "true", false: "false"}[gather] + "\n"
	if strategy != "" {
		src += "  strategy: " + strategy + "\n"
	}
	src += `  tasks:
    - name: mark
      set_fact:
        my_mark: "mark-of-{{ inventory_hostname }}"
    - name: read
      debug:
        msg: "GOT=` + expr + `"
      when: inventory_hostname == 'h1'
`
	pb, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestHostvarsSeesAnotherHostsSetFact is the defect: hostvars was built
// ONCE per play from the inventory, so a variable another host set was
// invisible. Measured against ansible-core 2.21.4, same playbook:
//
//	real:  h2.my_mark=mark-of-h2
//	ours:  h2.my_mark=<MISSING>
//
// Templating one host's configuration from another's variables is one of
// the most common things a real playbook does.
func TestHostvarsSeesAnotherHostsSetFact(t *testing.T) {
	out := hostvarsRun(t, "", "{{ hostvars['h2']['my_mark'] | default('<MISSING>') }}", false)
	if !strings.Contains(out, "GOT=mark-of-h2") {
		t.Errorf("h1 could not see h2's set_fact through hostvars:\n%s", out)
	}
}

// Gathered facts travel the same road, and needed their own fix: the
// first publish happens at the END of a task, so a play that gathers
// facts and reads hostvars in its FIRST task saw the inventory's view.
// Every host publishes once before the first task now.
func TestHostvarsSeesAnotherHostsGatheredFacts(t *testing.T) {
	e := New(twoLocalHosts(t))
	var buf strings.Builder
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: true
  tasks:
    - name: read
      debug:
        msg: "GOT={{ hostvars['h2']['ansible_system'] | default('<MISSING>') }}"
      when: inventory_hostname == 'h1'
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "GOT=<MISSING>") {
		t.Errorf("h1 could not see h2's gathered facts in the FIRST task:\n%s", buf.String())
	}
}

// TestHostvarsAcrossStrategies exists for -race: under free and
// host_pinned every host runs in its own goroutine, which is where a
// shared hostvars would race.
//
// It deliberately does NOT assert the value under those two. ⛔ free
// gives no cross-host ordering -- that is what it is FOR -- so h1 can
// reach the reading task before h2 has finished the task that sets the
// fact, and then <MISSING> is the correct answer. An earlier version
// asserted mark-of-h2 for all three and passed on this machine, where
// h2 always won the race; CI caught it on ppc64le under qemu, which is
// slow enough to lose it. A test that depends on who wins a race passes
// until it reaches a slower machine.
//
// linear has the per-task barrier, so the value IS guaranteed there, and
// TestHostvarsSeesAnotherHostsSetFact asserts it.
func TestHostvarsAcrossStrategies(t *testing.T) {
	for _, strategy := range []string{"linear", "free", "host_pinned"} {
		t.Run(strategy, func(t *testing.T) {
			out := hostvarsRun(t, strategy, "{{ hostvars['h2']['my_mark'] | default('<MISSING>') }}", false)
			switch {
			case strategy == "linear" && !strings.Contains(out, "GOT=mark-of-h2"):
				t.Errorf("linear has a per-task barrier, so h2's fact must be there:\n%s", out)
			case !strings.Contains(out, "GOT=mark-of-h2") && !strings.Contains(out, "GOT=<MISSING>"):
				t.Errorf("neither the fact nor a clean miss -- hostvars produced something else:\n%s", out)
			}
		})
	}
}

// A host the play does not include still resolves, from the inventory's
// own view -- real's hostvars covers the whole inventory, not the batch.
func TestHostvarsCoversHostsOutsideThePlay(t *testing.T) {
	e := New(twoLocalHosts(t))
	var buf strings.Builder
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	pb, err := Parse([]byte(`
- name: p
  hosts: h1
  gather_facts: false
  tasks:
    - name: read
      debug:
        msg: "GOT={{ hostvars['h2']['ansible_connection'] | default('<MISSING>') }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "GOT=local") {
		t.Errorf("a host outside the play did not resolve through hostvars:\n%s", buf.String())
	}
}

// The snapshot drops `hostvars` itself. Without that, each host's entry
// would carry a copy of every host's variables, and the whole structure
// would grow by a factor of the host count on every task.
func TestASnapshotDoesNotCarryHostvars(t *testing.T) {
	out := hostvarsRun(t, "",
		"{{ hostvars['h2']['hostvars'] is defined }}", false)
	if !strings.Contains(out, "GOT=False") {
		t.Errorf("hostvars['h2']['hostvars'] is defined; the snapshot nests:\n%s", out)
	}
}
