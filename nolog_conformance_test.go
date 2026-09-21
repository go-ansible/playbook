package playbook

import (
	"strings"
	"testing"
)

// TestNoLogCensorsTheResult pins no_log: true against real ansible-core
// 2.21.4. The secret below must not appear anywhere in the output —
// least of all on the FAILING task, which is exactly when a result is
// dumped in full.
func TestNoLogCensorsTheResult(t *testing.T) {
	out := runAndCapture(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - {name: ok_dbg,  debug: {msg: SHIBBOLETH}, no_log: true}
    - {name: chg_cmd, command: echo SHIBBOLETH, no_log: true}
    - {name: skipped, debug: {msg: SHIBBOLETH}, no_log: true, when: false}
    - name: failed
      command: sh -c "echo SHIBBOLETH; exit 1"
      no_log: true
      ignore_errors: true
    - {name: visible, debug: {msg: VISIBLE}}
`)
	if strings.Contains(out, "SHIBBOLETH") {
		t.Errorf("no_log leaked the secret:\n%s", out)
	}
	// The outcomes are still reported, just not their contents.
	for _, want := range []string{
		"ok: [localhost]\n",
		"changed: [localhost]\n",
		"skipping: [localhost]\n",
		// Real ansible-core's exact censored result, wording included.
		`fatal: [localhost]: FAILED! => {"censored": "the output has been hidden due to the fact that 'no_log: true' was specified for this result", "changed": true}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A task WITHOUT no_log still prints its message.
	if !strings.Contains(out, "VISIBLE") {
		t.Errorf("no_log suppressed an unrelated task:\n%s", out)
	}
}

// Without no_log the same debug task prints its message — so the test
// above cannot pass merely because nothing ran.
func TestWithoutNoLogTheMessageIsPrinted(t *testing.T) {
	out := runAndCapture(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - {name: t, debug: {msg: SHIBBOLETH}}
`)
	if !strings.Contains(out, "SHIBBOLETH") {
		t.Errorf("expected the message to be printed:\n%s", out)
	}
}

// TestUnhonouredTaskKeywordsAreNamed: a real Ansible task keyword this
// port does not support must be NAMED, not mistaken for a module. Every
// one of these used to fail with "ambiguous module", which reads like
// the playbook is malformed when it is this port that is incomplete.
func TestUnhonouredTaskKeywordsAreNamed(t *testing.T) {
	for _, kw := range []string{
		"connection: local", "remote_user: x", "port: 22", "throttle: 2",
		"check_mode: false", "diff: true", "collections: [a.b]",
		"module_defaults: {}", "timeout: 30", "any_errors_fatal: true",
		"environment: {A: b}", "ignore_unreachable: true", "delegate_facts: true",
		"debugger: never", "become_flags: -H", "become_exe: sudo",
	} {
		t.Run(kw, func(t *testing.T) {
			_, err := Parse([]byte("- {name: p, hosts: all, gather_facts: false, tasks: [{name: t, debug: {msg: x}, " + kw + "}]}\n"))
			if err == nil {
				t.Fatalf("%s parsed; it is not honoured, so it must be refused", kw)
			}
			key := strings.SplitN(kw, ":", 2)[0]
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error must name the keyword %q, got: %v", key, err)
			}
			if strings.Contains(err.Error(), "ambiguous module") {
				t.Errorf("%s still reads as a module-name clash: %v", kw, err)
			}
		})
	}
}

// A play-level environment: used to parse and then be ignored, so the
// command ran without the variable and nothing said so.
func TestPlayEnvironmentIsRefusedRatherThanIgnored(t *testing.T) {
	_, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  environment: {MY_PROBE: v}
  tasks:
    - {name: t, debug: {msg: x}}
`))
	if err == nil {
		t.Fatal("a play-level environment: must be refused, not silently ignored")
	}
	if !strings.Contains(err.Error(), "environment") {
		t.Errorf("error must name it: %v", err)
	}
}

// no_log is a TASK keyword now, so it must not read as a module.
func TestNoLogParsesAsAKeyword(t *testing.T) {
	pb, err := Parse([]byte("- {name: p, hosts: all, gather_facts: false, tasks: [{name: t, debug: {msg: x}, no_log: true}]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	task := pb[0].Tasks[0]
	if task.Module != "debug" {
		t.Errorf("Module = %q, want debug", task.Module)
	}
	if !task.NoLog {
		t.Error("NoLog was not set")
	}
}
