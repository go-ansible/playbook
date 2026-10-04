package playbook

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"time"

	remoteexec "github.com/go-remoteexec/transport"
)

// Connector dials a connection to a host, given its name and fully
// merged variables (before task-level rendering — this only needs the
// ansible_* connection variables, which are ordinary inventory/group
// vars).
type Connector func(ctx context.Context, hostName string, hostVars map[string]any) (remoteexec.Connection, error)

// DefaultConnect implements Ansible's connection-variable conventions:
// ansible_connection ("local" or "ssh", default "ssh" — "smart" is
// treated as ssh, this package has no separate paramiko/openssh split
// to be smart about), ansible_host, ansible_port (default 22),
// ansible_user, ansible_password / ansible_ssh_pass,
// ansible_ssh_private_key_file, ansible_host_key_checking,
// ansible_ssh_timeout. ansible_ssh_common_args is not implemented
// (OpenSSH-specific flags have no equivalent in a Go SSH client).
// "localhost"/"127.0.0.1" (by hostName, unless ansible_connection
// overrides it) use Local.
//
// Three of these settings — remote_user, host_key_checking, timeout —
// match real Ansible's own config precedence: an inventory/host var
// wins if set, otherwise an ANSIBLE_* environment variable if set,
// otherwise ansible.cfg's [defaults] section (see configFileValue) if
// it sets the matching key, otherwise the compiled-in default below.
// (forks is a fourth setting on the same precedence, minus the
// host-var layer — see Engine.Forks/ConfigDefaults — since it's a
// global concurrency cap, not a per-host connection detail, so it
// doesn't belong in this function's own resolution chain.) These four
// settings are the full extent of this port's ansible.cfg support — no
// other section or key is read at all (a real, stated gap — see
// go-ansible/cli's ansible-config, which reports exactly this
// precedence and these four settings, nothing more). Unlike real
// Ansible's lenient boolean parsing (yes/no/on/off/1/0/true/false,
// case-insensitive), ANSIBLE_HOST_KEY_CHECKING here is parsed with
// Go's strconv.ParseBool (true/false/1/0/t/f, case-sensitive on the
// word forms) — an invalid value falls back to the compiled-in
// default rather than erroring, matching how an unset var behaves.
func DefaultConnect(ctx context.Context, hostName string, hostVars map[string]any) (remoteexec.Connection, error) {
	explicitConnType, hasExplicitConnType := hostVars["ansible_connection"].(string)
	isLocalHostname := hostName == "localhost" || hostName == "127.0.0.1"
	if explicitConnType == "local" || (isLocalHostname && !hasExplicitConnType) {
		local := remoteexec.NewLocal()
		// Real runs a local module with the working directory set to
		// the PLAYBOOK's own directory, not to wherever
		// ansible-playbook was invoked -- measured against ansible-core
		// 2.21.4 from three different working directories, inside a
		// role and out, with and without an explicit connection:
		// keyword. So `read_csv: {path: files/data.csv}` resolves
		// beside the playbook, and did not here: the same playbook
		// worked or failed depending on where it was run from.
		//
		// It comes from playbook_dir rather than a new parameter
		// because that is the magic variable REAL exposes for exactly
		// this, the engine already publishes it, and a caller who
		// replaces Connect gets it by the same route.
		local.Dir = strVar(hostVars, "playbook_dir", "")
		return local, nil
	}

	// WinRM, for a Windows target. The transport under this package has
	// spoken WS-Management for a while; nothing here could ask for it,
	// so `ansible_connection: winrm` fell through to the SSH branch and
	// tried to open an SSH session against a Windows host.
	if explicitConnType == "winrm" {
		return dialWinRM(ctx, hostName, hostVars)
	}

	cfg := remoteexec.SSHConfig{
		Host:           strVar(hostVars, "ansible_host", hostName),
		Port:           intVar(hostVars, "ansible_port", 22),
		User:           strVar(hostVars, "ansible_user", envStr("ANSIBLE_REMOTE_USER", currentUser())),
		Password:       strVar(hostVars, "ansible_password", strVar(hostVars, "ansible_ssh_pass", "")),
		PrivateKeyFile: strVar(hostVars, "ansible_ssh_private_key_file", ""),
		HostKeyCheck:   boolVar(hostVars, "ansible_host_key_checking", envBool("ANSIBLE_HOST_KEY_CHECKING", true)),
		Timeout:        time.Duration(intVar(hostVars, "ansible_ssh_timeout", envInt("ANSIBLE_TIMEOUT", 10))) * time.Second,
	}
	if cfg.PrivateKeyFile == "" && cfg.Password == "" {
		cfg.UseAgent = true
	}
	conn, err := remoteexec.DialSSH(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", hostName, err)
	}
	return conn, nil
}

// envStr, envBool, and envInt read an ANSIBLE_* environment variable,
// falling back to ansible.cfg's [defaults] section (see
// configFileValue/cfgKey) and then the compiled-in default — the
// middle two rungs of the precedence chain (host var > env var >
// ansible.cfg file > compiled-in default) noted on DefaultConnect's
// own doc comment. Real Ansible's own precedence has the env var win
// over the config file every time, which is why the env-var check
// comes first in each of these rather than the other way around.
func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v, ok := configFileValue(cfgKey(key)); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	if v, ok := configFileValue(cfgKey(key)); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	if v, ok := configFileValue(cfgKey(key)); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// ConfigSetting is one entry of ConfigDefaults' report.
//
// Name is real Ansible's own canonical name for the setting --
// DEFAULT_FORKS rather than forks -- because that is what
// `ansible-config dump` prints and what a person greps its output for.
// The ANSIBLE_* variable and the ini key are the two ways to set it.
//
// Default and Current are rendered the way real renders them, which is
// PYTHON's repr and not Go's: True, False, None, a bare integer, a bare
// string. Source says where Current came from, in real's own spelling:
// "default", "env: ANSIBLE_X", or the path of the ini file.
//
// Description is this port's OWN wording. Real's descriptions are
// GPL-licensed prose and these repositories are BSD-licensed, so
// copying them across would import a licence rather than a fact -- the
// names, variables, keys, types and defaults are interface facts and
// are matched exactly; the prose is not and cannot be.
type ConfigSetting struct {
	Name         string
	EnvVar       string
	IniSection   string
	IniKey       string
	Type         string
	Description  string
	VersionAdded string
	Default      string
	Current      string
	Source       string
}

// ConfigDefaults reports every setting this package honors from the
// environment and ansible.cfg's [defaults] section — go-ansible/cli's
// ansible-config is a thin printer over this.
//
// It used to list four settings while the port honored seven: a person
// could not discover ANSIBLE_FORCE_COLOR, ANSIBLE_NOCOLOR or
// ANSIBLE_ALLOW_BROKEN_CONDITIONALS from `ansible-config` at all, though
// all three change what a run does.
//
// Real lists 219. The ones missing here are missing because this port
// does not honor them, and listing a setting that changes nothing would
// be worse than its absence.
func ConfigDefaults() []ConfigSetting {
	settings := []ConfigSetting{
		{
			// Real's default is None, not the current user: the config
			// layer leaves it unset and the connection resolves it,
			// which is what this port does too. Reporting the resolved
			// user as the DEFAULT said the wrong thing about where the
			// value comes from.
			Name: "DEFAULT_REMOTE_USER", EnvVar: "ANSIBLE_REMOTE_USER",
			IniSection: "defaults", IniKey: "remote_user", Type: "string",
			Description:  "The user to log in as, when the play and the inventory name none.",
			VersionAdded: "2.4",
			Default:      "None",
		},
		{
			Name: "HOST_KEY_CHECKING", EnvVar: "ANSIBLE_HOST_KEY_CHECKING",
			IniSection: "defaults", IniKey: "host_key_checking", Type: "boolean",
			Description: "Whether to refuse a host whose SSH key is not already known.",
			Default:     "True",
		},
		{
			Name: "DEFAULT_TIMEOUT", EnvVar: "ANSIBLE_TIMEOUT",
			IniSection: "defaults", IniKey: "timeout", Type: "integer",
			Description: "How long a connection may take to establish, in seconds.",
			Default:     "10",
		},
		{
			Name: "DEFAULT_FORKS", EnvVar: "ANSIBLE_FORKS",
			IniSection: "defaults", IniKey: "forks", Type: "integer",
			Description: "How many hosts to work on at once.",
			Default:     "5",
		},
		{
			Name: "ANSIBLE_FORCE_COLOR", EnvVar: "ANSIBLE_FORCE_COLOR",
			IniSection: "defaults", IniKey: "force_color", Type: "boolean",
			Description: "Colorize output even when it is not going to a terminal.",
			Default:     "False",
		},
		{
			Name: "ANSIBLE_NOCOLOR", EnvVar: "ANSIBLE_NOCOLOR",
			IniSection: "defaults", IniKey: "nocolor", Type: "boolean",
			Description: "Never colorize output, whatever it is going to.",
			Default:     "False",
		},
		{
			Name: "ALLOW_BROKEN_CONDITIONALS", EnvVar: "ANSIBLE_ALLOW_BROKEN_CONDITIONALS",
			IniSection: "defaults", IniKey: "allow_broken_conditionals", Type: "boolean",
			Description: "Accept a conditional that does not evaluate to a boolean.",
			Default:     "False",
		},
	}
	// The effective value and where it came from, in real's precedence:
	// the environment, then ansible.cfg, then the compiled-in default.
	//
	// Real spells an ini origin as the PATH ALONE -- measured,
	// "DEFAULT_FORKS(/tmp/ansible.cfg) = 7" -- and an environment one as
	// "env: ANSIBLE_X".
	cfgPath := ConfigFilePath()
	for i := range settings {
		s := &settings[i]
		s.Current, s.Source = s.Default, "default"
		if raw, ok := os.LookupEnv(s.EnvVar); ok && raw != "" {
			s.Current, s.Source = renderConfigValue(raw, s.Type), "env: "+s.EnvVar
			continue
		}
		// Only if the environment said nothing: the environment WINS.
		// Deciding the origin was the environment and then overwriting it
		// with the file, as the printer used to, attributed a setting
		// given in both places to the file while showing the
		// environment's value -- a line that disagreed with itself.
		if cfgPath == "" {
			continue
		}
		if raw, ok := ConfigFileValueForEnv(s.EnvVar); ok {
			s.Current, s.Source = renderConfigValue(raw, s.Type), cfgPath
		}
	}
	// Real sorts its dump by name, so the order is part of the output.
	sort.Slice(settings, func(i, j int) bool { return settings[i].Name < settings[j].Name })
	return settings
}

// renderConfigValue spells a value the way real does: a boolean as True
// or False whatever the input looked like, an integer bare, a string
// bare, and an empty string as None.
func renderConfigValue(raw, typ string) string {
	switch typ {
	case "boolean":
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "1", "true", "yes", "on", "y", "t":
			return "True"
		default:
			return "False"
		}
	case "integer":
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			return strconv.Itoa(n)
		}
		return raw
	default:
		if raw == "" {
			return "None"
		}
		return raw
	}
}

func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "root"
}

func strVar(vars map[string]any, key, def string) string {
	v, ok := vars[key]
	if !ok {
		return def
	}
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func intVar(vars map[string]any, key string, def int) int {
	v, ok := vars[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return def
}

func boolVar(vars map[string]any, key string, def bool) bool {
	v, ok := vars[key]
	if !ok {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

// becomeConfigFor resolves the effective become settings for a task
// (task-level overrides play-level) into a remoteexec.BecomeConfig, or
// reports enabled=false if no escalation applies.
func becomeConfigFor(play Play, task Task, hostVars map[string]any) (cfg remoteexec.BecomeConfig, enabled bool) {
	// Precedence, measured against real ansible-core 2.21.4 and the
	// same as connection's: HOST VAR > task keyword > play keyword.
	//
	// The host var winning is the counter-intuitive half, and both
	// directions were wrong here. `ansible_become=false` on a host was
	// ignored, so a task written `become: true` escalated on a host
	// that said not to; and `ansible_become=true` was ignored too, so a
	// task meant to escalate ran unprivileged.
	enabled = play.Become
	if task.Become != nil {
		enabled = *task.Become
	}
	enabled = boolVar(hostVars, "ansible_become", enabled)
	if !enabled {
		return remoteexec.BecomeConfig{}, false
	}
	user := play.BecomeUser
	if task.BecomeUser != "" {
		user = task.BecomeUser
	}
	user = strVar(hostVars, "ansible_become_user", user)
	method := remoteexec.BecomeSudo
	switch strVar(hostVars, "ansible_become_method", play.BecomeMethod) {
	case "su":
		method = remoteexec.BecomeSu
	case "doas":
		method = remoteexec.BecomeDoas
	}
	return remoteexec.BecomeConfig{
		Method:   method,
		User:     user,
		Password: strVar(hostVars, "ansible_become_password", strVar(hostVars, "ansible_become_pass", "")),
		Exe:      becomeOption(hostVars, method, "exe", task.BecomeExe, play.BecomeExe),
		Flags:    becomeOption(hostVars, method, "flags", task.BecomeFlags, play.BecomeFlags),
	}, true
}

// becomeOption resolves become_exe or become_flags across the five
// sources real Ansible reads for them.
//
// The ORDER is measured against ansible-core 2.21.4 rather than assumed,
// because the counter-intuitive rung is near the top: the host VAR beats
// the playbook KEYWORD. With `become_flags: "-H -S -n -K1"` on the play
// and ansible_become_flags set to "-H -S -n -K2", real emits -K2. The
// same holds for become_exe, and it is the same shape becomeConfigFor
// already documents for become/become_user.
//
//	host var > task keyword > play keyword > environment > ansible.cfg
//
// Each rung has two spellings: a generic one and one named after the
// method -- ansible_become_flags and ansible_sudo_flags for sudo,
// ansible_su_flags for su, and so on, which is how each become plugin
// declares its own options. The generic spelling is checked first.
//
// An empty result means "say nothing", and the transport then applies
// the method's own documented default.
func becomeOption(hostVars map[string]any, method remoteexec.BecomeMethod, what, taskKeyword, playKeyword string) string {
	for _, name := range []string{"ansible_become_" + what, "ansible_" + string(method) + "_" + what} {
		if v := strVar(hostVars, name, ""); v != "" {
			return v
		}
	}
	if taskKeyword != "" {
		return taskKeyword
	}
	if playKeyword != "" {
		return playKeyword
	}
	for _, name := range []string{"ANSIBLE_BECOME_" + strings.ToUpper(what), "ANSIBLE_" + strings.ToUpper(string(method)) + "_" + strings.ToUpper(what)} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	// Real puts these under [privilege_escalation], not [defaults].
	if v, ok := configFileValueIn("privilege_escalation", "become_"+what); ok && v != "" {
		return v
	}
	return ""
}
