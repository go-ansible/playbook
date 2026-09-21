package playbook

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestDebugVarMatchesRealAnsible pins `debug: {var: NAME}` against real
// ansible-core 2.21.4. Two of these were wrong: a task-level vars:
// entry read as null, and an UNDEFINED variable also read as null
// rather than saying so.
func TestDebugVarMatchesRealAnsible(t *testing.T) {
	out := runAndCapture(t, `
- name: p
  hosts: all
  gather_facts: false
  vars: {playvar: 7}
  tasks:
    - {name: play-level, debug: {var: playvar}}
    - {name: task-level, debug: {var: taskvar}, vars: {taskvar: 8}}
    - {name: registered, command: echo hi, register: r}
    - {name: show-reg,   debug: {var: r.rc}}
    - {name: undefined,  debug: {var: nothere}}
`)
	for _, want := range []string{
		`"playvar": 7`,
		// A task's own vars: must be visible to debug: var:, which
		// needs the TASK's merged view rather than the host's.
		`"taskvar": 8`,
		`"r.rc": 0`,
		// Real ansible-core 2.21's own wording. The leading 1 is a
		// fixed per-result error index, not a counter.
		`"nothere": "<< error 1 - 'nothere' is undefined >>"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"taskvar": null`) || strings.Contains(out, `"nothere": null`) {
		t.Errorf("a variable read as null:\n%s", out)
	}
}

// TestHandlerBanner pins real Ansible's "RUNNING HANDLER [...]", which
// is how a reader tells a handler run from a task of the same name.
func TestHandlerBanner(t *testing.T) {
	out := runAndCapture(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - {name: notifier, command: echo x, notify: hup}
  handlers:
    - {name: hup, debug: {msg: HANDLED}}
`)
	if !strings.Contains(out, "RUNNING HANDLER [hup]") {
		t.Errorf("handler must be bannered RUNNING HANDLER:\n%s", out)
	}
	if strings.Contains(out, "TASK [hup]") {
		t.Errorf("handler was bannered as a task:\n%s", out)
	}
	if !strings.Contains(out, "TASK [notifier]") {
		t.Errorf("ordinary task lost its banner:\n%s", out)
	}
}

// A handler sharing a task's name still gets its own banner, because
// the banner's KIND is part of what identifies it.
func TestHandlerBannerWhenNamesCollide(t *testing.T) {
	out := runAndCapture(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - {name: same, command: echo x, notify: same}
  handlers:
    - {name: same, debug: {msg: HANDLED}}
`)
	if !strings.Contains(out, "TASK [same]") || !strings.Contains(out, "RUNNING HANDLER [same]") {
		t.Errorf("both banners must appear:\n%s", out)
	}
}

func runAndCapture(t *testing.T, src string) string {
	t.Helper()
	pb, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	e := New(localhostInventory())
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
