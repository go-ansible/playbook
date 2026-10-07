package playbook

import (
	"context"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/go-ansible/inventory"
	remoteexec "github.com/go-remoteexec/transport"
)

// countingConnect records every dial and the variables it was given.
func countingConnect(t *testing.T, e *Engine) (calls *int, seen *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	var vars []map[string]any
	e.Connect = func(_ context.Context, _ string, hv map[string]any) (remoteexec.Connection, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		copied := map[string]any{}
		for k, v := range hv {
			copied[k] = v
		}
		vars = append(vars, copied)
		return remoteexec.NewLocal(), nil
	}
	return &n, &vars
}

// bareInventory has a host with NO connection variables at all, so the
// play's own keywords are the ONLY source of them. localhostInventory
// sets ansible_connection on the host, which hides the defect below:
// with the variable present in a task's merged vars anyway, both
// signatures agree whether or not the play keywords were folded in, and
// the test passes for the wrong reason. It did.
func bareInventory(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.ParseYAML([]byte("all:\n  hosts:\n    a-host: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// TestAnOrdinaryPlayConnectsOnce is the regression I nearly shipped. The
// play's connection:/remote_user:/port: keywords are folded into the
// variables at CONNECT time by withPlayConnection and are absent from a
// task's own merged variables -- so a signature computed without them
// differs from one computed with them for EVERY task, and the engine
// reconnected on each one. For an SSH host that is a fresh dial per task.
//
// Both sites go through ec.connSignature for that reason. Bypassing it
// on the task side alone -- the original shape -- makes this fail.
func TestAnOrdinaryPlayConnectsOnce(t *testing.T) {
	e := New(bareInventory(t))
	calls, _ := countingConnect(t, e)

	pb, err := Parse([]byte(`
- name: p
  hosts: all
  connection: local
  remote_user: someone
  gather_facts: false
  tasks:
    - {name: one, command: echo 1}
    - {name: two, command: echo 2}
    - {name: three, command: echo 3}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Errorf("connected %d times for a play that never changes connection; want 1", *calls)
	}
}

// A task that CHANGES a connection variable gets a new connection, which
// is what real does -- measured: after `set_fact: {ansible_connection:
// ssh}` the next task goes over SSH and reports UNREACHABLE, where this
// port used to keep running locally and print the command's output.
func TestChangingAConnectionVariableReconnects(t *testing.T) {
	e := New(localhostInventory())
	calls, seen := countingConnect(t, e)

	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - {name: before, command: echo before}
    - name: switch
      set_fact:
        ansible_user: someone-else
    - {name: after, command: echo after}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("connected %d times; want 2 (once, then again after the change)", *calls)
	}
	if got := (*seen)[1]["ansible_user"]; got != "someone-else" {
		t.Errorf("the second connection did not carry the new value: %v", got)
	}
}

// ...and a task that needs NO connection does not dial one. Real opens
// nothing until a task reaches for a connection, so a debug between the
// change and the next real task still reports ok. Reconnecting for every
// task gave ok=1 against real's ok=2, and made that debug fail.
func TestAConnectionlessTaskDoesNotReconnect(t *testing.T) {
	e := New(localhostInventory())
	calls, _ := countingConnect(t, e)

	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - {name: before, command: echo before}
    - name: switch
      set_fact:
        ansible_user: someone-else
    - {name: just a debug, debug: {msg: hi}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Errorf("connected %d times; a debug after the change must not dial, want 1", *calls)
	}
}

// TestConnectionVarsCoversDefaultConnect reads the bodies of the
// functions that BUILD a connection and fails if one of them consults a
// host variable connectionVars does not name.
//
// connectionVars is hand-kept, and a hand-kept list rots: a key missing
// from it means a task that changes that variable is silently ignored --
// the exact defect this feature exists to fix, reappearing quietly.
//
// It reads those FUNCTIONS and not the whole file, which the first
// version did: connect.go also holds becomeConfigFor, whose six
// ansible_become* variables are applied per task by connectionFor
// already (remoteexec.Become wraps whatever connection the task got).
// Putting them in the signature would dial a new connection every time a
// play changed become_user, for no reason -- so the exclusion is
// deliberate, and TestBecomeIsNotPartOfTheSignature pins it.
func TestConnectionVarsCoversDefaultConnect(t *testing.T) {
	bodies := ""
	for _, fn := range []struct{ file, name string }{
		{"connect.go", "DefaultConnect"},
		{"connect_winrm.go", "dialWinRM"},
		{"connect_winrm.go", "winrmConfigFor"},
	} {
		src, err := os.ReadFile(fn.file)
		if err != nil {
			continue // a file that does not exist holds no reads
		}
		body := funcBody(string(src), fn.name)
		if body == "" {
			continue
		}
		bodies += body
	}
	if bodies == "" {
		t.Fatal("read no function bodies at all; the check is broken and a pass would mean nothing")
	}

	direct := regexp.MustCompile(`hostVars\["([a-z_]+)"\]`)
	helper := regexp.MustCompile(`(?:str|int|bool)Var\(hostVars, "([a-z_]+)"`)
	found := map[string]bool{}
	for _, m := range direct.FindAllStringSubmatch(bodies, -1) {
		found[m[1]] = true
	}
	for _, m := range helper.FindAllStringSubmatch(bodies, -1) {
		found[m[1]] = true
	}
	if len(found) < 5 {
		t.Fatalf("read only %d variables, so this check is broken and a pass would mean "+
			"nothing: %v", len(found), found)
	}

	named := map[string]bool{}
	for _, k := range connectionVars {
		named[k] = true
	}
	for k := range found {
		if !named[k] {
			t.Errorf("a connection is built from %q and connectionVars does not name it, "+
				"so a task changing it would be silently ignored", k)
		}
	}
}

// funcBody returns the text of the named top-level function, from its
// signature to the closing brace in column 0.
func funcBody(src, name string) string {
	i := strings.Index(src, "func "+name+"(")
	if i < 0 {
		return ""
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// Become is applied per task on top of whatever connection the task got,
// so changing become_user must NOT dial a new connection. Pinned because
// the first version of the check above demanded exactly that.
func TestBecomeIsNotPartOfTheSignature(t *testing.T) {
	a := map[string]any{"ansible_connection": "local", "ansible_become_user": "root"}
	b := map[string]any{"ansible_connection": "local", "ansible_become_user": "deploy"}
	if connectionSignature(a) != connectionSignature(b) {
		t.Error("become_user changed the connection signature; it is layered per task, not dialled")
	}
}

// The signature ignores the variables fact gathering adds, which is what
// keeps a gather from looking like a connection change.
func TestFactsDoNotChangeTheSignature(t *testing.T) {
	base := map[string]any{"ansible_connection": "local", "ansible_user": "me"}
	withFacts := map[string]any{"ansible_connection": "local", "ansible_user": "me"}
	for _, k := range []string{"ansible_hostname", "ansible_os_family", "ansible_distribution", "ansible_env"} {
		withFacts[k] = "something"
	}
	if connectionSignature(base) != connectionSignature(withFacts) {
		t.Error("gathered facts changed the connection signature; every gather would reconnect")
	}
	// The control: a variable that DOES decide the connection changes it.
	other := map[string]any{"ansible_connection": "local", "ansible_user": "someone-else"}
	if connectionSignature(base) == connectionSignature(other) {
		t.Error("a different ansible_user produced the same signature, so nothing would ever reconnect")
	}
	if strings.Contains(connectionSignature(base), "ansible_hostname") {
		t.Error("the signature carries a fact")
	}
}
