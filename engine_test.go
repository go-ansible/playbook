package playbook

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-ansible/inventory"
)

// localhostInventory returns an inventory with one host, "localhost",
// forced to the local connection — so engine tests exercise the real
// module/template/vars machinery end to end without needing a real SSH
// target.
func localhostInventory() *inventory.Inventory {
	inv, err := inventory.ParseYAML([]byte(`
all:
  hosts:
    localhost:
      ansible_connection: local
`))
	if err != nil {
		panic(err)
	}
	return inv
}

func TestEngineSimplePlaybook(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.txt")

	pb, err := Parse([]byte(`
- name: write a file
  hosts: all
  gather_facts: false
  tasks:
    - name: create it
      copy:
        content: "hello"
        dest: ` + dest + `
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
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("content = %q", data)
	}

	sum := rr.Summary()["localhost"]
	if sum.Changed != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestEngineGatherFacts(t *testing.T) {
	pb, err := Parse([]byte(`
- name: use a fact
  hosts: all
  tasks:
    - name: show it
      debug:
        msg: "{{ ansible_system }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	var lastMsg string
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if r.Task == "show it" {
			lastMsg = r.Msg
		}
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	if lastMsg == "" {
		t.Fatal("ansible_system was not available in the task (facts not gathered/injected?)")
	}
}

func TestEngineWhenSkips(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  vars:
    flag: false
  tasks:
    - name: conditional
      debug: {}
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
	if sum.Skipped != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestEngineLoop(t *testing.T) {
	dir := t.TempDir()
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: touch files
      file:
        path: "` + dir + `/{{ item }}"
        state: touch
      loop:
        - a
        - b
        - c
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
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	for _, name := range []string{"a", "b", "c"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s not created: %v", name, err)
		}
	}
}

func TestEngineRegisterAndChangedWhen(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: probe
      command: "true"
      register: probe_result
      changed_when: false
    - name: reactive
      debug:
        msg: "probe rc was {{ probe_result.rc }}"
      when: probe_result.rc == 0
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	var reactiveRan bool
	e.OnResult = func(r Result) {
		if r.Task == "reactive" && !r.Skipped {
			reactiveRan = true
		}
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	if !reactiveRan {
		t.Fatal("reactive task did not see the registered probe_result")
	}
	sum := rr.Summary()["localhost"]
	if sum.Changed != 0 {
		t.Fatalf("changed_when:false should have suppressed changed, got %+v", sum)
	}
}

func TestEngineIgnoreErrors(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: fails
      fail:
        msg: boom
      ignore_errors: true
    - name: still runs
      debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	var stillRan bool
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if r.Task == "still runs" {
			stillRan = true
		}
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if !stillRan {
		t.Fatal("ignore_errors should let the host continue to the next task")
	}
}

func TestEngineFailureExcludesHostFromLaterTasks(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: fails
      fail:
        msg: boom
    - name: should be skipped
      debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	var laterRan bool
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if r.Task == "should be skipped" {
			laterRan = true
		}
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if !rr.Failed() {
		t.Fatal("want a failure recorded")
	}
	if laterRan {
		t.Fatal("a failed host must not run subsequent tasks")
	}
}

func TestEngineBlockRescue(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: risky
      block:
        - fail:
            msg: boom
      rescue:
        - debug:
            msg: rescued
    - name: after
      debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	var rescued, afterRan bool
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if r.Msg == "rescued" {
			rescued = true
		}
		if r.Task == "after" && !r.Skipped {
			afterRan = true
		}
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if !rescued {
		t.Fatal("rescue block did not run")
	}
	if !afterRan {
		t.Fatal("a rescued host should continue past the block")
	}
	if rr.Failed() {
		// The block's own failure IS recorded (it's history), but the
		// host itself should have recovered — checked via afterRan.
	}
}

func TestEngineBlockAlwaysRunsOnFailure(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: risky
      block:
        - fail:
            msg: boom
      always:
        - debug:
            msg: cleanup ran
`))
	if err != nil {
		t.Fatal(err)
	}
	var cleanupRan bool
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if r.Msg == "cleanup ran" {
			cleanupRan = true
		}
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if !cleanupRan {
		t.Fatal("always must run even when the block failed and had no rescue")
	}
}

func TestEngineBlockAlwaysThenStillFailed(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: risky
      block:
        - fail:
            msg: boom
      always:
        - debug: {}
    - name: after
      debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	var afterRan bool
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if r.Task == "after" && !r.Skipped {
			afterRan = true
		}
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if afterRan {
		t.Fatal("an unrescued block failure must still exclude the host afterward, even though always ran")
	}
}

func TestEngineNotifyHandler(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: change something
      debug: {}
      changed_when: true
      notify: say hi
  handlers:
    - name: say hi
      debug:
        msg: handler ran
`))
	if err != nil {
		t.Fatal(err)
	}
	var handlerRan bool
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if r.Msg == "handler ran" {
			handlerRan = true
		}
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if !handlerRan {
		t.Fatal("handler notified by a changed task should have run")
	}
}

func TestEngineHandlerNotRunWithoutChange(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: no change
      debug: {}
      changed_when: false
      notify: say hi
  handlers:
    - name: say hi
      debug:
        msg: handler ran
`))
	if err != nil {
		t.Fatal(err)
	}
	var handlerRan bool
	e := New(localhostInventory())
	e.OnResult = func(r Result) {
		if r.Msg == "handler ran" {
			handlerRan = true
		}
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if handlerRan {
		t.Fatal("handler must not run when nothing notified it")
	}
}

func TestEngineExtraVarsWinPrecedence(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  vars:
    x: play
  tasks:
    - name: show x
      debug:
        msg: "{{ x }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.ExtraVars = map[string]any{"x": "extra"}
	var msg string
	e.OnResult = func(r Result) {
		if r.Task == "show x" {
			msg = r.Msg
		}
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if msg != "extra" {
		t.Fatalf("msg = %q, want extra vars to win over play vars", msg)
	}
}

func TestEngineMultiplePlays(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - debug: {}
- hosts: all
  gather_facts: false
  tasks:
    - debug: {}
    - debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if len(rr.Plays) != 2 {
		t.Fatalf("Plays = %d", len(rr.Plays))
	}
	if len(rr.Plays[0].Results) != 1 || len(rr.Plays[1].Results) != 2 {
		t.Fatalf("results = %v / %v", rr.Plays[0].Results, rr.Plays[1].Results)
	}
}

func TestEngineBadHostPattern(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: "web[bad"
  gather_facts: false
  tasks:
    - debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	if _, err := e.RunPlaybook(context.Background(), pb); err == nil {
		t.Fatal("want error for a malformed host pattern")
	}
}

func TestEngineConnectFailureRecorded(t *testing.T) {
	inv, err := inventory.ParseYAML([]byte(`
all:
  hosts:
    unreachable.invalid:
      ansible_host: 203.0.113.254
      ansible_port: 1
      ansible_ssh_timeout: 1
      ansible_host_key_checking: false
`))
	if err != nil {
		t.Fatal(err)
	}
	pb, err := Parse([]byte(`
- hosts: all
  tasks:
    - debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(inv)
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if !rr.Failed() {
		t.Fatal("want a connect failure recorded")
	}
}
