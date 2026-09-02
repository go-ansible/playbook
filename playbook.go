// Package playbook implements Ansible's playbook execution model: plays
// over an inventory pattern, tasks with when/loop/register/notify/
// block-rescue-always, and handlers — wired to
// github.com/go-ansible/{inventory,vars,template,modules,facts} and
// github.com/go-remoteexec/transport.
package playbook

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Playbook is an ordered list of plays, as ansible-playbook reads it.
type Playbook []Play

// Play runs a set of tasks against a pattern of inventory hosts.
type Play struct {
	Name         string
	Hosts        string
	GatherFacts  bool // default true
	Become       bool
	BecomeUser   string
	BecomeMethod string
	Vars         map[string]any
	Tasks        []Task
	Handlers     []Task
	Tags         []string
	Serial       int // 0 means "all hosts at once" (linear strategy default)
}

// Task is one step of a play (or of a block's body/rescue/always).
// Module/Args are populated from whichever single non-reserved key the
// task's YAML mapping carried (e.g. `copy:` or `command:`) — empty for
// a block/meta task.
type Task struct {
	Name         string
	Module       string
	Args         map[string]any
	When         string // Jinja2 expression, already normalized from a string or []string
	Loop         any    // a literal list, or a "{{ expr }}" string rendered at run time
	LoopVar      string // default "item"
	Register     string
	IgnoreErrors bool
	ChangedWhen  string
	FailedWhen   string
	Tags         []string
	Become       *bool // nil means "inherit the play's setting"
	BecomeUser   string
	Notify       []string
	Vars         map[string]any
	DelegateTo   string

	Block  []Task
	Rescue []Task
	Always []Task
}

// IsBlock reports whether t is a block task (block/rescue/always)
// rather than a module invocation.
func (t Task) IsBlock() bool { return t.Block != nil }

// Parse parses a playbook YAML document (a top-level list of plays).
func Parse(data []byte) (Playbook, error) {
	var raw []map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("playbook: %w", err)
	}
	pb := make(Playbook, 0, len(raw))
	for i, m := range raw {
		play, err := parsePlay(m)
		if err != nil {
			return nil, fmt.Errorf("playbook: play %d: %w", i, err)
		}
		pb = append(pb, play)
	}
	return pb, nil
}

var playReservedKeys = map[string]bool{
	"name": true, "hosts": true, "gather_facts": true, "become": true,
	"become_user": true, "become_method": true, "vars": true, "vars_files": true,
	"tasks": true, "handlers": true, "roles": true, "tags": true, "serial": true,
	"strategy": true, "pre_tasks": true, "post_tasks": true,
}

func parsePlay(m map[string]any) (Play, error) {
	p := Play{
		Name:         str(m["name"]),
		Hosts:        str(m["hosts"]),
		GatherFacts:  boolDefault(m["gather_facts"], true),
		Become:       boolDefault(m["become"], false),
		BecomeUser:   strDefault(m["become_user"], "root"),
		BecomeMethod: strDefault(m["become_method"], "sudo"),
		Vars:         toMap(m["vars"]),
		Tags:         toStringList(m["tags"]),
		Serial:       toInt(m["serial"]),
	}
	if p.Hosts == "" {
		return p, fmt.Errorf("play %q: missing required field: hosts", p.Name)
	}

	var tasks []Task
	for _, key := range []string{"pre_tasks", "tasks", "post_tasks"} {
		list, err := parseTaskList(m[key])
		if err != nil {
			return p, fmt.Errorf("%s: %w", key, err)
		}
		tasks = append(tasks, list...)
	}
	p.Tasks = tasks

	handlers, err := parseTaskList(m["handlers"])
	if err != nil {
		return p, fmt.Errorf("handlers: %w", err)
	}
	p.Handlers = handlers

	return p, nil
}

func parseTaskList(v any) ([]Task, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("expected a list")
	}
	out := make([]Task, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("item %d: expected a mapping", i)
		}
		t, err := parseTask(m)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		out = append(out, t)
	}
	return out, nil
}

var taskReservedKeys = map[string]bool{
	"name": true, "when": true, "loop": true, "loop_control": true,
	"register": true, "ignore_errors": true, "changed_when": true,
	"failed_when": true, "tags": true, "become": true, "become_user": true,
	"become_method": true, "notify": true, "vars": true, "delegate_to": true,
	"block": true, "rescue": true, "always": true, "with_items": true,
}

func parseTask(m map[string]any) (Task, error) {
	t := Task{
		Name:         str(m["name"]),
		When:         normalizeWhen(m["when"]),
		Loop:         firstNonNil(m["loop"], m["with_items"]),
		LoopVar:      "item",
		Register:     str(m["register"]),
		IgnoreErrors: boolDefault(m["ignore_errors"], false),
		ChangedWhen:  str(m["changed_when"]),
		FailedWhen:   str(m["failed_when"]),
		Tags:         toStringList(m["tags"]),
		BecomeUser:   str(m["become_user"]),
		Notify:       toStringList(m["notify"]),
		Vars:         toMap(m["vars"]),
		DelegateTo:   str(m["delegate_to"]),
	}
	if lc, ok := m["loop_control"].(map[string]any); ok {
		if lv := str(lc["loop_var"]); lv != "" {
			t.LoopVar = lv
		}
	}
	if v, ok := m["become"]; ok {
		b := boolDefault(v, false)
		t.Become = &b
	}

	if block, ok := m["block"]; ok {
		list, err := parseTaskList(block)
		if err != nil {
			return t, fmt.Errorf("block: %w", err)
		}
		t.Block = list
		if t.Block == nil {
			t.Block = []Task{} // non-nil marks this as a block task even if empty
		}
		if rescue, ok := m["rescue"]; ok {
			t.Rescue, err = parseTaskList(rescue)
			if err != nil {
				return t, fmt.Errorf("rescue: %w", err)
			}
		}
		if always, ok := m["always"]; ok {
			t.Always, err = parseTaskList(always)
			if err != nil {
				return t, fmt.Errorf("always: %w", err)
			}
		}
		return t, nil
	}

	var moduleKey string
	for k := range m {
		if taskReservedKeys[k] {
			continue
		}
		if moduleKey != "" {
			return t, fmt.Errorf("task %q: ambiguous module: both %q and %q present", t.Name, moduleKey, k)
		}
		moduleKey = k
	}
	if moduleKey == "" {
		return t, fmt.Errorf("task %q: no module specified", t.Name)
	}
	t.Module = moduleKey
	switch v := m[moduleKey].(type) {
	case map[string]any:
		t.Args = v
	case nil:
		t.Args = map[string]any{}
	case string:
		t.Args = map[string]any{"_raw_params": v}
	default:
		return t, fmt.Errorf("task %q: module %q: unsupported argument shape %T", t.Name, moduleKey, v)
	}
	return t, nil
}

// normalizeWhen joins a `when:` string or list of strings into one
// Jinja2 expression (Ansible ANDs a list of conditions together).
func normalizeWhen(v any) string {
	switch w := v.(type) {
	case string:
		return w
	case []any:
		parts := make([]string, 0, len(w))
		for _, item := range w {
			parts = append(parts, "("+str(item)+")")
		}
		return joinAnd(parts)
	default:
		return ""
	}
}

func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " and "
		}
		out += p
	}
	return out
}

func firstNonNil(vs ...any) any {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func strDefault(v any, def string) string {
	if s := str(v); s != "" {
		return s
	}
	return def
}

func boolDefault(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func toMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func toStringList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, str(item))
		}
		return out
	default:
		return nil
	}
}
