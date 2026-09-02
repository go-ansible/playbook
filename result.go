package playbook

// Result is one task's outcome on one host (one per loop iteration when
// a task loops).
type Result struct {
	Host    string
	Task    string
	Module  string
	Changed bool
	Failed  bool
	Skipped bool
	Msg     string
	Extra   map[string]any
}

// PlayResult aggregates every Result from one play, in the order tasks
// ran.
type PlayResult struct {
	Play    string
	Results []Result
}

func (pr *PlayResult) record(r Result) {
	pr.Results = append(pr.Results, r)
}

// RunResult aggregates every play's results, in playbook order.
type RunResult struct {
	Plays []PlayResult
}

// Failed reports whether any result in the run failed (and was not
// subsequently rescued — a rescued failure still appears here, since
// Result records history, not final host status; check Ok for that
// question instead).
func (rr *RunResult) Failed() bool {
	for _, p := range rr.Plays {
		for _, r := range p.Results {
			if r.Failed {
				return true
			}
		}
	}
	return false
}

// Summary counts changed/failed/skipped/ok results across the whole
// run, keyed by host — Ansible's PLAY RECAP.
type HostSummary struct {
	Ok, Changed, Failed, Skipped int
}

func (rr *RunResult) Summary() map[string]*HostSummary {
	out := map[string]*HostSummary{}
	for _, p := range rr.Plays {
		for _, r := range p.Results {
			s, ok := out[r.Host]
			if !ok {
				s = &HostSummary{}
				out[r.Host] = s
			}
			switch {
			case r.Skipped:
				s.Skipped++
			case r.Failed:
				s.Failed++
			case r.Changed:
				s.Changed++
			default:
				s.Ok++
			}
		}
	}
	return out
}
