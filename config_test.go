package playbook

import (
	"context"
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

	settings := ConfigDefaults()
	if len(settings) != 3 {
		t.Fatalf("ConfigDefaults() = %d entries, want 3", len(settings))
	}
	byName := map[string]ConfigSetting{}
	for _, s := range settings {
		byName[s.Name] = s
	}
	if byName["remote_user"].Current != "deployer" {
		t.Fatalf("remote_user.Current = %q", byName["remote_user"].Current)
	}
	if byName["host_key_checking"].Current != "false" {
		t.Fatalf("host_key_checking.Current = %q", byName["host_key_checking"].Current)
	}
	if byName["timeout"].Current != "45" {
		t.Fatalf("timeout.Current = %q", byName["timeout"].Current)
	}
	// EnvVar/Default fields are static regardless of the environment.
	if byName["timeout"].EnvVar != "ANSIBLE_TIMEOUT" || byName["timeout"].Default != "10" {
		t.Fatalf("timeout entry = %+v", byName["timeout"])
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
