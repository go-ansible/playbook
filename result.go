package playbook

import "sync"

// Result is one task's outcome on one host (one per loop iteration when
// a task loops).
type Result struct {
	Host    string
	Task    string
	Module  string
	Changed bool
	Failed  bool
	Skipped bool

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
	for _, p := range rr.Plays {
		for _, r := range p.Results {
			s := summaryFor(r.Host)
			switch {
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
