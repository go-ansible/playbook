package playbook

import (
	"strings"
	"testing"
	"time"
)

// The defaults are real Ansible's, read out of its winrm connection
// plugin: port 5986, path /wsman, and a scheme that follows the port
// when unset -- "http if the port is 5985, otherwise https".
func TestWinRMDefaults(t *testing.T) {
	cfg, err := winrmConfigFor("win1", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "win1" {
		t.Errorf("Host = %q, want the inventory hostname", cfg.Host)
	}
	if cfg.Port != 5986 {
		t.Errorf("Port = %d, want 5986", cfg.Port)
	}
	if !cfg.SSL {
		t.Error("SSL is off: port 5986 must imply https")
	}
	if cfg.Path != "/wsman" {
		t.Errorf("Path = %q, want /wsman", cfg.Path)
	}
	if cfg.Transport != "negotiate" {
		t.Errorf("Transport = %q, want negotiate", cfg.Transport)
	}
	if cfg.InsecureSkipVerify {
		t.Error("a host that said nothing about certificates must still verify them")
	}
}

// Port 5985 is the plain-HTTP listener, and real Ansible infers the
// scheme from it rather than requiring both to be set.
func TestWinRMSchemeFollowsPort(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vars    map[string]any
		wantSSL bool
	}{
		{"5985 implies http", map[string]any{"ansible_winrm_port": 5985}, false},
		{"5986 implies https", map[string]any{"ansible_winrm_port": 5986}, true},
		{"any other port implies https", map[string]any{"ansible_winrm_port": 15986}, true},
		{"an explicit scheme wins", map[string]any{"ansible_winrm_port": 5986, "ansible_winrm_scheme": "http"}, false},
		{"and the other way too", map[string]any{"ansible_winrm_port": 5985, "ansible_winrm_scheme": "https"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := winrmConfigFor("w", tc.vars)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.SSL != tc.wantSSL {
				t.Errorf("SSL = %v, want %v", cfg.SSL, tc.wantSSL)
			}
		})
	}
}

// ansible_port is the generic variable; ansible_winrm_port is the
// specific one and wins, which is how the real plugin orders its own
// `vars:` list.
func TestWinRMSpecificVariablesWin(t *testing.T) {
	cfg, err := winrmConfigFor("inventoryname", map[string]any{
		"ansible_host": "generic.host", "ansible_winrm_host": "specific.host",
		"ansible_port": 5985, "ansible_winrm_port": 5986,
		"ansible_user": "generic", "ansible_winrm_user": "specific",
		"ansible_password": "generic-pw", "ansible_winrm_password": "specific-pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "specific.host" || cfg.Port != 5986 || cfg.User != "specific" || cfg.Password != "specific-pw" {
		t.Errorf("cfg = %+v, want every ansible_winrm_* to win", cfg)
	}
}

// ansible_winrm_pass is the real plugin's second spelling of the
// password, and it has to be read -- an inventory using it would
// otherwise authenticate with no password at all.
func TestWinRMPasswordSpellings(t *testing.T) {
	for _, key := range []string{"ansible_winrm_password", "ansible_winrm_pass", "ansible_password"} {
		cfg, err := winrmConfigFor("w", map[string]any{key: "secret"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Password != "secret" {
			t.Errorf("%s was not read: Password = %q", key, cfg.Password)
		}
	}
}

// Real accepts pywinrm's transport names. The five with an equivalent
// here map onto it; the two without are refused BY NAME rather than
// downgraded, since silently authenticating a different way than the
// inventory asked for is the failure worth avoiding.
func TestWinRMTransportMapping(t *testing.T) {
	for in, want := range map[string]string{
		"ntlm": "negotiate", "negotiate": "negotiate",
		"basic": "basic", "plaintext": "basic",
		"certificate": "ssl", "ssl": "ssl",
	} {
		cfg, err := winrmConfigFor("w", map[string]any{"ansible_winrm_transport": in})
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if cfg.Transport != want {
			t.Errorf("%s mapped to %q, want %q", in, cfg.Transport, want)
		}
	}
	for _, in := range []string{"kerberos", "credssp"} {
		_, err := winrmConfigFor("w", map[string]any{"ansible_winrm_transport": in})
		if err == nil {
			t.Fatalf("%s was accepted; it is not implemented and must say so", in)
		}
		if !strings.Contains(err.Error(), in) {
			t.Errorf("the error for %s does not name it: %v", in, err)
		}
	}
	if _, err := winrmConfigFor("w", map[string]any{"ansible_winrm_transport": "nonsense"}); err == nil {
		t.Error("an unknown transport was accepted")
	}
}

// server_cert_validation takes "validate" or "ignore" in real Ansible.
// A third value is an error rather than a guess: read as "ignore" by
// accident, a typo here stops certificates being checked.
func TestWinRMCertValidation(t *testing.T) {
	cfg, err := winrmConfigFor("w", map[string]any{"ansible_winrm_server_cert_validation": "ignore"})
	if err != nil || !cfg.InsecureSkipVerify {
		t.Errorf("ignore did not take effect: %+v %v", cfg, err)
	}
	cfg, err = winrmConfigFor("w", map[string]any{"ansible_winrm_server_cert_validation": "validate"})
	if err != nil || cfg.InsecureSkipVerify {
		t.Errorf("validate did not take effect: %+v %v", cfg, err)
	}
	if _, err := winrmConfigFor("w", map[string]any{"ansible_winrm_server_cert_validation": "Ignore"}); err == nil {
		t.Error("a near-miss spelling was accepted; it must not silently stop verifying")
	}
}

func TestWinRMTimeoutAndPaths(t *testing.T) {
	cfg, err := winrmConfigFor("w", map[string]any{
		"ansible_winrm_connection_timeout": 7,
		"ansible_winrm_path":               "/other",
		"ansible_winrm_ca_trust_path":      "/ca.pem",
		"ansible_winrm_cert_pem":           "/c.pem",
		"ansible_winrm_cert_key_pem":       "/k.pem",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectTimeout != 7*time.Second || cfg.Path != "/other" ||
		cfg.CACert != "/ca.pem" || cfg.ClientCert != "/c.pem" || cfg.ClientKey != "/k.pem" {
		t.Errorf("cfg = %+v", cfg)
	}
}

// An unparseable scheme must not fall through to one of the two valid
// ones.
func TestWinRMBadScheme(t *testing.T) {
	if _, err := winrmConfigFor("w", map[string]any{"ansible_winrm_scheme": "ftp"}); err == nil {
		t.Error("ftp was accepted as a WinRM scheme")
	}
}
