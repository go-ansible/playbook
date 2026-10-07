package playbook

import (
	"fmt"
	"sort"
	"strings"
)

// connectionVars are the variables that DECIDE a connection: change one
// and the next task must talk to a different endpoint, as a different
// user, or over a different transport.
//
// It is a hand-kept list, which is the kind that rots, so
// TestConnectionVarsCoversDefaultConnect reads connect.go and fails if
// DefaultConnect consults a variable this does not name. A key missing
// here means a task that changes it is silently ignored -- the exact
// defect this file exists to fix.
//
// ⚠ A caller who replaces Engine.Connect may read other variables. The
// signature cannot know that, so a custom Connector is reconsulted only
// when one of THESE changes. Said here rather than discovered.
var connectionVars = []string{
	"ansible_connection",
	"ansible_host",
	"ansible_host_key_checking",
	"ansible_password",
	"ansible_port",
	"ansible_ssh_pass",
	"ansible_ssh_private_key_file",
	"ansible_ssh_timeout",
	"ansible_user",
	"playbook_dir",

	// The WinRM arm has its own endpoint and credentials, and they
	// decide a connection exactly as the SSH ones do. They were missing
	// from the first version of this list, and the test below is what
	// found them -- which is the whole argument for having it.
	"ansible_winrm_ca_trust_path",
	"ansible_winrm_cert_key_pem",
	"ansible_winrm_cert_pem",
	"ansible_winrm_connection_timeout",
	"ansible_winrm_host",
	"ansible_winrm_pass",
	"ansible_winrm_password",
	"ansible_winrm_path",
	"ansible_winrm_port",
	"ansible_winrm_scheme",
	"ansible_winrm_server_cert_validation",
	"ansible_winrm_transport",
	"ansible_winrm_user",
}

// connectionSignature is what a connection was built from. Two sets of
// variables with the same signature get the same connection; a task that
// changes one of them gets a new one.
//
// It deliberately does NOT hash every ansible_* key: fact gathering adds
// dozens (ansible_hostname, ansible_os_family, ...), and keying on those
// would reconnect after every gather for no reason.
func connectionSignature(vars map[string]any) string {
	parts := make([]string, 0, len(connectionVars))
	for _, k := range connectionVars {
		if v, ok := vars[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}
