package playbook

import (
	"github.com/go-ansible/vars"
)

// hostvars has to be LIVE: `hostvars['web1']['ansible_default_ipv4']`
// is how a playbook templates one host's configuration from another's
// facts, and it is one of the most common things a real playbook does.
//
// This port built it ONCE per play, from the inventory, so a fact
// gathered or a variable set by another host was invisible. Measured
// against ansible-core 2.21.4:
//
//	hostvars['h2']['my_mark']        real: mark-of-h2   ours: <MISSING>
//	hostvars['h2']['ansible_system'] real: Darwin       ours: <MISSING>
//
// The shape here is "publish a snapshot, read everyone's snapshots",
// rather than reading another host's vars.Context directly, because that
// Context is NOT synchronised and under free/host_pinned every host runs
// in its own goroutine. Each host writes only its OWN snapshot and only
// its own Context; the snapshots are the one shared thing, and they are
// replaced wholesale rather than mutated, so a reader holding one is
// holding something nobody will write to.
//
// The timing is deliberate: a host publishes at the END of each of its
// tasks and reads everyone's snapshots at the START of the next. Under
// linear that makes hostvars reflect exactly the tasks that have
// finished, which is what real shows.

// publishVars stores this host's current variables for other hosts to
// read. The map must not be modified afterwards.
func (st *hostState) publishVars(m map[string]any) {
	st.snapMu.Lock()
	st.snapshot = m
	st.snapMu.Unlock()
}

// peekVars returns this host's last published variables.
func (st *hostState) peekVars() map[string]any {
	st.snapMu.Lock()
	defer st.snapMu.Unlock()
	return st.snapshot
}

// liveHostvars is every host's current variables, keyed by host name:
// the published snapshot for a host in this play, and the inventory's
// own view for every other host the inventory knows.
//
// The static entries come from the map built at play start, so a host
// outside the play still resolves -- real's hostvars covers the whole
// inventory, not just the play's batch.
func (ec *execCtx) liveHostvars(static map[string]any) map[string]any {
	out := make(map[string]any, len(static)+len(ec.states))
	for k, v := range static {
		out[k] = v
	}
	for name, st := range ec.states {
		if snap := st.peekVars(); snap != nil {
			out[name] = snap
		}
	}
	return out
}

// refreshHostvars gives this host's Context the current view of every
// other host. Called by the host's OWN goroutine, writing only to its
// own Context.
func (ec *execCtx) refreshHostvars(st *hostState) {
	groups, static := ec.inventoryView()
	st.vc.SetVar(vars.Inventory, "groups", groups)
	st.vc.SetVar(vars.Inventory, "hostvars", ec.liveHostvars(static))

	// group_names is the third thing derived from the inventory, and
	// group_by changes it mid-play: a host that joins a group stops
	// being "ungrouped". Measured against ansible-core 2.21.4 after
	// `group_by: {key: tagged}` --
	//
	//	real: tagged      ours: ungrouped
	//
	// It is read out of the view that was just built rather than asking
	// the inventory again: hostvarsVar already computes magicHostVars
	// for every host, so this costs a map lookup instead of another
	// guarded read.
	if hv, ok := static[st.name].(map[string]any); ok {
		if names, ok := hv["group_names"]; ok {
			st.vc.SetVar(vars.Inventory, "group_names", names)
		}
	}
}

// snapshotVars publishes this host's variables for the others to read.
//
// `hostvars` itself is dropped from the snapshot: an entry that carried
// its own copy of every host's variables would grow by a factor of the
// host count on every task, and nothing reads
// hostvars['h2']['hostvars'].
func (ec *execCtx) snapshotVars(st *hostState) {
	// resolvedFor, not resolved: the snapshot is taken at the END of a
	// task, when a value the task just registered is in the store. The
	// unbraked form re-rendered it, so a module result containing
	// `{{ ... }}` was evaluated HERE -- which is where the proof of
	// concept kept firing after the brake went into every other call
	// site. A security brake is only as good as its least-covered
	// caller, and this caller was added by the hostvars work itself.
	m := ec.engine.resolvedFor(st.vc)
	snap := make(map[string]any, len(m))
	for k, v := range m {
		if k == "hostvars" {
			continue
		}
		snap[k] = v
	}
	st.publishVars(snap)
}

// ⛔ add_host and group_by CHANGE THE INVENTORY while a play is running,
// and `groups` and `hostvars` were both built from it once, at play
// start. So a host added by add_host was in neither -- measured against
// ansible-core 2.21.4:
//
//	groups['latecomers']                real: ['h2']  ours: <MISSING>
//	hostvars['newbie']['some_var']      real: hello   ours: <MISSING>
//
// Rebuilding them on every task would cost O(hosts x groups) per task per
// host for a playbook that never calls add_host, which is almost all of
// them. So the inventory carries a GENERATION: the two directives that
// mutate it bump the counter, and the rebuild happens only when the
// number a reader last saw has changed. The common case is one atomic
// load.
//
// The lock is here rather than in go-ansible/inventory because
// Inventory's maps are EXPORTED fields: an internal mutex could not
// guard `inv.Hosts[name]`, and hiding them would break every caller.
// What this does guard is the engine's own concurrent access, which is
// where the goroutines are -- add_host from one host's goroutine while
// another rebuilds. That race existed before any of this: two hosts
// calling add_host under `free` were already writing the same map.

// invChanged records that the inventory was mutated mid-play.
func (ec *execCtx) invChanged() {
	ec.invGen.Add(1)
}

// inventoryView returns `groups` and the inventory's view of every host,
// rebuilt only when the inventory has changed since the last call.
func (ec *execCtx) inventoryView() (groups map[string]any, hostvars map[string]any) {
	gen := ec.invGen.Load()

	ec.invMu.RLock()
	if ec.invViewGen == gen && ec.invViewGroups != nil {
		g, h := ec.invViewGroups, ec.invViewHostvars
		ec.invMu.RUnlock()
		return g, h
	}
	ec.invMu.RUnlock()

	ec.invMu.Lock()
	defer ec.invMu.Unlock()
	// Another goroutine may have rebuilt it while this one waited.
	if ec.invViewGen == gen && ec.invViewGroups != nil {
		return ec.invViewGroups, ec.invViewHostvars
	}
	g := ec.engine.groupsVar()
	h := ec.engine.hostvarsVar(g)
	ec.invViewGen, ec.invViewGroups, ec.invViewHostvars = gen, g, h
	return g, h
}
