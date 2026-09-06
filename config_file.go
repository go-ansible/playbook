package playbook

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// ConfigFileValueForEnv reports whether the ansible.cfg go-ansible
// would read from actually sets the [defaults] key matching envVar
// (e.g. "ANSIBLE_REMOTE_USER" -> "remote_user") — go-ansible/cli's
// ansible-config dump uses this to report a setting's real origin
// rather than guessing "cfg" just because some config file happens to
// exist, whether or not it sets that particular key.
func ConfigFileValueForEnv(envVar string) (string, bool) {
	return configFileValue(cfgKey(envVar))
}

// ConfigFilePath returns the ansible.cfg go-ansible would read from —
// see findConfigFile — or "" if none of the usual locations has one.
// go-ansible/cli's ansible-config uses this for `view` and to report
// each setting's real origin (env var / config file / compiled
// default) in `dump`, rather than guessing from whether the resolved
// value merely differs from the default.
func ConfigFilePath() string {
	return findConfigFile()
}

// findConfigFile locates ansible.cfg the same way real Ansible does:
// $ANSIBLE_CONFIG (a directory there means $ANSIBLE_CONFIG/ansible.cfg),
// else ./ansible.cfg, else ~/.ansible.cfg, else /etc/ansible/ansible.cfg
// — the first one that exists wins outright (never merged with the
// others), matching ansible.config.manager.find_ini_config_file.
func findConfigFile() string {
	if v := os.Getenv("ANSIBLE_CONFIG"); v != "" {
		if info, err := os.Stat(v); err == nil {
			if info.IsDir() {
				return filepath.Join(v, "ansible.cfg")
			}
			return v
		}
		return ""
	}
	if cwd, err := os.Getwd(); err == nil {
		if p := filepath.Join(cwd, "ansible.cfg"); fileExists(p) {
			return p
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".ansible.cfg"); fileExists(p) {
			return p
		}
	}
	if fileExists("/etc/ansible/ansible.cfg") {
		return "/etc/ansible/ansible.cfg"
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// configFileValue reads key from ansible.cfg's [defaults] section — the
// only section this port reads from, since remote_user/
// host_key_checking/timeout/forks are the only settings it honors at
// all (see ConfigDefaults). Re-discovers and re-parses the file on
// every call rather than caching: this is called at most a few times
// per host connection, nowhere near a hot loop next to the connection
// setup it feeds into, and not caching keeps this straightforwardly
// testable against a real file with t.Chdir/t.Setenv rather than
// needing a way to reset a package-level cache between tests.
func configFileValue(key string) (string, bool) {
	path := findConfigFile()
	if path == "" {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	section := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != "defaults" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == key {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// cfgKey derives ansible.cfg's [defaults] key name from the matching
// ANSIBLE_* environment variable name — every setting this port reads
// from either source uses exactly this transform (strip the ANSIBLE_
// prefix, lowercase: ANSIBLE_HOST_KEY_CHECKING -> host_key_checking),
// confirmed against ansible-core's own config/base.yml for all four
// settings this package honors, rather than assumed to generalize.
func cfgKey(envVar string) string {
	return strings.ToLower(strings.TrimPrefix(envVar, "ANSIBLE_"))
}
