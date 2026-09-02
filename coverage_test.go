package playbook

import (
	"context"
	"io"
	"testing"

	remoteexec "github.com/go-remoteexec/transport"
)

func TestEngineSetFactVisibleLater(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: define
      set_fact:
        my_value: 42
    - name: use it
      debug:
        msg: "{{ my_value }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	var msg string
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if r.Task == "use it" {
			msg = r.Msg
		}
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	if msg != "42" {
		t.Fatalf("msg = %q, want the set_fact value visible to a later task", msg)
	}
}

func TestEngineBecomeSudoIfAvailable(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  become: true
  tasks:
    - name: whoami
      command: whoami
      register: who
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Skip("sudo -n not available in this sandbox (expected without passwordless sudo)")
	}
}

func TestSummarySkippedCounted(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  vars:
    flag: false
  tasks:
    - debug: {}
      when: flag
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	sum := rr.Summary()["localhost"]
	if sum.Skipped != 1 || sum.Ok != 0 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestBecomeConfigForVariants(t *testing.T) {
	play := Play{Become: true, BecomeUser: "root", BecomeMethod: "sudo"}

	if _, ok := becomeConfigFor(Play{Become: false}, Task{}, nil); ok {
		t.Fatal("want disabled when neither play nor task requests become")
	}

	cfg, ok := becomeConfigFor(play, Task{}, map[string]any{})
	if !ok || cfg.User != "root" {
		t.Fatalf("cfg = %+v ok=%v, want play-level become inherited", cfg, ok)
	}

	off := false
	cfg2, ok2 := becomeConfigFor(play, Task{Become: &off}, nil)
	if ok2 {
		t.Fatalf("task-level become:false should override play-level become:true, got %+v", cfg2)
	}

	cfg3, ok3 := becomeConfigFor(play, Task{BecomeUser: "deploy"}, map[string]any{"ansible_become_method": "su"})
	if !ok3 || cfg3.User != "deploy" || cfg3.Method != remoteexec.BecomeSu {
		t.Fatalf("cfg3 = %+v ok=%v", cfg3, ok3)
	}

	cfg4, ok4 := becomeConfigFor(play, Task{}, map[string]any{"ansible_become_method": "doas", "ansible_become_password": "pw"})
	if !ok4 || cfg4.Method != remoteexec.BecomeDoas || cfg4.Password != "pw" {
		t.Fatalf("cfg4 = %+v ok=%v", cfg4, ok4)
	}
}

func TestConnectVarHelpers(t *testing.T) {
	vars := map[string]any{
		"s":   "hello",
		"i":   7,
		"i64": int64(8),
		"f":   float64(9),
		"b":   true,
	}
	if strVar(vars, "s", "def") != "hello" {
		t.Error("strVar")
	}
	if strVar(vars, "missing", "def") != "def" {
		t.Error("strVar default")
	}
	if strVar(map[string]any{"s": 5}, "s", "def") != "def" {
		t.Error("strVar wrong type falls back to default")
	}
	if intVar(vars, "i", 0) != 7 || intVar(vars, "i64", 0) != 8 || intVar(vars, "f", 0) != 9 {
		t.Error("intVar")
	}
	if intVar(vars, "missing", 42) != 42 {
		t.Error("intVar default")
	}
	if intVar(vars, "s", 42) != 42 {
		t.Error("intVar wrong type falls back to default")
	}
	if !boolVar(vars, "b", false) {
		t.Error("boolVar")
	}
	if boolVar(vars, "missing", true) != true {
		t.Error("boolVar default")
	}
	if boolVar(vars, "s", true) != true {
		t.Error("boolVar wrong type falls back to default")
	}
	if currentUser() == "" {
		t.Error("currentUser should never be empty")
	}
}

func TestParseHelpersDefaults(t *testing.T) {
	if strDefault(nil, "fallback") != "fallback" {
		t.Error("strDefault nil")
	}
	if strDefault("set", "fallback") != "set" {
		t.Error("strDefault set")
	}
	if toInt(nil) != 0 {
		t.Error("toInt nil")
	}
	if toInt(int64(5)) != 5 || toInt(float64(6)) != 6 || toInt(3) != 3 {
		t.Error("toInt variants")
	}
}

func TestDefaultConnectLocalByHostname(t *testing.T) {
	conn, err := DefaultConnect(context.Background(), "localhost", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	res, err := conn.Exec(context.Background(), "echo hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RC != 0 {
		t.Fatalf("rc = %d", res.RC)
	}
}

func TestDefaultConnectExplicitLocal(t *testing.T) {
	conn, err := DefaultConnect(context.Background(), "anything", map[string]any{"ansible_connection": "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
}

func TestSummaryFailedCounted(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - fail:
        msg: boom
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	sum := rr.Summary()["localhost"]
	if sum.Failed != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

// alwaysFailExecConn wraps a real Connection but fails every Exec — for
// exercising the "gathering facts itself failed" branch, which needs a
// connection that dials fine but then errors on the facts probe.
type alwaysFailExecConn struct{ remoteexec.Connection }

func (alwaysFailExecConn) Exec(ctx context.Context, cmd string, stdin io.Reader) (remoteexec.Result, error) {
	return remoteexec.Result{}, errFacts
}
func (alwaysFailExecConn) Close() error { return nil }

var errFacts = &factsErr{}

type factsErr struct{}

func (*factsErr) Error() string { return "facts probe boom" }

func TestEngineGatherFactsError(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tasks:
    - debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.Connect = func(ctx context.Context, hostName string, hostVars map[string]any) (remoteexec.Connection, error) {
		return alwaysFailExecConn{}, nil
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if !rr.Failed() {
		t.Fatal("want a gather_facts failure recorded")
	}
}

func TestContainsFalse(t *testing.T) {
	if contains([]string{"a", "b"}, "c") {
		t.Fatal("want false")
	}
	if !contains([]string{"a", "b"}, "b") {
		t.Fatal("want true")
	}
}

func TestEngineArgsRenderError(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - debug:
        msg: "{{ nonexistent_var | mandatory }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if !rr.Failed() {
		t.Fatal("want a render error to fail the task")
	}
}
