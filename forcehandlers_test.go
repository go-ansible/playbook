package playbook

import (
	"context"
	"strings"
	"testing"
)

// force_handlers runs a notified handler for a host that FAILED later in
// the play. The engine already honoured Engine.ForceHandlers (the
// --force-handlers flag); the play keyword was accepted and ignored.
//
// The keyword overrides the flag in BOTH directions, which is why Play
// holds a *bool: measured against real ansible-core 2.21.4,
//
//	keyword   flag              handler runs
//	true      -                 yes
//	false     -                 no
//	(unset)   -                 no
//	true      --force-handlers  yes
//	false     --force-handlers  NO     <- the keyword wins
//	(unset)   --force-handlers  yes    <- the flag applies
//
// All six agree with real end to end. The two marked rows are the ones a
// plain bool could not express.
func TestForceHandlers(t *testing.T) {
	const src = `
- hosts: all
  gather_facts: false
  KEYWORD
  tasks:
    - name: notify
      command: /usr/bin/true
      notify: say
    - name: fail on purpose
      command: /usr/bin/false
  handlers:
    - name: say
      debug: {msg: HANDLER-RAN}
`
	for _, c := range []struct {
		name    string
		keyword string
		flag    bool
		want    bool
	}{
		{"keyword true, no flag", "force_handlers: true", false, true},
		{"keyword false, no flag", "force_handlers: false", false, false},
		{"no keyword, no flag", "", false, false},
		{"keyword true and flag", "force_handlers: true", true, true},
		// The keyword wins over the flag...
		{"keyword false beats the flag", "force_handlers: false", true, false},
		// ...and without a keyword the flag is what decides.
		{"no keyword, flag applies", "", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			pb, err := Parse([]byte(strings.Replace(src, "KEYWORD", c.keyword, 1)))
			if err != nil {
				t.Fatal(err)
			}
			var ran bool
			e := New(localhostInventory())
			e.ForceHandlers = c.flag
			e.OnResult = func(r Result) {
				if strings.Contains(r.Msg, "HANDLER-RAN") {
					ran = true
				}
			}
			if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
				t.Fatal(err)
			}
			if ran != c.want {
				t.Errorf("handler ran = %v, want %v (keyword %q, flag %v)", ran, c.want, c.keyword, c.flag)
			}
		})
	}
}

// The CONTROL: the handler must run for a host that did NOT fail, with
// no keyword and no flag. Otherwise "it did not run" above could mean
// handlers never run at all.
func TestAHandlerRunsForAHostThatDidNotFail(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: notify
      command: /usr/bin/true
      notify: say
  handlers:
    - name: say
      debug: {msg: HANDLER-RAN}
`))
	if err != nil {
		t.Fatal(err)
	}
	var ran bool
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if strings.Contains(r.Msg, "HANDLER-RAN") {
			ran = true
		}
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("a notified handler did not run for a host that succeeded; " +
			"the force_handlers cases prove nothing while this fails")
	}
}
