package playbook

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSimplePlay(t *testing.T) {
	pb, err := Parse([]byte(`
- name: a play
  hosts: all
  tasks:
    - name: say hi
      debug:
        msg: hello
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(pb) != 1 {
		t.Fatalf("len(pb) = %d", len(pb))
	}
	p := pb[0]
	if p.Name != "a play" || p.Hosts != "all" {
		t.Fatalf("play = %+v", p)
	}
	if !p.GatherFacts {
		t.Error("gather_facts should default to true")
	}
	if len(p.Tasks) != 1 {
		t.Fatalf("tasks = %v", p.Tasks)
	}
	task := p.Tasks[0]
	if task.Module != "debug" || task.Args["msg"] != "hello" {
		t.Fatalf("task = %+v", task)
	}
}

func TestParseFreeFormModule(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tasks:
    - command: echo hi
`))
	if err != nil {
		t.Fatal(err)
	}
	task := pb[0].Tasks[0]
	if task.Module != "command" || task.Args["_raw_params"] != "echo hi" {
		t.Fatalf("task = %+v", task)
	}
}

func TestParseMissingHosts(t *testing.T) {
	_, err := Parse([]byte(`
- tasks:
    - debug: {}
`))
	if err == nil {
		t.Fatal("want error for missing hosts")
	}
}

func TestParseNoModule(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  tasks:
    - name: nothing here
`))
	if err == nil {
		t.Fatal("want error for a task with no module")
	}
}

func TestParseAmbiguousModule(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  tasks:
    - command: echo hi
      shell: echo bye
`))
	if err == nil {
		t.Fatal("want error for two module keys on one task")
	}
}

func TestParseWhenString(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tasks:
    - debug: {}
      when: x == 1
`))
	if err != nil {
		t.Fatal(err)
	}
	if pb[0].Tasks[0].When != "x == 1" {
		t.Fatalf("When = %q", pb[0].Tasks[0].When)
	}
}

func TestParseWhenList(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tasks:
    - debug: {}
      when:
        - x == 1
        - y == 2
`))
	if err != nil {
		t.Fatal(err)
	}
	want := "(x == 1) and (y == 2)"
	if pb[0].Tasks[0].When != want {
		t.Fatalf("When = %q, want %q", pb[0].Tasks[0].When, want)
	}
}

func TestParseNotifyStringAndList(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tasks:
    - debug: {}
      notify: restart nginx
    - debug: {}
      notify:
        - restart nginx
        - reload cron
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(pb[0].Tasks[0].Notify) != 1 || pb[0].Tasks[0].Notify[0] != "restart nginx" {
		t.Fatalf("Notify[0] = %v", pb[0].Tasks[0].Notify)
	}
	if len(pb[0].Tasks[1].Notify) != 2 {
		t.Fatalf("Notify[1] = %v", pb[0].Tasks[1].Notify)
	}
}

func TestParseBlockRescueAlways(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tasks:
    - name: risky
      block:
        - command: might-fail
      rescue:
        - command: cleanup
      always:
        - command: notify-done
`))
	if err != nil {
		t.Fatal(err)
	}
	task := pb[0].Tasks[0]
	if !task.IsBlock() {
		t.Fatal("want IsBlock() true")
	}
	if len(task.Block) != 1 || len(task.Rescue) != 1 || len(task.Always) != 1 {
		t.Fatalf("task = %+v", task)
	}
}

func TestParseLoop(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tasks:
    - debug:
        msg: "{{ item }}"
      loop:
        - a
        - b
`))
	if err != nil {
		t.Fatal(err)
	}
	list, ok := pb[0].Tasks[0].Loop.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("Loop = %v", pb[0].Tasks[0].Loop)
	}
}

func TestParseWithItemsAliasesLoop(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tasks:
    - debug: {}
      with_items:
        - a
        - b
`))
	if err != nil {
		t.Fatal(err)
	}
	list, ok := pb[0].Tasks[0].Loop.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("Loop = %v", pb[0].Tasks[0].Loop)
	}
}

func TestParseBecomeOverride(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  become: true
  tasks:
    - debug: {}
    - command: x
      become: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if !pb[0].Become {
		t.Fatal("play become should be true")
	}
	if pb[0].Tasks[0].Become != nil {
		t.Fatal("task 0 should inherit (nil Become)")
	}
	if pb[0].Tasks[1].Become == nil || *pb[0].Tasks[1].Become != false {
		t.Fatalf("task 1 Become = %v, want explicit false", pb[0].Tasks[1].Become)
	}
}

func TestParseHandlers(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tasks: []
  handlers:
    - name: restart nginx
      command: systemctl restart nginx
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(pb[0].Handlers) != 1 || pb[0].Handlers[0].Name != "restart nginx" {
		t.Fatalf("Handlers = %+v", pb[0].Handlers)
	}
}

func TestParseInvalidYAML(t *testing.T) {
	if _, err := Parse([]byte("not: [valid")); err == nil {
		t.Fatal("want error for invalid YAML")
	}
}

func TestParseBadTaskListShape(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  tasks: "not a list"
`))
	if err == nil {
		t.Fatal("want error when tasks is not a list")
	}
}

func TestParseBadTaskItemShape(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  tasks:
    - "not a mapping"
`))
	if err == nil {
		t.Fatal("want error when a task item is not a mapping")
	}
}

func TestParseModuleUnsupportedShape(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  tasks:
    - debug: [1, 2, 3]
`))
	if err == nil {
		t.Fatal("want error for a module value that is neither map, string, nor nil")
	}
}

func TestParsePreAndPostTasks(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  pre_tasks:
    - command: pre
  tasks:
    - command: main
  post_tasks:
    - command: post
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(pb[0].Tasks) != 3 {
		t.Fatalf("Tasks = %+v", pb[0].Tasks)
	}
	if pb[0].Tasks[0].Args["_raw_params"] != "pre" ||
		pb[0].Tasks[1].Args["_raw_params"] != "main" ||
		pb[0].Tasks[2].Args["_raw_params"] != "post" {
		t.Fatalf("Tasks order wrong: %+v", pb[0].Tasks)
	}
}

func TestParseLoopControl(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tasks:
    - debug: {}
      loop: [a, b]
      loop_control:
        loop_var: my_item
`))
	if err != nil {
		t.Fatal(err)
	}
	if pb[0].Tasks[0].LoopVar != "my_item" {
		t.Fatalf("LoopVar = %q", pb[0].Tasks[0].LoopVar)
	}
}

func TestParseVarsAndSerial(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  serial: 2
  vars:
    x: 1
  tasks:
    - debug: {}
      vars:
        y: 2
`))
	if err != nil {
		t.Fatal(err)
	}
	// A YAML scalar normalises into a one-element list, as real
	// Ansible's own list-typed serial attribute does.
	if len(pb[0].Serial) != 1 || pb[0].Serial[0] != "2" {
		t.Fatalf("Serial = %q", pb[0].Serial)
	}
	if pb[0].Vars["x"] != 1 {
		t.Fatalf("play Vars = %v", pb[0].Vars)
	}
	if pb[0].Tasks[0].Vars["y"] != 2 {
		t.Fatalf("task Vars = %v", pb[0].Tasks[0].Vars)
	}
}

// TestModuleDefaults pins the module_defaults semantics MEASURED
// against real ansible-core 2.21.4, including the one that is easy to
// get backwards: a nearer level replaces an outer level's whole entry
// for a module rather than merging key by key. A play-level
// `copy: {mode, content}` under a task-level `copy: {mode}` loses the
// content there, and the task fails "src (or content) is required".
func TestModuleDefaults(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  module_defaults:
    file: {mode: "0640", owner: root}
    ansible.builtin.copy: {mode: "0604"}
  tasks:
    - {name: plain, file: {path: /a, state: touch}}
    - {name: fqcn-default-bare-task, copy: {dest: /b, content: x}}
    - name: own defaults replace the play's whole entry
      file: {path: /c, state: touch}
      module_defaults: {file: {mode: "0642"}}
    - name: block
      module_defaults: {file: {mode: "0641"}}
      block:
        - {name: inblock, file: {path: /d, state: touch}}
        - name: inblock-own
          file: {path: /e, state: touch}
          module_defaults: {file: {mode: "0600"}}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byName := map[string]Task{}
	var walk func([]Task)
	walk = func(ts []Task) {
		for _, task := range ts {
			byName[task.Name] = task
			walk(task.Block)
		}
	}
	walk(pb[0].Tasks)

	for _, tc := range []struct {
		task string
		want map[string]any
	}{
		// The play's entry, untouched.
		{"plain", map[string]any{"path": "/a", "state": "touch", "mode": "0640", "owner": "root"}},
		// An FQCN key matches a task written with the bare name.
		{"fqcn-default-bare-task", map[string]any{"dest": "/b", "content": "x", "mode": "0604"}},
		// REPLACES: owner is gone, not merged in from the play.
		{"own defaults replace the play's whole entry", map[string]any{"path": "/c", "state": "touch", "mode": "0642"}},
		{"inblock", map[string]any{"path": "/d", "state": "touch", "mode": "0641"}},
		{"inblock-own", map[string]any{"path": "/e", "state": "touch", "mode": "0600"}},
	} {
		got := byName[tc.task].argsWithDefaults()
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: args = %v, want %v", tc.task, got, tc.want)
		}
	}
}

// TestModuleDefaultsRefusesActionGroup: a "group/..." key names an
// action group, which this port has no concept of. Real Ansible warns
// and ignores it; ignoring it here would run the module with arguments
// the playbook did not ask for, so it is refused by name instead.
func TestModuleDefaultsRefusesActionGroup(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  module_defaults:
    group/foo: {mode: "0640"}
  tasks: [{file: {path: /a}}]
`))
	if err == nil || !strings.Contains(err.Error(), "action group") {
		t.Errorf("err = %v, want one naming the action group", err)
	}
}

// TestActionAndLocalAction pins the three ways real names a module
// somewhere other than its own key. All three were refused here:
// action: and args: by name, local_action: by being mistaken for a
// module ("the module local_action was not found").
func TestActionAndLocalAction(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  tasks:
    - {name: string form, action: command id -un}
    - {name: mapping form, action: {module: copy, content: hi, dest: /tmp/x}}
    - {name: local string, local_action: command id -un}
    - {name: local mapping, local_action: {module: copy, content: hi, dest: /tmp/x}}
    - {name: local keeps an explicit delegate, local_action: command id, delegate_to: elsewhere}
    - {name: bare module, command: id -un}
    - name: args sits under the module key
      command: echo from-module-key
      args: {_raw_params: echo from-args, chdir: /tmp}
`))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Task{}
	for _, task := range pb[0].Tasks {
		byName[task.Name] = task
	}
	for _, tc := range []struct {
		task, module, delegate string
		args                   map[string]any
	}{
		// The first word of the string names the module; the rest is
		// _raw_params — unlike the module-key form, where the WHOLE
		// string is _raw_params.
		{"string form", "command", "", map[string]any{"_raw_params": "id -un"}},
		{"mapping form", "copy", "", map[string]any{"content": "hi", "dest": "/tmp/x"}},
		// local_action is action plus delegate_to: localhost.
		{"local string", "command", "localhost", map[string]any{"_raw_params": "id -un"}},
		{"local mapping", "copy", "localhost", map[string]any{"content": "hi", "dest": "/tmp/x"}},
		{"local keeps an explicit delegate", "command", "elsewhere", map[string]any{"_raw_params": "id"}},
		{"bare module", "command", "", map[string]any{"_raw_params": "id -un"}},
		// args: supplies what the module key does not set, and loses
		// to it where both do — measured, the module key wins.
		{"args sits under the module key", "command", "", map[string]any{
			"_raw_params": "echo from-module-key", "chdir": "/tmp"}},
	} {
		got := byName[tc.task]
		if got.Module != tc.module || got.DelegateTo != tc.delegate {
			t.Errorf("%s: module=%q delegate=%q, want %q/%q",
				tc.task, got.Module, got.DelegateTo, tc.module, tc.delegate)
		}
		if !reflect.DeepEqual(got.Args, tc.args) {
			t.Errorf("%s: args = %#v, want %#v", tc.task, got.Args, tc.args)
		}
	}
}
