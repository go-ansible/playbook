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

// ⛔ TestALookupResultIsNotTemplated is a SECOND source of untrusted
// data, distinct from the variable layers, and the brake that keys on
// untrusted NAMES could not see it:
//
//	vars:
//	  from_file: "{{ lookup('file', 'data.txt') }}"
//
// names nothing untrusted, yet pulls a file's contents into a TRUSTED
// variable. With that file holding `{{ lookup('pipe','touch X') }}`,
// measured against ansible-core 2.21.4:
//
//	real:  GOT={{ lookup('pipe','touch X') }}   and no file
//	ours:  GOT=                                 and THE FILE EXISTED
//
// Real is safe because it marks a lookup's RESULT unsafe. A lookup
// returns a file's contents, a command's output, an API's answer -- data
// by definition.
func TestALookupResultIsNotTemplated(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "fired")
	data := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(data, []byte("{{ lookup('pipe','touch "+probe+"') }}"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runPlaybookText(t, `
- name: p
  hosts: all
  gather_facts: false
  vars:
    from_file: "{{ lookup('file', '`+data+`') }}"
  tasks:
    - debug: {msg: "GOT={{ from_file }}"}
`)
	if _, err := os.Stat(probe); err == nil {
		t.Error("⛔ a file's contents were evaluated: a lookup's result was templated")
	}
	if !strings.Contains(out, "GOT={{ lookup(") {
		t.Errorf("the file's contents did not come through literally:\n%s", out)
	}
}

// The control that keeps the one above honest: an ORDINARY lookup still
// resolves. Refusing to re-render a lookup's result must not stop the
// lookup itself from running.
func TestAnOrdinaryLookupStillResolves(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(data, []byte("plain-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runPlaybookText(t, `
- name: p
  hosts: all
  gather_facts: false
  vars:
    c: "{{ lookup('file', '`+data+`') }}"
  tasks:
    - debug: {msg: "C={{ c }}"}
`)
	if !strings.Contains(out, "C=plain-content") {
		t.Errorf("an ordinary lookup stopped working:\n%s", out)
	}
}

// query() and q() are the same lookup machinery under other names, so
// they carry the same rule. Asserting only `lookup` would leave two
// spellings of the same hole open.
func TestQueryAndQAreTreatedAsLookups(t *testing.T) {
	for _, call := range []string{"query('file', '%s')", "q('file', '%s')"} {
		dir := t.TempDir()
		probe := filepath.Join(dir, "fired")
		data := filepath.Join(dir, "data.txt")
		if err := os.WriteFile(data, []byte("{{ lookup('pipe','touch "+probe+"') }}"), 0o644); err != nil {
			t.Fatal(err)
		}
		expr := strings.Replace(call, "%s", data, 1)
		runPlaybookText(t, `
- name: p
  hosts: all
  gather_facts: false
  vars:
    v: "{{ `+expr+` }}"
  tasks:
    - debug: {msg: "GOT={{ v }}"}
`)
		if _, err := os.Stat(probe); err == nil {
			t.Errorf("⛔ %s did not get the lookup rule: a file's contents were evaluated", call)
		}
	}
}

// TestIncludeVarsStillTemplates is the regression the first provenance
// rule caused, and the reason provenance is now recorded per KEY rather
// than per layer.
//
// include_vars shares the Facts layer with set_fact, so marking the whole
// layer untrusted stopped an included file's templates from resolving:
//
//	real:  Hello world
//	ours:  Hello {{ who }}
//
// Real TEMPLATES an included vars file, because it is a file the author
// named. A security brake that breaks an ordinary, common pattern does
// not survive contact with a real playbook.
func TestIncludeVarsStillTemplates(t *testing.T) {
	dir := t.TempDir()
	varsFile := filepath.Join(dir, "benign.yml")
	if err := os.WriteFile(varsFile, []byte("greeting: \"Hello {{ who }}\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runPlaybookText(t, `
- name: p
  hosts: all
  gather_facts: false
  vars: {who: world}
  tasks:
    - include_vars: `+varsFile+`
    - debug: {msg: "GOT={{ greeting }}"}
`)
	if !strings.Contains(out, "GOT=Hello world") {
		t.Errorf("an included vars file stopped resolving its templates:\n%s", out)
	}
}

// ...and a module result in the SAME layer is still untrusted, which is
// the half the per-key rule exists to keep. Asserting only the test above
// would pass just as well with the brake removed entirely.
func TestAModuleResultInTheSameLayerIsStillUntrusted(t *testing.T) {
	out := runPlaybookText(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - command: printf '%s' "{{ '{{' }} 7*7 {{ '}}' }}"
      register: r
    - set_fact: {f: "{{ r.stdout }}"}
    - debug: {msg: "GOT={{ f }}"}
`)
	if !strings.Contains(out, "GOT={{ 7*7 }}") {
		t.Errorf("a module result was evaluated although include_vars shares its layer:\n%s", out)
	}
}

// An ABSOLUTE include_vars path is used as given. filepath.Join folds it
// against BaseDir and silently drops the leading separator, so
// `include_vars: /etc/site/vars.yml` looked for "etc/site/vars.yml"
// beside the playbook. Found while writing the test above, which used an
// absolute path in a t.TempDir() and failed for this reason rather than
// the one it was written for. Measured: real reads the file.
func TestIncludeVarsAcceptsAnAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	varsFile := filepath.Join(dir, "abs.yml")
	if err := os.WriteFile(varsFile, []byte("absvar: ok-abs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(varsFile) {
		t.Fatalf("the fixture is not absolute (%s), so this test is not testing what it says", varsFile)
	}
	out := runPlaybookText(t, `
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - include_vars: `+varsFile+`
    - debug: {msg: "GOT={{ absvar }}"}
`)
	if !strings.Contains(out, "GOT=ok-abs") {
		t.Errorf("an absolute include_vars path did not resolve:\n%s", out)
	}
}
