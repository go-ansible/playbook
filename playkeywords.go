package playbook

import "fmt"

// playKeywords is the set of keywords real Ansible accepts on a Play,
// taken from the reference itself: every entry of
// `ansible-doc -t keyword -l` whose `applies_to` includes "Play", in
// ansible-core 2.21.4. There are 41, and playkeywords_test.go re-derives
// the list from the installed reference so it cannot drift.
//
// It is REAL's set, deliberately, not the set this port implements.
// Fifteen of these are accepted here without being honoured (or are
// handled outside parsePlay); refusing them would reject playbooks real
// runs, which is a worse failure than ignoring them. The point of the
// list is the other direction: a key in NEITHER set is a typo, and real
// says so.
var playKeywords = map[string]bool{
	"any_errors_fatal": true, "become": true, "become_exe": true,
	"become_flags": true, "become_method": true, "become_user": true,
	"check_mode": true, "collections": true, "connection": true,
	"debugger": true, "diff": true, "environment": true,
	"fact_path": true, "force_handlers": true, "gather_facts": true,
	"gather_subset": true, "gather_timeout": true, "handlers": true,
	"hosts": true, "ignore_errors": true, "ignore_unreachable": true,
	"max_fail_percentage": true, "module_defaults": true, "name": true,
	"no_log": true, "order": true, "port": true, "post_tasks": true,
	"pre_tasks": true, "remote_user": true, "roles": true,
	"run_once": true, "serial": true, "strategy": true, "tags": true,
	"tasks": true, "throttle": true, "timeout": true, "vars": true,
	"vars_files": true, "vars_prompt": true,
}

// checkPlayKeywords refuses a key real would refuse.
//
// Without it a mistyped play keyword was silently dropped: measured,
// `serail: 1` ran the play against every host at once and exited 0,
// where real exits 4 with "'serail' is not a valid attribute for a
// Play". A typo that turns a rolling update into a simultaneous one,
// with nothing printed, is the kind of divergence a drop-in replacement
// cannot have.
//
// A TASK key was already safe: an unknown one reads as a second action
// and both implementations refuse it ("conflicting action statements").
// This closes the play level, which had no such accident to catch it.
func checkPlayKeywords(m map[string]any, name string) error {
	for k := range m {
		if !playKeywords[k] {
			// Real's wording, and real's shape: the key alone, not the
			// play it sits in -- a play whose `name` is itself the typo
			// case would otherwise print an empty quoted name.
			return fmt.Errorf("%q is not a valid attribute for a Play", k)
		}
	}
	return nil
}
