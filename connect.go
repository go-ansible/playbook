package playbook

import (
	"context"
	"fmt"
	"os"
	"os/user"
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
// ansible_user (default the current OS user), ansible_password /
// ansible_ssh_pass, ansible_ssh_private_key_file,
// ansible_host_key_checking (default true, matching Ansible),
// ansible_ssh_common_args is not implemented (OpenSSH-specific flags
// have no equivalent in a Go SSH client). "localhost"/"127.0.0.1" (by
// hostName, unless ansible_connection overrides it) use Local.
func DefaultConnect(ctx context.Context, hostName string, hostVars map[string]any) (remoteexec.Connection, error) {
	explicitConnType, hasExplicitConnType := hostVars["ansible_connection"].(string)
	isLocalHostname := hostName == "localhost" || hostName == "127.0.0.1"
	if explicitConnType == "local" || (isLocalHostname && !hasExplicitConnType) {
		return remoteexec.NewLocal(), nil
	}

	cfg := remoteexec.SSHConfig{
		Host:           strVar(hostVars, "ansible_host", hostName),
		Port:           intVar(hostVars, "ansible_port", 22),
		User:           strVar(hostVars, "ansible_user", currentUser()),
		Password:       strVar(hostVars, "ansible_password", strVar(hostVars, "ansible_ssh_pass", "")),
		PrivateKeyFile: strVar(hostVars, "ansible_ssh_private_key_file", ""),
		HostKeyCheck:   boolVar(hostVars, "ansible_host_key_checking", true),
		Timeout:        time.Duration(intVar(hostVars, "ansible_ssh_timeout", 10)) * time.Second,
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
