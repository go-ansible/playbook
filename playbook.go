// Package playbook implements Ansible's playbook execution model: plays
// over an inventory pattern, tasks with when/loop/register/notify/
// block-rescue-always, and handlers — wired to
// github.com/go-ansible/{inventory,vars,template,modules,facts} and
// github.com/go-remoteexec/transport.
package playbook

import (
	"fmt"
	"os"
	"path/filepath"

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
	VarsFiles    []string // paths, resolved relative to the playbook's directory
	Tasks        []Task
	Handlers     []Task
	Tags         []string
	Serial       int // 0 means "all hosts at once" (linear strategy default)
	Roles        []RoleRef
}

// RoleRef is one entry of a play's roles: list.
type RoleRef struct {
	Name string
	Vars map[string]any
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

	// RoleDefaults/RoleVars are set only on the synthetic block task
	// produced for a roles: entry or include_role/import_role — the
	// engine pushes them onto the RoleDefaults/RoleVars layers for the
	// duration of the block, then restores the prior layer content.
	// Nested roles (a role that itself includes another role) are not
	// scoped correctly by this single-level save/restore — documented
	// limitation, not silently wrong: it's the outer role's vars that
	// win, matching what would happen if the inner include_role simply
	// didn't reset the layer.
	RoleDefaults map[string]any
	RoleVars     map[string]any

	Block  []Task
	Rescue []Task
	Always []Task
}

// IsBlock reports whether t is a block task (block/rescue/always)
// rather than a module invocation.
func (t Task) IsBlock() bool { return t.Block != nil }

// Parse parses a playbook YAML document (a top-level list of plays).
// File-referencing directives (vars_files, roles, include_tasks,
// import_tasks, include_role, import_role) resolve their paths relative
// to the current working directory — use ParseFile when the playbook
// lives elsewhere and its includes should resolve relative to it.
func Parse(data []byte) (Playbook, error) {
	return parse(data, ".")
}

// ParseFile reads and parses the playbook at path, resolving every
// file-referencing directive relative to path's directory (matching
// ansible-playbook, which resolves roles/ and included files relative
// to the playbook file, not the current working directory).
func ParseFile(path string) (Playbook, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("playbook: %w", err)
	}
	return parse(data, filepath.Dir(path))
}

func parse(data []byte, baseDir string) (Playbook, error) {
	var raw []map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("playbook: %w", err)
	}
	ctx := parseCtx{baseDir: baseDir}
	pb := make(Playbook, 0, len(raw))
	for i, m := range raw {
		// import_playbook is a top-level entry shape distinct from a
		// play (no hosts:, just this one key) — splice the referenced
		// file's own plays in here, resolved statically at parse time
		// like import_tasks/import_role. The imported file's own
		// nested imports/roles/includes resolve relative to ITS OWN
		// directory, not this playbook's — matching real
		// ansible-playbook.
		if path, ok := m["import_playbook"]; ok {
			pathStr, ok := path.(string)
			if !ok {
				return nil, fmt.Errorf("playbook: entry %d: import_playbook: expected a file path string, got %T", i, path)
			}
			imported, err := ParseFile(filepath.Join(baseDir, pathStr))
			if err != nil {
				return nil, fmt.Errorf("playbook: entry %d: import_playbook %s: %w", i, pathStr, err)
			}
			pb = append(pb, imported...)
			continue
		}
		play, err := parsePlay(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("playbook: play %d: %w", i, err)
		}
		pb = append(pb, play)
	}
	return pb, nil
}

// parseCtx threads the playbook's base directory through parsing, for
// every directive that reads another file (vars_files, roles,
// include_tasks/import_tasks, include_role/import_role). roleHandlers,
// when non-nil, accumulates every handler discovered inside a role
// loaded anywhere in the current play (roles:, include_role,
// import_role) — parsePlay appends it to Play.Handlers once the whole
// play is parsed, since a role's handlers/main.yml is meant to be
// notifiable by any task in the play, not just the role's own tasks.
type parseCtx struct {
	baseDir      string
	roleHandlers *[]Task
}

var playReservedKeys = map[string]bool{
	"name": true, "hosts": true, "gather_facts": true, "become": true,
	"become_user": true, "become_method": true, "vars": true, "vars_files": true,
	"tasks": true, "handlers": true, "roles": true, "tags": true, "serial": true,
	"strategy": true, "pre_tasks": true, "post_tasks": true,
}

func parsePlay(ctx parseCtx, m map[string]any) (Play, error) {
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
	if strategy := str(m["strategy"]); strategy != "" && strategy != "linear" {
		return p, fmt.Errorf("play %q: strategy %q not supported (only linear)", p.Name, strategy)
	}

	var roleHandlers []Task
	ctx.roleHandlers = &roleHandlers

	p.VarsFiles = toStringList(m["vars_files"])
	for _, path := range p.VarsFiles {
		fileVars, err := loadYAMLMap(filepath.Join(ctx.baseDir, path), false)
		if err != nil {
			return p, fmt.Errorf("vars_files: %w", err)
		}
		for k, v := range fileVars {
			if _, overridden := p.Vars[k]; !overridden {
				p.Vars[k] = v
			}
		}
	}

	var tasks []Task
	if rawRoles, ok := m["roles"]; ok {
		roleTasks, err := parseRoles(ctx, rawRoles)
		if err != nil {
			return p, fmt.Errorf("roles: %w", err)
		}
		tasks = append(tasks, roleTasks...)
	}
	for _, key := range []string{"pre_tasks", "tasks", "post_tasks"} {
		list, err := parseTaskList(ctx, m[key])
		if err != nil {
			return p, fmt.Errorf("%s: %w", key, err)
		}
		tasks = append(tasks, list...)
	}
	p.Tasks = tasks

	handlers, err := parseTaskList(ctx, m["handlers"])
	if err != nil {
		return p, fmt.Errorf("handlers: %w", err)
	}
	p.Handlers = append(append([]Task{}, roleHandlers...), handlers...)

	propagateTags(p.Tags, p.Tasks)

	return p, nil
}

// propagateTags computes each task's effective tag set — its own tags
// unioned with every enclosing block's and the play's — once, at parse
// time, so the engine's tag filter (Engine.RunTags/SkipTags) can check
// a single flat list per task instead of walking ancestry at run time.
// Tag inheritance does not extend into Play.Handlers: real Ansible
// tag-filters ordinary tasks but runs a notified handler regardless of
// tags, and this port matches that by never calling propagateTags on
// handlers.
func propagateTags(inherited []string, tasks []Task) {
	for i := range tasks {
		effective := unionTags(inherited, tasks[i].Tags)
		tasks[i].Tags = effective
		if tasks[i].IsBlock() {
			propagateTags(effective, tasks[i].Block)
			propagateTags(effective, tasks[i].Rescue)
			propagateTags(effective, tasks[i].Always)
		}
	}
}

func unionTags(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, t := range append(append([]string{}, a...), b...) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func parseTaskList(ctx parseCtx, v any) ([]Task, error) {
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
		t, err := parseTask(ctx, m)
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

// includeReservedKeys are the extra keys recognized on an
// include_tasks/import_tasks/include_role/import_role task, on top of
// taskReservedKeys — they configure the include itself rather than
// being module arguments.
var includeReservedKeys = map[string]bool{
	"include_tasks": true, "import_tasks": true,
	"include_role": true, "import_role": true,
}

func parseTask(ctx parseCtx, m map[string]any) (Task, error) {
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
		list, err := parseTaskList(ctx, block)
		if err != nil {
			return t, fmt.Errorf("block: %w", err)
		}
		t.Block = list
		if t.Block == nil {
			t.Block = []Task{} // non-nil marks this as a block task even if empty
		}
		if rescue, ok := m["rescue"]; ok {
			t.Rescue, err = parseTaskList(ctx, rescue)
			if err != nil {
				return t, fmt.Errorf("rescue: %w", err)
			}
		}
		if always, ok := m["always"]; ok {
			t.Always, err = parseTaskList(ctx, always)
			if err != nil {
				return t, fmt.Errorf("always: %w", err)
			}
		}
		return t, nil
	}

	// include_tasks/import_tasks/include_role/import_role are resolved
	// statically at parse time (splicing the referenced file's tasks —
	// or a role's tasks/defaults/vars — into a synthetic block task
	// carrying this task's own when/tags/vars). Real Ansible's
	// include_tasks/include_role are dynamic (the path/role name may be
	// templated and is re-evaluated per host at run time); this port
	// treats all four identically as static includes, which covers the
	// overwhelmingly common case of a literal, untemplated path/name.
	for _, key := range []string{"include_tasks", "import_tasks"} {
		if v, ok := m[key]; ok {
			return includeTasksTask(ctx, t, key, v)
		}
	}
	for _, key := range []string{"include_role", "import_role"} {
		if v, ok := m[key]; ok {
			return includeRoleTask(ctx, t, key, v)
		}
	}

	var moduleKey string
	for k := range m {
		if taskReservedKeys[k] || includeReservedKeys[k] {
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

// includeTasksTask resolves an include_tasks/import_tasks directive
// into a synthetic block task whose body is the referenced file's task
// list, parsed relative to ctx.baseDir.
func includeTasksTask(ctx parseCtx, t Task, key string, v any) (Task, error) {
	path, ok := v.(string)
	if !ok {
		return t, fmt.Errorf("task %q: %s: expected a file path string, got %T", t.Name, key, v)
	}
	tasks, err := loadYAMLTaskFile(ctx, filepath.Join(ctx.baseDir, path), false)
	if err != nil {
		return t, fmt.Errorf("task %q: %s %s: %w", t.Name, key, path, err)
	}
	t.Block = tasks
	if t.Block == nil {
		t.Block = []Task{}
	}
	return t, nil
}

// includeRoleTask resolves an include_role/import_role directive
// (a role name string, or a mapping with name:/role: plus optional
// vars:) into the same synthetic-block shape as a play-level roles:
// entry.
func includeRoleTask(ctx parseCtx, t Task, key string, v any) (Task, error) {
	var ref RoleRef
	switch r := v.(type) {
	case string:
		ref = RoleRef{Name: r}
	case map[string]any:
		ref.Name = str(firstNonNil(r["name"], r["role"]))
		if vv, ok := r["vars"].(map[string]any); ok {
			ref.Vars = vv
		}
	default:
		return t, fmt.Errorf("task %q: %s: unsupported shape %T", t.Name, key, v)
	}
	if ref.Name == "" {
		return t, fmt.Errorf("task %q: %s: missing role name", t.Name, key)
	}
	roleT, err := roleTask(ctx, ref)
	if err != nil {
		return t, fmt.Errorf("task %q: %s: %w", t.Name, key, err)
	}
	roleT.Name = t.Name
	if roleT.Name == "" {
		roleT.Name = "role: " + ref.Name
	}
	roleT.When = t.When
	roleT.Tags = t.Tags
	return roleT, nil
}

// parseRoles resolves a play's roles: list into synthetic block tasks,
// one per role, in order.
func parseRoles(ctx parseCtx, v any) ([]Task, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("expected a list")
	}
	out := make([]Task, 0, len(list))
	for i, item := range list {
		ref, err := parseRoleRef(item)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		t, err := roleTask(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		out = append(out, t)
	}
	return out, nil
}

func parseRoleRef(v any) (RoleRef, error) {
	switch r := v.(type) {
	case string:
		return RoleRef{Name: r}, nil
	case map[string]any:
		name := str(firstNonNil(r["role"], r["name"]))
		if name == "" {
			return RoleRef{}, fmt.Errorf("missing role/name")
		}
		vars := map[string]any{}
		for k, val := range r {
			switch k {
			case "role", "name", "when", "tags":
				continue
			}
			vars[k] = val
		}
		ref := RoleRef{Name: name}
		if len(vars) > 0 {
			ref.Vars = vars
		}
		return ref, nil
	default:
		return RoleRef{}, fmt.Errorf("unsupported roles: entry type %T", v)
	}
}

// roleTask loads a role from ctx.baseDir/roles/<name> (the
// tasks/handlers/defaults/vars/main.yml convention — meta/main.yml
// dependency chaining is not read) and returns it as a synthetic block
// task. Any handlers the role defines are appended to
// ctx.roleHandlers, which parsePlay folds into Play.Handlers, since a
// role's handlers are notifiable by any task in the play.
func roleTask(ctx parseCtx, ref RoleRef) (Task, error) {
	role, err := loadRole(ctx, ref.Name)
	if err != nil {
		return Task{}, err
	}
	if len(role.Handlers) > 0 && ctx.roleHandlers != nil {
		*ctx.roleHandlers = append(*ctx.roleHandlers, role.Handlers...)
	}
	t := Task{
		Name:         "role: " + ref.Name,
		Block:        role.Tasks,
		RoleDefaults: role.Defaults,
		RoleVars:     mergedRoleVars(role.Vars, ref.Vars),
	}
	if t.Block == nil {
		t.Block = []Task{}
	}
	return t, nil
}

// roleFile is the resolved content of one role, loaded from
// roles/<name>/{tasks,handlers,defaults,vars}/main.yml under baseDir.
// Each file is optional; a missing one contributes nothing.
type roleFile struct {
	Tasks    []Task
	Handlers []Task
	Defaults map[string]any
	Vars     map[string]any
}

func loadRole(ctx parseCtx, name string) (roleFile, error) {
	dir := filepath.Join(ctx.baseDir, "roles", name)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return roleFile{}, fmt.Errorf("role %q: no such directory %s", name, dir)
	}
	var r roleFile
	var err error
	roleCtx := parseCtx{baseDir: ctx.baseDir, roleHandlers: ctx.roleHandlers}
	if r.Tasks, err = loadYAMLTaskFile(roleCtx, filepath.Join(dir, "tasks", "main.yml"), true); err != nil {
		return r, fmt.Errorf("role %q: tasks: %w", name, err)
	}
	if r.Handlers, err = loadYAMLTaskFile(roleCtx, filepath.Join(dir, "handlers", "main.yml"), true); err != nil {
		return r, fmt.Errorf("role %q: handlers: %w", name, err)
	}
	if r.Defaults, err = loadYAMLMap(filepath.Join(dir, "defaults", "main.yml"), true); err != nil {
		return r, fmt.Errorf("role %q: defaults: %w", name, err)
	}
	if r.Vars, err = loadYAMLMap(filepath.Join(dir, "vars", "main.yml"), true); err != nil {
		return r, fmt.Errorf("role %q: vars: %w", name, err)
	}
	return r, nil
}

// loadYAMLTaskFile reads and parses a task-list YAML file. optional
// controls whether a missing file is an error: true for a role's
// tasks/main.yml or handlers/main.yml (every role file is optional —
// a role may legitimately provide only vars, say), false for an
// include_tasks/import_tasks target (real Ansible errors on a missing
// include, and so does this port).
func loadYAMLTaskFile(ctx parseCtx, path string, optional bool) ([]Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if optional && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var raw []map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make([]Task, 0, len(raw))
	for i, m := range raw {
		t, err := parseTask(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("%s: item %d: %w", path, i, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// loadYAMLMap reads a YAML file into a plain map. optional controls
// whether a missing file is an error: true for a role's
// defaults/main.yml or vars/main.yml (every role file is optional),
// false for a vars_files/include_vars target (real Ansible errors on a
// missing one, and so does this port).
func loadYAMLMap(path string, optional bool) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if optional && os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func mergedRoleVars(roleVars, refVars map[string]any) map[string]any {
	if len(roleVars) == 0 && len(refVars) == 0 {
		return nil
	}
	out := make(map[string]any, len(roleVars)+len(refVars))
	for k, v := range roleVars {
		out[k] = v
	}
	for k, v := range refVars {
		out[k] = v
	}
	return out
}

// normalizeWhen joins a `when:` string or list of strings into one
// Jinja2 expression (Ansible ANDs a list of conditions together).
func normalizeWhen(v any) string {
	switch w := v.(type) {
	case string:
		return w
	case bool:
		// A literal YAML `when: true`/`when: false` (not a string) is
		// legal Ansible and common; render it as a Jinja2 boolean
		// literal so EvalBool sees it rather than treating an unset
		// When as "no condition, always run".
		if w {
			return "true"
		}
		return "false"
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
