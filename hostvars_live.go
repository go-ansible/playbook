package playbook

import "github.com/go-ansible/vars"

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
	st.vc.SetVar(vars.Inventory, "hostvars", ec.liveHostvars(ec.staticHostvars))
}

// snapshotVars publishes this host's variables for the others to read.
//
// `hostvars` itself is dropped from the snapshot: an entry that carried
// its own copy of every host's variables would grow by a factor of the
// host count on every task, and nothing reads
// hostvars['h2']['hostvars'].
func (ec *execCtx) snapshotVars(st *hostState) {
	m := ec.engine.resolved(st.vc.Merged())
	snap := make(map[string]any, len(m))
	for k, v := range m {
		if k == "hostvars" {
			continue
		}
		snap[k] = v
	}
	st.publishVars(snap)
}
