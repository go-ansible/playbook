package playbook

import (
	"context"
	"fmt"
	"strings"
	"time"

	remoteexec "github.com/go-remoteexec/transport"
)

// winrmTransports maps real Ansible's ansible_winrm_transport values
// onto the three auth schemes go-remoteexec/transport implements.
//
// Real Ansible accepts the list pywinrm accepts: basic, plaintext,
// certificate, ssl, ntlm, kerberos, credssp. The first five have an
// equivalent here; kerberos and credssp do not, and are refused by name
// below rather than silently downgraded to something weaker, which is
// the whole point of naming an auth scheme in the first place.
var winrmTransports = map[string]string{
	"ntlm":        "negotiate",
	"negotiate":   "negotiate",
	"basic":       "basic",
	"plaintext":   "basic",
	"certificate": "ssl",
	"ssl":         "ssl",
}

// winrmUnsupported are the real transport values this port cannot
// honour, each with what it would take -- an error that says which is
// worth more than one that says "unknown".
var winrmUnsupported = map[string]string{
	"kerberos": "needs a GSSAPI/Kerberos ticket exchange, which this transport does not implement",
	"credssp":  "needs CredSSP's own SPNEGO-over-TLS handshake, which this transport does not implement",
}

// dialWinRM builds a WinRM connection from the ansible_winrm_* host
// variables, following the real winrm connection plugin.
//
// The variable names are not invented. The real plugin declares
// host/port/user/password/scheme/path/transport/connection_timeout
// explicitly, and documents that any further pywinrm Protocol argument
// may be passed as ansible_winrm_<option> -- which is where
// server_cert_validation, ca_trust_path, cert_pem and cert_key_pem come
// from. Options the real plugin has and this one does not (the kinit_*
// family) belong to Kerberos, which is refused above.
func dialWinRM(ctx context.Context, hostName string, hostVars map[string]any) (remoteexec.Connection, error) {
	cfg, err := winrmConfigFor(hostName, hostVars)
	if err != nil {
		return nil, err
	}
	conn, err := remoteexec.DialWinRM(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", hostName, err)
	}
	return conn, nil
}

// winrmConfigFor is the mapping on its own, separated from dialling so
// the variable names and defaults can be tested without a Windows host
// to reach.
func winrmConfigFor(hostName string, hostVars map[string]any) (remoteexec.WinRMConfig, error) {
	// Real: port defaults to 5986, and the scheme follows from it when
	// unset -- "http if the port is 5985, otherwise https".
	port := intVar(hostVars, "ansible_winrm_port", intVar(hostVars, "ansible_port", 5986))
	scheme := strVar(hostVars, "ansible_winrm_scheme", "")
	if scheme == "" {
		if port == 5985 {
			scheme = "http"
		} else {
			scheme = "https"
		}
	}
	switch scheme {
	case "http", "https":
	default:
		return remoteexec.WinRMConfig{}, fmt.Errorf("connecting to %s: ansible_winrm_scheme %q is not http or https", hostName, scheme)
	}

	rawTransport := strVar(hostVars, "ansible_winrm_transport", "negotiate")
	if why, bad := winrmUnsupported[rawTransport]; bad {
		return remoteexec.WinRMConfig{}, fmt.Errorf("connecting to %s: ansible_winrm_transport %q is not supported: %s",
			hostName, rawTransport, why)
	}
	transportName, ok := winrmTransports[rawTransport]
	if !ok {
		return remoteexec.WinRMConfig{}, fmt.Errorf("connecting to %s: ansible_winrm_transport %q is not one of %s",
			hostName, rawTransport, strings.Join([]string{"basic", "plaintext", "ntlm", "negotiate", "certificate", "ssl"}, ", "))
	}

	// Real's server_cert_validation takes "validate" (its default) or
	// "ignore". Anything else is an error rather than a guess: a typo
	// here would otherwise read as "ignore" and quietly stop verifying.
	validation := strVar(hostVars, "ansible_winrm_server_cert_validation", "validate")
	var skipVerify bool
	switch validation {
	case "validate":
	case "ignore":
		skipVerify = true
	default:
		return remoteexec.WinRMConfig{}, fmt.Errorf("connecting to %s: ansible_winrm_server_cert_validation %q is not validate or ignore",
			hostName, validation)
	}

	cfg := remoteexec.WinRMConfig{
		Host: strVar(hostVars, "ansible_winrm_host", strVar(hostVars, "ansible_host", hostName)),
		Port: port,
		User: strVar(hostVars, "ansible_winrm_user",
			strVar(hostVars, "ansible_user", envStr("ANSIBLE_REMOTE_USER", currentUser()))),
		Password: strVar(hostVars, "ansible_winrm_password",
			strVar(hostVars, "ansible_winrm_pass", strVar(hostVars, "ansible_password", ""))),
		SSL:                scheme == "https",
		InsecureSkipVerify: skipVerify,
		CACert:             strVar(hostVars, "ansible_winrm_ca_trust_path", ""),
		Transport:          transportName,
		ClientCert:         strVar(hostVars, "ansible_winrm_cert_pem", ""),
		ClientKey:          strVar(hostVars, "ansible_winrm_cert_key_pem", ""),
		Path:               strVar(hostVars, "ansible_winrm_path", "/wsman"),
		ConnectTimeout: time.Duration(intVar(hostVars, "ansible_winrm_connection_timeout",
			envInt("ANSIBLE_TIMEOUT", 30))) * time.Second,
	}

	return cfg, nil
}
