package playbook

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
	// timeout: and throttle: left this list when they became
	// HONOURED -- see TestTaskTimeout and TestThrottleLimitsOneTask --
	// and become_flags:/become_exe: left it for the same reason, once
	// go-remoteexec/transport v0.2.0 gave BecomeConfig the Exe and
	// Flags fields there was nowhere to put them before. See
	// TestBecomeKeywordsNoLongerRefused, which also checks they did not
	// fall back to reading as a module name on the way out.
	//
	// connection:, remote_user: and port: left it too. They were
	// refused only because a connection was built once per play and
	// could not change; now that a task's variables decide which
	// connection it gets, they are the keyword spelling of
	// ansible_connection/ansible_user/ansible_port. See
	// TestTaskConnectionKeywordIsHonoured for the measured precedence.
	//
	// The rest stay refused on purpose: silently accepting a keyword
	// this port does not honour would mean running something other than
	// what the playbook says, which is worse than saying no.
	// collections: left it too, and for a different reason from the
	// others: it is ACCEPTED and has no effect, because this registry is
	// a flat namespace by construction (NormalizeName strips every known
	// collection prefix) with 566 distinct names. No search path can
	// change which module a bare name resolves to here. It was already
	// accepted at PLAY level while being refused at task level, so the
	// rule was only half applied. See TestCollectionsIsAcceptedEverywhere.
	for _, kw := range []string{
		"delegate_facts: true",
		"debugger: never",
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

// TestEnvironmentReachesTheCommand pins environment: against real
// ansible-core 2.21.4: the play's entries apply, a task's are merged
// over them, the task wins on a key both set, and values are templated.
func TestEnvironmentReachesTheCommand(t *testing.T) {
	out := runAndCapture(t, `
- name: p
  hosts: all
  gather_facts: false
  vars: {who: world}
  environment:
    PLAY_VAR: from-play
    OVERRIDE: play-wins
  tasks:
    - name: play only
      shell: 'echo "P=[${PLAY_VAR:-unset}] O=[${OVERRIDE:-unset}] T=[${TASK_VAR:-unset}]"'
      register: a
    - name: task adds and overrides
      shell: 'echo "P=[${PLAY_VAR:-unset}] O=[${OVERRIDE:-unset}] T=[${TASK_VAR:-unset}]"'
      register: b
      environment: {TASK_VAR: from-task, OVERRIDE: task-wins}
    - name: templated
      shell: 'echo "V=[${TMPL:-unset}]"'
      register: c
      environment: {TMPL: "hello-{{ who }}"}
    - name: show
      debug:
        msg: "a={{ a.stdout }} b={{ b.stdout }} c={{ c.stdout }}"
`)
	for _, want := range []string{
		// The play's entries reach a task that sets none of its own.
		"a=P=[from-play] O=[play-wins] T=[unset]",
		// A task MERGES with the play rather than replacing it, and
		// wins on a shared key.
		"b=P=[from-play] O=[task-wins] T=[from-task]",
		// Values are templated.
		"c=V=[hello-world]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// A task with no environment: anywhere carries no wire flag, so nothing
// is prepended to its command.
func TestNoEnvironmentMeansNoPrefix(t *testing.T) {
	out := runAndCapture(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - {name: t, shell: 'echo "V=[${NOPE:-unset}]"', register: r}
    - {name: show, debug: {msg: "{{ r.stdout }}"}}
`)
	if !strings.Contains(out, "V=[unset]") {
		t.Errorf("expected an unset variable:\n%s", out)
	}
}

// TestHonouredKeywordsParseAsKeywords is the other side of
// TestUnhonouredTaskKeywordsAreNamed: these two are now acted on, so
// they must parse as keywords rather than be refused OR be mistaken
// for a module name.
func TestHonouredKeywordsParseAsKeywords(t *testing.T) {
	pb, err := Parse([]byte("- {name: p, hosts: all, gather_facts: false, tasks: [{name: t, debug: {msg: x}, timeout: 30, throttle: 2}]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	task := pb[0].Tasks[0]
	if task.Module != "debug" {
		t.Errorf("Module = %q, want debug", task.Module)
	}
	if task.Timeout != 30 {
		t.Errorf("Timeout = %d, want 30", task.Timeout)
	}
	if task.Throttle != 2 {
		t.Errorf("Throttle = %d, want 2", task.Throttle)
	}
}

// TestNoLogSuppressesTheDiff is the path TestNoLogCensorsTheResult
// missed: it covers ok, changed, skipped and failed, and --diff is a
// FIFTH place a result reaches the screen. `copy: {content: <secret>}`
// with no_log: true printed the secret in full under --diff, which is
// the one flag a reader adds when they want to see what changed.
//
// Real's mechanism is worth knowing, because it explains why its own
// v2_on_file_diff has no no_log branch: as_result_dict() replaces a
// no_log result with the keys in its PRESERVE set (_ansible_no_log,
// attempts, changed, deprecations, exception, retries, warnings) plus
// `censored`. `diff` is not among them, so there is nothing left to
// print. Measured against ansible-core 2.21.4: the no_log task prints
// no diff, and the same task with no_log removed prints the whole one,
// secret included.
func TestNoLogSuppressesTheDiff(t *testing.T) {
	const secret = "SUPER-SECRET-VALUE-9f3a"
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")

	run := func(noLog bool) string {
		if err := os.WriteFile(target, []byte("OLD CONTENT\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		src := "- name: p\n  hosts: all\n  gather_facts: false\n  tasks:\n" +
			"    - name: t\n      copy: {content: \"" + secret + "\\n\", dest: " + target + "}\n"
		if noLog {
			src += "      no_log: true\n"
		}
		pb, err := Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		e := New(localhostInventory())
		e.DiffMode = true
		e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
		if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	// The control runs FIRST and is fatal: without it, "the secret is
	// absent" could just mean the diff never rendered at all, and the
	// assertion below would pass on a port with no --diff support.
	visible := run(false)
	if !strings.Contains(visible, secret) {
		t.Fatalf("control failed: --diff did not render the secret without no_log, so its absence below proves nothing:\n%s", visible)
	}

	censored := run(true)
	if strings.Contains(censored, secret) {
		t.Errorf("no_log leaked the secret through --diff:\n%s", censored)
	}
	// The outcome is still reported; only the contents are hidden.
	if !strings.Contains(censored, "changed: [localhost]") {
		t.Errorf("the task's outcome went missing with its diff:\n%s", censored)
	}
}

// TestVerbosityDumpsTheResult pins real's -v: the ok/changed line
// carries the whole result in the COMPACT single-line form -- the same
// one a `fatal:` line uses, not the pretty 4-space block. Measured:
//
//	changed: [localhost]
//	changed: [localhost] => {"changed": true, "cmd": ["echo", "hello"], …
func TestVerbosityDumpsTheResult(t *testing.T) {
	src := `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - name: a command
      command: echo hello
`
	quiet := runAndCaptureVerbose(t, src, 0)
	loud := runAndCaptureVerbose(t, src, 1)

	if strings.Contains(quiet, "=>") {
		t.Errorf("without -v the line should carry nothing:\n%s", quiet)
	}
	if !strings.Contains(loud, `changed: [localhost] => {`) {
		t.Errorf("-v did not dump the result:\n%s", loud)
	}
	// The compact form, not the pretty one: real's -v line has no
	// newline inside the JSON.
	for _, line := range strings.Split(loud, "\n") {
		if strings.HasPrefix(line, "changed: [localhost] => {") && !strings.HasSuffix(line, "}") {
			t.Errorf("the dump is not on one line, so it is the pretty form:\n%s", line)
		}
	}
	if !strings.Contains(loud, `"stdout": "hello"`) {
		t.Errorf("-v dumped something, but not the command's own result:\n%s", loud)
	}
}

// A module that dumps at EVERY verbosity keeps its pretty block, and -v
// does not replace it with the compact one. Measured on real: at -v, a
// debug task still shows the 4-space block while a command beside it
// shows the compact dump, so the two coexist.
func TestAnAlwaysOnDumpSurvivesVerbosity(t *testing.T) {
	loud := runAndCaptureVerbose(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - name: a debug
      debug: {msg: hi}
`, 1)
	if !strings.Contains(loud, "ok: [localhost] => {\n    \"msg\": \"hi\"\n}") {
		t.Errorf("debug's pretty block did not survive -v:\n%q", loud)
	}
}

// ⛔ no_log must still win at -v, which is the whole point of putting
// the verbose dump through resultJSON rather than printing the result
// directly. Real does not reveal a no_log result at any verbosity --
// measured up to -vvv.
func TestNoLogWinsOverVerbosity(t *testing.T) {
	loud := runAndCaptureVerbose(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - name: a secret command
      command: echo SHIBBOLETH
      no_log: true
`, 1)
	if strings.Contains(loud, "SHIBBOLETH") {
		t.Errorf("-v leaked a no_log result:\n%s", loud)
	}
	if !strings.Contains(loud, "censored") {
		t.Errorf("expected the censored result at -v:\n%s", loud)
	}
	// The control: the same task WITHOUT no_log does dump the secret at
	// -v, so the absence above is no_log and not a missing dump.
	visible := runAndCaptureVerbose(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - name: a command
      command: echo SHIBBOLETH
`, 1)
	if !strings.Contains(visible, "SHIBBOLETH") {
		t.Fatalf("control failed: -v did not dump the result at all, so the test above proves nothing:\n%s", visible)
	}
}

func runAndCaptureVerbose(t *testing.T, src string, verbosity int) string {
	t.Helper()
	pb, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	cb := NewDefaultCallback(&buf, false)
	cb.Verbosity = verbosity
	e := New(localhostInventory())
	e.Callbacks = []Callback{cb}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestCollectionsIsAcceptedEverywhere pins both halves. Real accepts
// `collections:` at play AND task level (measured, both run). This port
// accepted it on a play and refused it on a task, which is the same rule
// applied in one of two places.
//
// Accepting it is not "silently ignoring a keyword": it cannot change
// what runs here. The module registry is one flat namespace by
// construction -- NormalizeName strips every known collection prefix, so
// `community.general.ufw` and a bare `ufw` are the same entry -- and no
// two modules share a bare name. A search path has nothing to search.
func TestCollectionsIsAcceptedEverywhere(t *testing.T) {
	for _, src := range []string{
		"- {name: p, hosts: all, gather_facts: false, collections: [ansible.builtin], tasks: [{name: t, debug: {msg: x}}]}\n",
		"- {name: p, hosts: all, gather_facts: false, tasks: [{name: t, collections: [ansible.builtin], debug: {msg: x}}]}\n",
	} {
		if _, err := Parse([]byte(src)); err != nil {
			t.Errorf("collections: was refused, and real accepts it here: %v", err)
		}
	}
	// And it is not read as the module name on the way through, which is
	// how an unrecognised key used to fail.
	pb, err := Parse([]byte("- {name: p, hosts: all, gather_facts: false, tasks: [{name: t, collections: [a.b], debug: {msg: x}}]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := pb[0].Tasks[0].Module; got != "debug" {
		t.Errorf("module = %q, want debug -- collections: was taken for the module", got)
	}
}
