package playbook

import (
	"context"
	"os"
	"regexp"
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

// TestGroupsFollowsAddHost: add_host and group_by CHANGE the inventory
// mid-play, and `groups` was built from it once at play start -- so a
// group created by add_host did not exist for the rest of the play.
// Measured against ansible-core 2.21.4:
//
//	groups['latecomers']  real: ['h2']  ours: <MISSING>
func TestGroupsFollowsAddHost(t *testing.T) {
	e := New(twoLocalHosts(t))
	var buf strings.Builder
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	pb, err := Parse([]byte(`
- name: p
  hosts: h1
  gather_facts: false
  tasks:
    - add_host: {name: h2, groups: latecomers}
    - debug: {msg: "GOT={{ groups['latecomers'] | default('<MISSING>') }}"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "GOT=['h2']") {
		t.Errorf("groups did not follow add_host:\n%s", buf.String())
	}
}

// ...and hostvars covers a host that did not exist when the play began.
func TestHostvarsCoversAnAddedHost(t *testing.T) {
	e := New(twoLocalHosts(t))
	var buf strings.Builder
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	pb, err := Parse([]byte(`
- name: p
  hosts: h1
  gather_facts: false
  tasks:
    - add_host: {name: newbie, ansible_connection: local, some_var: hello}
    - debug: {msg: "GOT={{ hostvars['newbie']['some_var'] | default('<MISSING>') }}"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "GOT=hello") {
		t.Errorf("hostvars did not cover a host added by add_host:\n%s", buf.String())
	}
}

// The rebuild is skipped while the inventory is unchanged, which is what
// keeps a playbook that never calls add_host -- almost all of them --
// from paying O(hosts x groups) per task per host.
//
// Asserting the CACHE rather than the output, because the output is
// identical either way: that is precisely why a broken cache would go
// unnoticed.
func TestTheInventoryViewIsCachedUntilItChanges(t *testing.T) {
	e := New(twoLocalHosts(t))
	ec := &execCtx{engine: e}

	g1, h1 := ec.inventoryView()
	g2, h2 := ec.inventoryView()
	if &g1 == nil || &g2 == nil {
		t.Fatal("no view")
	}
	// Same generation: the very same maps come back, not equal copies.
	if !sameMap(g1, g2) || !sameMap(h1, h2) {
		t.Error("the view was rebuilt although the inventory had not changed")
	}

	ec.invChanged()
	g3, _ := ec.inventoryView()
	if sameMap(g1, g3) {
		t.Error("the view was NOT rebuilt after the inventory changed")
	}
}

// sameMap reports whether two maps are the same object.
func sameMap(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	a["__probe"] = 1
	_, ok := b["__probe"]
	delete(a, "__probe")
	return ok
}

// TestGroupByUpdatesGroupsAndGroupNames: group_by puts a host in a group
// mid-play, so `groups` gains it and the host stops being "ungrouped".
// Measured against ansible-core 2.21.4:
//
//	groups['tagged']  real: ['h1']   ours: <NO-GROUP>
//	group_names       real: tagged   ours: ungrouped
func TestGroupByUpdatesGroupsAndGroupNames(t *testing.T) {
	e := New(twoLocalHosts(t))
	var buf strings.Builder
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	pb, err := Parse([]byte(`
- name: p
  hosts: h1
  gather_facts: false
  tasks:
    - group_by: {key: tagged}
    - debug: {msg: "GOT={{ groups['tagged'] | default('<NO-GROUP>') }} NAMES={{ group_names | sort | join(',') }}"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "GOT=['h1']") {
		t.Errorf("groups did not gain the group_by group:\n%s", out)
	}
	if !strings.Contains(out, "NAMES=tagged") {
		t.Errorf("group_names did not follow group_by:\n%s", out)
	}
}

// TestEveryInventoryMutationBumpsTheGeneration reads engine.go and fails
// if a call that MUTATES the inventory is not followed by invChanged().
//
// This is the check that would have caught the mistake it was written
// after: the edit adding the lock and the bump to runGroupBy used the
// wrong indentation, matched nothing, and changed no file -- so group_by
// mutated the inventory and nothing told the cache. The symptom was a
// group that did not exist, three layers away from the cause.
//
// A cache keyed on a counter is only as good as the places that bump it,
// and those are a hand-kept set.
func TestEveryInventoryMutationBumpsTheGeneration(t *testing.T) {
	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	// The inventory's own mutating methods, from go-ansible/inventory.
	mutators := regexp.MustCompile(`Inventory\.(AddHost|AddToGroup)\(`)
	locs := mutators.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		t.Fatal("found no inventory mutations at all; the check is broken and a pass would mean nothing")
	}
	for _, loc := range locs {
		// Look at the few lines that follow the call.
		end := loc[1] + 220
		if end > len(text) {
			end = len(text)
		}
		if !strings.Contains(text[loc[1]:end], "invChanged()") {
			t.Errorf("an inventory mutation at offset %d is not followed by invChanged(); "+
				"groups/hostvars/group_names would keep a stale view:\n\t%s",
				loc[0], firstLine(text[loc[0]:end]))
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
