package playbook

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"strconv"
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
		return remoteexec.NewLocal(), nil
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

// ConfigSetting is one entry of ConfigDefaults' report: a named
// setting, its ANSIBLE_* environment variable, its compiled-in
// default, and its current effective value (env var if set and valid,
// else the default) — the same precedence DefaultConnect itself uses,
// minus the inventory/host-var layer (which needs a specific host to
// resolve against, and so isn't part of this global view).
type ConfigSetting struct {
	Name    string
	EnvVar  string
	Default string
	Current string
}

// ConfigDefaults reports every setting this package honors from the
// environment and ansible.cfg's [defaults] section — go-ansible/cli's
// ansible-config is a thin printer over this. This is the full list:
// go-ansible reads no other ansible.cfg section or key, and no other
// ANSIBLE_* variable, anywhere in the org.
func ConfigDefaults() []ConfigSetting {
	return []ConfigSetting{
		{
			Name: "remote_user", EnvVar: "ANSIBLE_REMOTE_USER",
			Default: currentUser(), Current: envStr("ANSIBLE_REMOTE_USER", currentUser()),
		},
		{
			Name: "host_key_checking", EnvVar: "ANSIBLE_HOST_KEY_CHECKING",
			Default: "true", Current: strconv.FormatBool(envBool("ANSIBLE_HOST_KEY_CHECKING", true)),
		},
		{
			Name: "timeout", EnvVar: "ANSIBLE_TIMEOUT",
			Default: "10", Current: strconv.Itoa(envInt("ANSIBLE_TIMEOUT", 10)),
		},
		{
			Name: "forks", EnvVar: "ANSIBLE_FORKS",
			Default: "5", Current: strconv.Itoa(envInt("ANSIBLE_FORKS", 5)),
		},
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
	enabled = play.Become
	if task.Become != nil {
		enabled = *task.Become
	}
	if !enabled {
		return remoteexec.BecomeConfig{}, false
	}
	user := play.BecomeUser
	if task.BecomeUser != "" {
		user = task.BecomeUser
	}
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
	}, true
}
