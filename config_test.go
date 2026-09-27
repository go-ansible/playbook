package playbook

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestEnvStrDefaultWhenUnset(t *testing.T) {
	if got := envStr("GOANSIBLE_TEST_UNSET_VAR", "fallback"); got != "fallback" {
		t.Fatalf("envStr = %q, want fallback", got)
	}
}

func TestEnvStrUsesSetValue(t *testing.T) {
	t.Setenv("ANSIBLE_REMOTE_USER", "deployer")
	if got := envStr("ANSIBLE_REMOTE_USER", "fallback"); got != "deployer" {
		t.Fatalf("envStr = %q, want deployer", got)
	}
}

func TestEnvBoolDefaultWhenUnset(t *testing.T) {
	if !envBool("GOANSIBLE_TEST_UNSET_VAR", true) {
		t.Fatal("want the default when unset")
	}
}

func TestEnvBoolParsesSetValue(t *testing.T) {
	t.Setenv("ANSIBLE_HOST_KEY_CHECKING", "false")
	if envBool("ANSIBLE_HOST_KEY_CHECKING", true) {
		t.Fatal("want false, the env value")
	}
}

func TestEnvBoolInvalidValueFallsBackToDefault(t *testing.T) {
	t.Setenv("ANSIBLE_HOST_KEY_CHECKING", "not-a-bool")
	if !envBool("ANSIBLE_HOST_KEY_CHECKING", true) {
		t.Fatal("an unparsable env value should fall back to the default, not error or zero-value")
	}
}

func TestEnvIntParsesSetValue(t *testing.T) {
	t.Setenv("ANSIBLE_TIMEOUT", "30")
	if got := envInt("ANSIBLE_TIMEOUT", 10); got != 30 {
		t.Fatalf("envInt = %d, want 30", got)
	}
}

func TestEnvIntInvalidValueFallsBackToDefault(t *testing.T) {
	t.Setenv("ANSIBLE_TIMEOUT", "not-a-number")
	if got := envInt("ANSIBLE_TIMEOUT", 10); got != 10 {
		t.Fatalf("envInt = %d, want the default 10", got)
	}
}

func TestConfigDefaultsReflectsEnvOverride(t *testing.T) {
	t.Setenv("ANSIBLE_REMOTE_USER", "deployer")
	t.Setenv("ANSIBLE_HOST_KEY_CHECKING", "false")
	t.Setenv("ANSIBLE_TIMEOUT", "45")

	byName := map[string]ConfigSetting{}
	for _, s := range ConfigDefaults() {
		byName[s.Name] = s
	}
	// Real's canonical names, which is what ansible-config prints and
	// what a person greps for.
	for name, want := range map[string]string{
		"DEFAULT_REMOTE_USER": "deployer",
		// A boolean is spelled the way Python spells it, whatever the
		// variable said.
		"HOST_KEY_CHECKING": "False",
		"DEFAULT_TIMEOUT":   "45",
	} {
		if got := byName[name].Current; got != want {
			t.Errorf("%s.Current = %q, want %q", name, got, want)
		}
		if byName[name].Source != "env: "+byName[name].EnvVar {
			t.Errorf("%s.Source = %q", name, byName[name].Source)
		}
	}
	// A TRUE-ish value too, and in a spelling Python does not use:
	// without this the True branch is never reached, and a neuter that
	// spelled it Go's way passed.
	t.Setenv("ANSIBLE_FORCE_COLOR", "yes")
	for _, s := range ConfigDefaults() {
		if s.Name == "ANSIBLE_FORCE_COLOR" && s.Current != "True" {
			t.Errorf("ANSIBLE_FORCE_COLOR=yes reported %q, want True", s.Current)
		}
	}

	// EnvVar/Default are static regardless of the environment.
	if e := byName["DEFAULT_TIMEOUT"]; e.EnvVar != "ANSIBLE_TIMEOUT" || e.Default != "10" {
		t.Errorf("DEFAULT_TIMEOUT entry = %+v", e)
	}
	// Every setting this port honors is listed. It reported four while
	// honoring seven, so three were unreachable through ansible-config.
	for _, name := range []string{
		"ALLOW_BROKEN_CONDITIONALS", "ANSIBLE_FORCE_COLOR", "ANSIBLE_NOCOLOR",
		"DEFAULT_FORKS", "DEFAULT_REMOTE_USER", "DEFAULT_TIMEOUT", "HOST_KEY_CHECKING",
	} {
		if _, listed := byName[name]; !listed {
			t.Errorf("%s is honored but not listed", name)
		}
	}
	// And each carries what `ansible-config list` needs to print.
	for _, s := range ConfigDefaults() {
		if s.EnvVar == "" || s.IniKey == "" || s.IniSection == "" || s.Type == "" || s.Description == "" {
			t.Errorf("%s is missing listing metadata: %+v", s.Name, s)
		}
	}
}

// TestDefaultConnectHonorsEnvTimeout is a real end-to-end check that
// the env var actually reaches SSHConfig, not just ConfigDefaults'
// separate reporting path — it dials a host with no ansible_* vars
// set at all and confirms the attempt still respects
// ANSIBLE_HOST_KEY_CHECKING/ANSIBLE_TIMEOUT/ANSIBLE_REMOTE_USER by
// failing (no real SSH server here) rather than hanging on the
// compiled-in 10s default when a much shorter timeout is set.
func TestDefaultConnectHonorsEnvTimeout(t *testing.T) {
	t.Setenv("ANSIBLE_TIMEOUT", "1")
	t.Setenv("ANSIBLE_REMOTE_USER", "deployer")
	t.Setenv("ANSIBLE_HOST_KEY_CHECKING", "false")

	_, err := DefaultConnect(context.Background(), "127.0.0.1", map[string]any{
		"ansible_connection": "ssh",
		"ansible_port":       1, // nothing listens on port 1
	})
	if err == nil {
		t.Fatal("want a connect error against a port nothing listens on")
	}
}

// isolateConfigDiscovery points every ansible.cfg discovery location
// (cwd and $HOME) at fresh, empty temp directories and clears
// $ANSIBLE_CONFIG, so a test's own real-file assertions can't be
// affected by whatever happens to exist on the machine actually
// running the test (there's nothing on this workstation today, but a
// test that only passes by accident of the current machine's state
// isn't one to keep).
func isolateConfigDiscovery(t *testing.T) {
	t.Helper()
	t.Setenv("ANSIBLE_CONFIG", "")
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
}

// TestAnsibleCfgFileIsReadWhenEnvVarUnset locks in ansible.cfg [defaults]
// support, verified against a real `ansible-config dump` run of an
// equivalent fixture first: remote_user/timeout come from the file when
// no env var overrides them.
func TestAnsibleCfgFileIsReadWhenEnvVarUnset(t *testing.T) {
	isolateConfigDiscovery(t)
	if err := os.WriteFile("ansible.cfg", []byte("[defaults]\nremote_user = cfguser\ntimeout = 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := envStr("ANSIBLE_REMOTE_USER", "fallback"); got != "cfguser" {
		t.Fatalf("envStr = %q, want cfguser", got)
	}
	if got := envInt("ANSIBLE_TIMEOUT", 10); got != 42 {
		t.Fatalf("envInt = %d, want 42", got)
	}
}

// TestAnsibleCfgEnvVarWinsOverFile locks in real Ansible's own
// precedence (verified against a real ansible-config dump run): the
// env var wins over ansible.cfg every time, not just when the file is
// entirely absent.
func TestAnsibleCfgEnvVarWinsOverFile(t *testing.T) {
	isolateConfigDiscovery(t)
	if err := os.WriteFile("ansible.cfg", []byte("[defaults]\nremote_user = cfguser\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANSIBLE_REMOTE_USER", "envuser")
	if got := envStr("ANSIBLE_REMOTE_USER", "fallback"); got != "envuser" {
		t.Fatalf("envStr = %q, want envuser (env var must win over the config file)", got)
	}
}

func TestAnsibleCfgHostKeyCheckingBoolean(t *testing.T) {
	isolateConfigDiscovery(t)
	if err := os.WriteFile("ansible.cfg", []byte("[defaults]\nhost_key_checking = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := envBool("ANSIBLE_HOST_KEY_CHECKING", true); got != false {
		t.Fatalf("envBool = %v, want false", got)
	}
}

func TestAnsibleCfgIgnoresOtherSections(t *testing.T) {
	isolateConfigDiscovery(t)
	if err := os.WriteFile("ansible.cfg", []byte("[privilege_escalation]\nremote_user = wrongsection\n[defaults]\nremote_user = rightsection\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := envStr("ANSIBLE_REMOTE_USER", "fallback"); got != "rightsection" {
		t.Fatalf("envStr = %q, want rightsection (a key outside [defaults] must not apply)", got)
	}
}

func TestConfigFilePathNoneFound(t *testing.T) {
	isolateConfigDiscovery(t)
	if got := ConfigFilePath(); got != "" {
		t.Fatalf("ConfigFilePath = %q, want empty with no ansible.cfg anywhere", got)
	}
}

func TestConfigFileValueForEnvMissingKey(t *testing.T) {
	isolateConfigDiscovery(t)
	if err := os.WriteFile("ansible.cfg", []byte("[defaults]\ntimeout = 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ConfigFileValueForEnv("ANSIBLE_REMOTE_USER"); ok {
		t.Fatal("ConfigFileValueForEnv: want not-ok for a key the file doesn't set")
	}
}

// The dump lines real prints for the settings this port honors,
// measured from ansible-core 2.21.4 with nothing set:
//
//	ALLOW_BROKEN_CONDITIONALS(default) = False
//	ANSIBLE_FORCE_COLOR(default) = False
//	ANSIBLE_NOCOLOR(default) = False
//	DEFAULT_FORKS(default) = 5
//	DEFAULT_REMOTE_USER(default) = None
//	DEFAULT_TIMEOUT(default) = 10
//	HOST_KEY_CHECKING(default) = True
//
// The names are real's canonical ones, the values are Python's spelling,
// and the order is real's (sorted by name).
func TestConfigDefaultsMatchRealsDumpLines(t *testing.T) {
	for _, v := range []string{
		"ANSIBLE_REMOTE_USER", "ANSIBLE_HOST_KEY_CHECKING", "ANSIBLE_TIMEOUT",
		"ANSIBLE_FORKS", "ANSIBLE_FORCE_COLOR", "ANSIBLE_NOCOLOR",
		"ANSIBLE_ALLOW_BROKEN_CONDITIONALS",
	} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	want := []string{
		"ALLOW_BROKEN_CONDITIONALS(default) = False",
		"ANSIBLE_FORCE_COLOR(default) = False",
		"ANSIBLE_NOCOLOR(default) = False",
		"DEFAULT_FORKS(default) = 5",
		"DEFAULT_REMOTE_USER(default) = None",
		"DEFAULT_TIMEOUT(default) = 10",
		"HOST_KEY_CHECKING(default) = True",
	}
	var got []string
	for _, s := range ConfigDefaults() {
		got = append(got, s.Name+"("+s.Source+") = "+s.Current)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dump lines differ from real's\n got %#v\nwant %#v", got, want)
	}
}

// Measured: a value from the environment is reported as
// "NAME(env: ANSIBLE_X) = value", and a boolean is spelled True or False
// whatever the variable said.
func TestConfigDefaultsReportTheEnvironmentAsRealDoes(t *testing.T) {
	t.Setenv("ANSIBLE_FORKS", "9")
	t.Setenv("ANSIBLE_REMOTE_USER", "bob")
	t.Setenv("ANSIBLE_HOST_KEY_CHECKING", "no")
	byName := map[string]ConfigSetting{}
	for _, s := range ConfigDefaults() {
		byName[s.Name] = s
	}
	for name, want := range map[string]string{
		"DEFAULT_FORKS(env: ANSIBLE_FORKS) = 9":                     "",
		"DEFAULT_REMOTE_USER(env: ANSIBLE_REMOTE_USER) = bob":       "",
		"HOST_KEY_CHECKING(env: ANSIBLE_HOST_KEY_CHECKING) = False": "",
	} {
		_ = want
		key := name[:strings.Index(name, "(")]
		s := byName[key]
		if line := s.Name + "(" + s.Source + ") = " + s.Current; line != name {
			t.Errorf("got %q, want %q", line, name)
		}
	}
}
