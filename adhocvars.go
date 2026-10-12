package playbook

import (
	"os"

	"github.com/go-ansible/inventory"
)

// AdHocHostVars returns the variables an ad-hoc module invocation
// templates its arguments against, for one host out of the matched set.
//
// It lives here, beside the play path's own construction, because the
// alternative is a second place that decides which variables exist —
// and two such places drift. `ansible -m debug -a 'msg={{ x }}'` has to
// resolve x the way a play would, and everything below was measured
// against real ansible-core 2.21.4 with exactly that command:
//
//	inventory_hostname         "h1"
//	inventory_hostname_short   "h1"
//	group_names                ["web"]
//	groups.web | length        2
//	hostvars["h2"].wv          "from-web-group"
//	wv                         "from-web-group"   (a group_vars value)
//	ansible_play_hosts|length  1                  (the MATCHED hosts)
//	inventory_dir              the inventory's own directory
//	playbook_dir               the WORKING directory
//
// The last two are not the same thing, which an inventory in a
// subdirectory shows: `-i sub/inv2` gives inventory_dir .../sub and
// playbook_dir the cwd. There is no playbook, so real uses the cwd.
func AdHocHostVars(inv *inventory.Inventory, host string, matched []string) map[string]any {
	groups := groupsVarFor(inv)

	out := map[string]any{}
	for k, v := range inv.HostVars(host) {
		out[k] = v
	}
	// After HostVars, so a host_var of the same name cannot shadow a
	// magic one — the play path orders these the same way, and for the
	// same reason.
	for k, v := range magicHostVars(inv, host) {
		out[k] = v
	}
	out["groups"] = groups
	out["hostvars"] = hostvarsVarFor(inv, groups)
	out["inventory_dir"] = inv.SourceDir

	// Ad-hoc runs one implicit play over the hosts the pattern matched,
	// so all three are that set: there is no serial: to make the batch
	// differ from the play, and no failures yet to make the live list
	// differ from the original.
	hosts := append([]string{}, matched...)
	out["ansible_play_hosts"] = hosts
	out["ansible_play_hosts_all"] = hosts
	out["ansible_play_batch"] = hosts

	if wd, err := os.Getwd(); err == nil {
		out["playbook_dir"] = wd
	}
	return out
}
