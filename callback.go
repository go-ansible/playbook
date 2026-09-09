package playbook

// Callback is a reporting plugin: it observes a run as it happens
// without influencing it, matching real Ansible's own callback plugin
// contract (ansible.plugins.callback.CallbackBase and its v2_* hooks).
// An Engine carries a list of them (Engine.Callbacks), the way real
// Ansible loads one stdout-type callback plus any number of
// notification-type ones at the same time.
//
// Every hook is called synchronously from whichever goroutine reached
// it, so OnTaskResult in particular arrives concurrently from several
// host goroutines at once. An implementation that keeps state or writes
// to a shared stream must serialize itself — DefaultCallback does. A
// hook must not block or panic.
//
// Embed BaseCallback to implement only the hooks you care about, the
// way a real callback plugin overrides only the v2_* methods it needs.
//
// This port has three hooks where real Ansible has 24, because a hook
// nothing in this port can trigger, and nothing can consume, would be
// an empty promise. Two absences worth naming: real Ansible's
// v2_playbook_on_start produces nothing at default verbosity (confirmed
// from default.py — it prints only above -v), and this port has no
// verbosity concept to gate it on; and handler runs are indistinguishable
// from ordinary task runs here, so there is nothing to raise real
// Ansible's separate v2_playbook_on_handler_task_start from.
type Callback interface {
	// OnPlayStart is raised once per play, before its hosts are
	// resolved — real Ansible's v2_playbook_on_play_start.
	OnPlayStart(play Play)

	// OnTaskResult is raised for every task result on every host, one
	// per loop iteration when a task loops. It covers what real Ansible
	// splits across v2_runner_on_ok/failed/skipped, since Result already
	// carries which of those it is.
	OnTaskResult(r Result)

	// OnStats is raised once at the end of a RunPlaybook call, whether
	// or not it returned an error — real Ansible's
	// v2_playbook_on_stats, the PLAY RECAP hook.
	OnStats(rr *RunResult)
}

// BaseCallback implements every Callback hook as a no-op. Embed it in a
// callback that only cares about some of them, matching CallbackBase's
// own all-no-op defaults.
type BaseCallback struct{}

func (BaseCallback) OnPlayStart(Play)    {}
func (BaseCallback) OnTaskResult(Result) {}
func (BaseCallback) OnStats(*RunResult)  {}
