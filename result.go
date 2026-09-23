package playbook

import (
	"sync"

	"github.com/go-ansible/modules"
)

// Result is one task's outcome on one host (one per loop iteration when
// a task loops).
type Result struct {
	Host    string
	Task    string
	Module  string
	Changed bool
	Failed  bool
	Skipped bool

	// ArgsFailed marks a failure that happened BEFORE the module ran,
	// while finalizing its arguments. Such a result has no module
	// result dict behind it, so it carries none of the keys one would
	// have — including the item/ansible_loop_var an iteration's own
	// failure shows.
	ArgsFailed bool

	// DisplayOnly marks a line that is printed but counts toward
	// nothing in the recap: real closes a loop whose arguments would
	// not finalize with a task-level summary AFTER the per-item lines,
	// and counts the task once, not twice.
	DisplayOnly bool

	// LoopVar is the name the loop bound its item to — "item" unless
	// loop_control.loop_var said otherwise. A failing iteration's
	// printed result carries it, as ansible_loop_var.
	LoopVar string

	// ItemLabel is what a looping task prints in its `(item=...)`:
	// loop_control.label when the task set one, and the item itself
	// otherwise. Kept apart from Item because Item is what register:
	// and the result dict carry, and a label only changes the
	// PRINTING.
	ItemLabel any

	// Included is the file a dynamic include_tasks pulled in, and is
	// set only on the announcement result real Ansible prints for one:
	// "included: <path> for <host>".
	Included string

	// BannerOnly marks a result that exists only to banner its task:
	// real Ansible's meta: fires the task-start callback and no runner
	// callback at all, so it prints a TASK header with no line under
	// it and counts toward nothing in the recap.
	BannerOnly bool

	// Ignored marks a failure the task's own ignore_errors swallowed.
	// Real Ansible counts one of these under "ignored" AND under "ok",
	// not under "failed" — a run whose every failure was ignored reports
	// failed=0.
	Ignored bool

	// Unreachable marks a host that could not be connected to at all.
	// Real Ansible gives it its own recap column, separate from a task
	// that ran and failed.
	Unreachable bool

	Msg   string
	Extra map[string]any

	// Diffs is what the task changed, present only under
	// Engine.DiffMode and only for a module that reports one. A
	// callback renders them; nothing in the engine reads them.
	Diffs []modules.Diff

	// Item is the loop item this result is for, and Looped says the
	// task looped at all — separate, because an item may legitimately
	// be nil. Real Ansible prints the item on the result line, and
	// counts a looped task ONCE in the recap however many iterations
	// it ran.
	Item   any
	Looped bool

	// NoLog marks a result whose task set no_log: true. A callback must
	// print nothing from it beyond the outcome and the host: real
	// Ansible replaces the whole result with a single `censored` key,
	// keeping only `changed`.
	//
	// It matters most on FAILURE, which is exactly when a result is
	// dumped in full — a task handling a credential would otherwise
	// leak it at the worst moment.
	NoLog bool

	// Role is the name of the role this task came from, empty for a
	// task written directly in a playbook. Real Ansible banners such a
	// task "TASK [myrole : the task]"; see DisplayName.
	Role string

	// Handler marks a result produced by a handler rather than an
	// ordinary task. Real Ansible banners those differently —
	// "RUNNING HANDLER [restart nginx]" rather than "TASK [...]" — which
	// is how a reader tells a handler run from a task that happens to
	// share its name.
	Handler bool

	// Delegate is the host a delegate_to task actually ran against,
	// already templated. Empty when the task ran on Host itself — and
	// also empty for a task that was skipped, since a skipped task
	// never connects anywhere. Real Ansible reports the pair as
	// "ok: [web1 -> deploy1]".
	Delegate string
}

// PlayResult aggregates every Result from one play, in the order
// record was called — not necessarily task-list order, since the
// engine runs one goroutine per active host and every host's goroutine
// calls record on the same *PlayResult concurrently. mu is a pointer
// (not an embedded sync.Mutex) specifically so a PlayResult can still
// be copied by value — as RunPlaybook does, appending *pr into
// RunResult.Plays — without go vet's copylocks check firing; every
// copy keeps pointing at the one real mutex.
type PlayResult struct {
	Play    string
	Results []Result

	// Rescued counts, per host, the blocks whose rescue recovered a
	// failure. It is not derivable from Results: rescuing is a property
	// of a BLOCK, and the failing task inside it looks the same either
	// way. Real Ansible counts those under "rescued" and not "failed".
	Rescued map[string]int

	mu *sync.Mutex
}

// newPlayResult is the only correct way to construct a PlayResult
// outside of copying an existing one — it allocates the shared mutex.
func newPlayResult(play string) *PlayResult {
	return &PlayResult{Play: play, Rescued: map[string]int{}, mu: &sync.Mutex{}}
}

// recordRescued notes that a block's rescue recovered host.
func (pr *PlayResult) recordRescued(host string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.Rescued == nil {
		pr.Rescued = map[string]int{}
	}
	pr.Rescued[host]++
}

func (pr *PlayResult) record(r Result) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.Results = append(pr.Results, r)
}

// RunResult aggregates every play's results, in playbook order.
type RunResult struct {
	Plays []PlayResult
}

// Failed reports whether the run ended with a real failure — the
// question ansible-playbook's exit code asks. It is the host's FINAL
// state, not the history: a failure the task's own ignore_errors
// swallowed does not count, and neither does one a block's rescue
// recovered. Real ansible-playbook exits 0 for a run whose only
// failures were ignored or rescued, and this port used to exit 2.
func (rr *RunResult) Failed() bool {
	for _, s := range rr.Summary() {
		if s.Failed > 0 || s.Unreachable > 0 {
			return true
		}
	}
	return false
}

// Unreachable reports whether any host ended the run unreachable —
// one whose ignore_unreachable said to carry on does NOT count, since
// real Ansible files it under ok/ignored instead.
//
// It is separate from Failed because real ansible-playbook's exit code
// distinguishes them, and unreachable WINS: 4 if any host was
// unreachable, else 2 if any failed, else 0 (ansible's own
// StrategyBase._process_pending_results tail, and measured — a run with
// one failed host and one unreachable host exits 4, not 2 and not 6).
func (rr *RunResult) Unreachable() bool {
	for _, s := range rr.Summary() {
		if s.Unreachable > 0 {
			return true
		}
	}
	return false
}

// foldLoops collapses each looped task's iterations into one result
// per host, leaving every other result untouched and in order.
//
// The folded state is what real Ansible reports: changed if ANY
// iteration changed, failed if any failed, ignored if any was ignored,
// and skipped only when EVERY iteration was skipped — a loop with one
// item that ran and two that did not counts as ok, not skipped.
func foldLoops(results []Result) []Result {
	out := make([]Result, 0, len(results))
	// Keyed by host and task name: folding two same-named tasks
	// together is what real Ansible's own per-task counting does too.
	index := map[[2]string]int{}

	for _, r := range results {
		if !r.Looped {
			out = append(out, r)
			continue
		}
		key := [2]string{r.Host, r.Task}
		at, seen := index[key]
		if !seen {
			index[key] = len(out)
			out = append(out, r)
			continue
		}
		folded := &out[at]
		folded.Changed = folded.Changed || r.Changed
		folded.Failed = folded.Failed || r.Failed
		folded.Ignored = folded.Ignored || r.Ignored
		folded.Unreachable = folded.Unreachable || r.Unreachable
		// Skipped survives only while every iteration so far skipped.
		folded.Skipped = folded.Skipped && r.Skipped
	}
	return out
}

// Summary counts changed/failed/skipped/ok results across the whole
// run, keyed by host — Ansible's PLAY RECAP.
type HostSummary struct {
	Ok, Changed, Failed, Skipped int

	// Unreachable, Rescued and Ignored are real Ansible's own further
	// recap columns. Without them a rescued or ignored failure has
	// nowhere to go but "failed", which is what this port used to do —
	// reporting failures for a run real Ansible calls clean.
	Unreachable, Rescued, Ignored int
}

func (rr *RunResult) Summary() map[string]*HostSummary {
	out := map[string]*HostSummary{}
	summaryFor := func(host string) *HostSummary {
		s, ok := out[host]
		if !ok {
			s = &HostSummary{}
			out[host] = s
		}
		return s
	}
	// Real Ansible counts a LOOPED task once per host however many
	// iterations it ran: a loop over three items that all changed
	// reports changed=1, not 3. This port reported one entry per
	// iteration, so a 100-item loop inflated the recap by a hundred.
	for _, p := range rr.Plays {
		for _, r := range foldLoops(p.Results) {
			if r.BannerOnly || r.DisplayOnly {
				continue
			}
			s := summaryFor(r.Host)
			switch {
			case r.Unreachable && r.Ignored:
				// ignore_unreachable: real Ansible counts the host
				// under ok and ignored, NOT under unreachable — a
				// play that ignores every unreachable host reports
				// unreachable=0 and exits 0. Measured on 2.21.4.
				s.Ignored++
				s.Ok++
			case r.Unreachable:
				s.Unreachable++
			case r.Skipped:
				s.Skipped++
			case r.Failed && r.Ignored:
				// Real Ansible counts an ignored failure under BOTH
				// ignored and ok, which is why a run of nothing but
				// ignored failures still reports ok=N failed=0.
				s.Ignored++
				s.Ok++
				// And under changed, if it changed something before it
				// failed. Measured: a `shell` that writes and then
				// exits non-zero under ignore_errors reports changed=1
				// there and reported changed=0 here. A failure that is
				// NOT ignored counts under neither, which is why this
				// belongs in this branch rather than beside it.
				if r.Changed {
					s.Changed++
				}
			case r.Failed:
				s.Failed++
			case r.Changed:
				// A changed task counts under ok as well, as it does in
				// real Ansible's own stats.
				s.Changed++
				s.Ok++
			default:
				s.Ok++
			}
		}
		for host, n := range p.Rescued {
			s := summaryFor(host)
			s.Rescued += n
			// A rescued failure was already counted under failed when the
			// task itself was recorded; real Ansible reports it as
			// rescued instead.
			s.Failed -= n
			if s.Failed < 0 {
				s.Failed = 0
			}
		}
	}
	return out
}
