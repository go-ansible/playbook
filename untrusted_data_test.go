package playbook

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-ansible/inventory"
	"testing"
)

// ⛔ TestTargetDataCannotRunCommandsOnTheController is the whole reason
// this exists, and it is the proof of concept rather than a proxy for it.
//
// A module result is data a MANAGED HOST chose. This port re-rendered it
// as a template on the CONTROL NODE -- and `lookup('pipe', ...)` runs on
// the control node. So a host returning
//
//	{{ lookup('pipe','touch FILE') }}
//
// from any command created that file on the controller: the machine
// holding the vault password, the fleet's SSH keys and the cloud
// credentials. Real Ansible refuses because it wraps such data in
// AnsibleUnsafeText and will not template it.
//
// The payload is a touch inside t.TempDir(), and the test asserts the
// file is ABSENT. The control below asserts the same payload DOES fire
// when it is the playbook's own text, so this absence is the brake and
// not a dud payload.
func TestTargetDataCannotRunCommandsOnTheController(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "controller-pwned")

	// Written the way a hostile target would: the command's OUTPUT is
	// the dangerous template, not the command.
	src := `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - name: the target returns a template
      command: printf '%s' "{{ '{{' }} lookup('pipe','touch ` + probe + `') {{ '}}' }}"
      register: r
    - name: an ordinary use of that data
      debug: {msg: "value={{ r.stdout }}"}
    - name: and through a fact
      set_fact: {f: "{{ r.stdout }}"}
    - debug: {msg: "fact={{ f }}"}
`
	runPlaybookText(t, src)

	if _, err := os.Stat(probe); err == nil {
		t.Fatal("⛔ a managed host's data ran a command ON THE CONTROLLER")
	}
}

// The control: the identical payload, written by the PLAYBOOK, does run.
// Without this, the test above would pass just as well against a port
// where lookup('pipe') is broken, or where the task never ran.
func TestThePayloadItselfDoesFire(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "fired")

	runPlaybookText(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - debug: {msg: "{{ lookup('pipe','touch `+probe+`') }}"}
`)
	if _, err := os.Stat(probe); err != nil {
		t.Fatalf("control failed: the payload did not fire even from the playbook's own text, "+
			"so the absence in the test above proves nothing (%v)", err)
	}
}

// Real keeps a module result LITERAL, and keeps it a string. This port
// evaluated it -- measured against ansible-core 2.21.4, a command whose
// output is the text `{{ 7*7 }}`:
//
//	real:  r.stdout == "{{ 7*7 }}"
//	ours:  r.stdout == 49          (evaluated, and no longer a string)
func TestAModuleResultIsNotTemplated(t *testing.T) {
	out := runPlaybookText(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - command: printf '%s' "{{ '{{' }} 7*7 {{ '}}' }}"
      register: r
    - debug: {msg: "GOT={{ r.stdout }}"}
    - set_fact: {f: "{{ r.stdout }}"}
    - debug: {msg: "VIA_FACT={{ f }}"}
`)
	if !strings.Contains(out, "GOT={{ 7*7 }}") {
		t.Errorf("a module result was evaluated:\n%s", out)
	}
	if !strings.Contains(out, "VIA_FACT={{ 7*7 }}") {
		t.Errorf("a module result was evaluated on its way through set_fact:\n%s", out)
	}
}

// hostvars carries every host's registered results and facts, so leaving
// it resolvable put all of that untrusted data back under a TRUSTED key.
// The proof of concept kept firing through exactly this route after the
// variable layers themselves were covered.
func TestUntrustedDataIsNotReachableThroughHostvars(t *testing.T) {
	out := runPlaybookText(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - command: printf '%s' "{{ '{{' }} 7*7 {{ '}}' }}"
      register: r
    - debug: {msg: "VIA_HOSTVARS={{ hostvars['h1']['r']['stdout'] }}"}
`)
	if !strings.Contains(out, "VIA_HOSTVARS={{ 7*7 }}") {
		t.Errorf("data reached the templater through hostvars:\n%s", out)
	}
}

// And the brake must not break ordinary chained variables, whichは what
// the first version of it did: it keyed on "the result still looks like a
// template", and a chain produces exactly that while its own inputs are
// still unresolved.
func TestOrdinaryChainedVariablesStillResolve(t *testing.T) {
	out := runPlaybookText(t, `
- name: p
  hosts: all
  gather_facts: false
  vars:
    a: base
    b: "{{ a }}/b"
    c: "{{ b }}/c"
  tasks:
    - debug: {msg: "CHAIN={{ c }}"}
`)
	if !strings.Contains(out, "CHAIN=base/b/c") {
		t.Errorf("a chained variable stopped resolving:\n%s", out)
	}
}

func runPlaybookText(t *testing.T, src string) string {
	t.Helper()
	pb, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	e := New(oneLocalHost(t))
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func oneLocalHost(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.ParseYAML([]byte("all:\n  hosts:\n    h1: {ansible_connection: local}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}
