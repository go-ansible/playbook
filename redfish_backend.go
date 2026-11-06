package playbook

import "github.com/go-ansible/modules"

// RedfishBackendVar is the variable an operator sets -- in the inventory,
// in a play's vars or with -e -- to choose the backend of the redfish_info,
// redfish_command and redfish_config modules: "redfishtool" (the default:
// DMTF's redfishtool run on the target, so the request leaves from the
// target's network, as in real Ansible) or "go-redfish" (go-redfish in
// this process, so the request leaves from the control node).
//
// The engine hands it to those modules as modules.RedfishBackendKey, the
// way it hands every module _ansible_diff. A variable is the operator's
// channel because real Ansible ignores a variable it does not know: a
// playbook that sets it still runs there. A task argument would not --
// real refuses an argument outside the module's argument_spec.
const RedfishBackendVar = "goansible_redfish_backend"

// redfishBackendFor returns the value of RedfishBackendVar to hand to the
// task's module, and false when there is nothing to hand: the module does
// not read the key (modules.AcceptsRedfishBackend decides, next to the code
// that reads it, so no list here can go stale when a module learns the
// key), the variable is unset, or the task's own args
// already name a backend (the narrower statement wins). The value is
// passed as written; the module refuses anything but its known backends,
// so a typo fails the task instead of silently choosing the other network.
func redfishBackendFor(module string, vars, args map[string]any) (any, bool) {
	if !modules.AcceptsRedfishBackend(module) {
		return nil, false
	}
	if _, set := args[modules.RedfishBackendKey]; set {
		return nil, false
	}
	v, ok := vars[RedfishBackendVar]
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}
