// Package playbook implements Ansible's playbook execution model: plays
// over an inventory pattern, tasks with when/loop/register/notify/
// block-rescue-always, and handlers — wired to
// github.com/go-ansible/{inventory,vars,template,modules,facts} and
// github.com/go-remoteexec/transport.
package playbook

import (
	"errors"
	"fmt"
	"github.com/go-ansible/template"
	"github.com/go-ansible/vault"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-ansible/modules"
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
	// Serial is the rolling-update batch sizes. Each entry is either a
	// plain count ("2") or a percentage of the play's TOTAL host count
	// ("50%"); a list gives successive batch sizes, whose last entry
	// repeats until every host has run. Empty means "all hosts at
	// once", the linear strategy's default.
	//
	// Real Ansible's own `serial` is a list attribute, so a YAML scalar
	// is normalised into a one-element list here too — `serial: 2` and
	// `serial: [2]` are the same play.
	Serial []string

	// Environment is added to the environment of every command the play
	// runs. A task's own environment: is merged over it, key by key.
	Environment map[string]any

	// ModuleDefaults are per-module argument defaults, keyed by module
	// name: every task in the play that runs that module gets them
	// underneath its own arguments. A task or block may set its own,
	// which REPLACES the play's entry for that module outright rather
	// than merging key by key — measured against real ansible-core
	// 2.21.4, where a play-level `copy: {mode, content}` plus a
	// task-level `copy: {mode}` loses the content and the task fails
	// "src (or content) is required".
	ModuleDefaults map[string]map[string]any

	// IgnoreUnreachable keeps a host in the play when it cannot be
	// reached, instead of dropping it: the result still prints
	// UNREACHABLE!, the recap counts it ok+ignored rather than
	// unreachable, and the next task tries to connect again. A task
	// may override it either way.
	IgnoreUnreachable bool

	// Order is how the play's hosts are sequenced: "inventory" (the
	// default), "sorted", "reverse_sorted", "reverse_inventory" or
	// "shuffle".
	Order string

	// CheckMode forces this play into (or out of) a dry run, whatever
	// the --check flag says. Nil means "follow the flag" — which is NOT
	// the same as false, since false forces a REAL run under --check.
	CheckMode *bool

	// Connection, RemoteUser and Port are the play's connection
	// defaults — ansible's connection:, remote_user: and port:. Each is
	// overridden by the matching host variable (ansible_connection,
	// ansible_user, ansible_port), which is the precedence real Ansible
	// applies: measured with a host var of ssh against a play keyword
	// of local, where the HOST VAR won.
	Connection string
	RemoteUser string
	Port       int

	// AnyErrorsFatal stops the WHOLE play the moment any host fails,
	// rather than carrying on with the hosts that are still healthy.
	AnyErrorsFatal bool

	// MaxFailPercentage stops the play once more than this percentage
	// of the current batch has failed. Nil means no limit — which is
	// NOT the same as 0, where a single failure stops everything.
	MaxFailPercentage *float64
	Roles             []RoleRef

	// Strategy is "linear" (the default: every host finishes task N
	// before any host starts task N+1) or "free" (each host runs its
	// entire task list, and its own notified handlers, independently —
	// a slow host never holds back a fast one). Any other named
	// strategy real Ansible supports (debug, host_pinned, or a
	// strategy plugin) is rejected at parse time rather than silently
	// treated as linear.
	Strategy string

	// VarsPrompt, resolved once per play (not per host) before its
	// tasks run — see Engine.applyVarsPrompt — into an ordinary play
	// var, same scope as Vars. Real Ansible only accepts a list of
	// maps here, never a bare-string shorthand (confirmed from
	// ansible-core's own Play._load_vars_prompt/preprocess_vars: an
	// item that isn't a mapping raises a parse error there too).
	VarsPrompt []VarPrompt
}

// VarPrompt is one vars_prompt entry. Prompt defaults to Name when
// empty, Private defaults to true (confirmed from ansible-core's own
// playbook_executor.py: private = boolean(var.get("private", True)) —
// prompts hide input UNLESS private: false is explicit, the opposite
// of what the name alone might suggest). encrypt/salt/salt_size/unsafe
// (hashing the prompted value, and disabling template-escaping of it)
// are real ansible-core vars_prompt keys this port does not implement
// — accepted and parsed for shape compatibility, silently no-op'd
// rather than erroring on an unrecognized key, since a real playbook
// using only the common name/prompt/default/private/confirm subset
// (the overwhelming majority) should not need every knob wired to run.
type VarPrompt struct {
	Name    string
	Prompt  string
	Default string
	Private bool
	Confirm bool
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
	Name   string
	Module string
	Args   map[string]any
	When   string // Jinja2 expression, already normalized from a string or []string
	Loop   any    // a literal list, or a "{{ expr }}" string rendered at run time

	// LoopWith names the lookup plugin a with_<name>: key asked for,
	// empty for a plain loop:. Real routes both through the same
	// place — with_items IS the items lookup — and so does this.
	LoopWith string
	LoopVar  string // default "item"
	IndexVar string // loop_control.index_var — unset means no index variable

	// LoopPause is loop_control.pause — seconds to wait between
	// iterations. Reported in ansible_failed_task, where real carries
	// it as a float.
	LoopPause float64

	// LoopLabel is loop_control.label — what real prints in the
	// `(item=...)` of each iteration INSTEAD of the item itself, so a
	// loop over big dicts stays readable. It is a template, rendered
	// per item.
	LoopLabel    string
	Register     string
	IgnoreErrors bool
	ChangedWhen  string
	FailedWhen   string
	Tags         []string
	Become       *bool // nil means "inherit the play's setting"
	BecomeUser   string
	Notify       []string

	// Listen are the notify TOPICS this handler answers to, on top of
	// its own name. Only meaningful on a handler.
	Listen     []string
	Vars       map[string]any
	DelegateTo string

	// Until/Retries/Delay implement the task retry loop: real Ansible
	// runs the task 1+Retries times (Retries nil, the "unset" state,
	// means no retry loop at all UNLESS Until is non-empty, in which
	// case real Ansible defaults Retries to 3 — mirrored in
	// runTaskOnHost, not here, since it needs to distinguish "Retries
	// explicitly 0" from "Retries unset"), re-checking Until (or, if
	// Until is empty but Retries was explicitly set, "not failed")
	// after each attempt and sleeping Delay seconds before the next one
	// if it didn't pass. See runTaskOnHost's retry loop for the exact
	// attempt-counting algorithm, including a real, deliberately
	// reproduced quirk in ansible-core's own retry loop.
	Until   string
	Retries *int
	Delay   float64

	// RunOnce restricts execution to the first currently-active host in
	// a runSingleTask call, broadcasting any Register result to every
	// other active host afterward (so a later task on ANY host can
	// still read it by bare name) — matching real Ansible's own
	// run_once, including its result-sharing, verified against a real
	// ansible-playbook run. A real, narrower limitation under strategy:
	// free: each host there calls runSingleTask with itself as the only
	// active host (see runFree), so there is no cross-host "first one"
	// to restrict to — every host still runs its own copy. Real Ansible
	// coordinates run_once across free's independent per-host lanes;
	// this port does not, and says so here rather than silently
	// re-running the task on every host without comment.
	// RunOnce is tri-state, as it is there: nil means the playbook
	// never mentioned it, which ansible_failed_task reports as null
	// rather than false.
	RunOnce *bool

	// Async/Poll implement async:/poll: — see runTaskOnHost's async
	// branch and modules.AsyncLaunch/AsyncCheck for the real mechanism
	// and its one disclosed limitation (no active kill on timeout).
	// Async <= 0 means synchronous, ordinary execution — the
	// overwhelming majority of tasks. Only command/shell support
	// Async > 0 at all: every other module's work happens as a
	// sequence of calls from the control node, not one remote
	// invocation that could be backgrounded on the target the way
	// async requires — a task on any other module with Async > 0 fails
	// loud rather than silently running synchronously and ignoring
	// what was explicitly asked for. Poll nil means "unset": defaults
	// to 15 seconds (real Ansible's own DEFAULT_POLL_INTERVAL) when
	// Async > 0; Poll 0 is fire-and-forget (the task returns immediately
	// once the job is launched, for a later task to check via
	// async_status); Poll > 0 waits, checking every Poll seconds, until
	// the job finishes or Async seconds pass (a timeout failure).
	Async int
	Poll  *int

	// RoleDefaults/RoleVars are set only on the synthetic block task
	// produced for a roles: entry or include_role/import_role — the
	// engine (Engine.pushRoleVars) merges them on top of the
	// RoleDefaults/RoleVars layers' current content for the duration of
	// the block, then restores the prior (pre-merge) content. Nesting
	// (a role that itself includes another role) composes correctly to
	// any depth this way: a variable the inner role doesn't define in
	// its own defaults/main.yml or vars/main.yml still resolves to
	// whatever the enclosing role(s) already had, matching real
	// Ansible's behavior of keeping every currently active role's
	// defaults/vars in scope at once.
	RoleDefaults map[string]any
	RoleVars     map[string]any

	// RoleVarsScoped limits RoleDefaults/RoleVars to this role's own
	// block, unwinding them when it ends. True only for include_role,
	// which is dynamic: real Ansible resolves it at run time and its
	// variables leave scope with it. A roles: entry and import_role are
	// both static — real Ansible injects their variables for the whole
	// play, so they persist. Measured against real ansible-core 2.21.4
	// by running include_role and import_role in isolation: after the
	// former the role's vars read as undefined, after the latter they
	// still resolve.
	RoleVarsScoped bool

	// IncludedFile is set on the synthetic block an include_tasks
	// parses into, and holds the resolved path of the file it pulled
	// in. Real Ansible announces a dynamic include — a banner plus
	// "included: <path> for <host>", counted as ok in the recap — and
	// announces nothing for a static import; this carries what that
	// line needs.
	IncludedFile string

	// RoleDir is the directory of the role this task came from, empty
	// for a task written directly in a playbook. A relative src: on a
	// file-carrying module resolves against it — real Ansible looks in
	// the role's own files/ (or templates/ for template) before
	// anything else, which is what makes "src: hello.txt" work inside a
	// role at all.
	RoleDir string

	// Environment is added to the environment of the command this task
	// runs, merged over its play's — the task's entry wins on a key
	// both set.
	Environment map[string]any

	// ModuleDefaults is this task's EFFECTIVE set of per-module
	// argument defaults — its own, over any enclosing block's, over
	// the play's, resolved once at parse time by
	// propagateModuleDefaults. Applying it is argsWithDefaults's job.
	ModuleDefaults map[string]map[string]any

	// IgnoreUnreachable overrides the play's for this task; nil means
	// inherit it. Real Ansible decides per TASK, which is what makes
	// an ignored-unreachable ping followed by an ordinary command
	// report UNREACHABLE twice and drop the host only on the second.
	IgnoreUnreachable *bool

	// CheckMode forces this task into (or out of) a dry run. Setting it
	// FALSE is the useful case: real Ansible runs such a task for real
	// even under --check, which is how a playbook reads state it needs
	// in order to predict the rest.
	CheckMode *bool

	// AnyErrorsFatal stops the whole play when THIS task fails on any
	// host, whatever the play's own setting.
	AnyErrorsFatal bool

	// NoLog hides this task's result. Real Ansible replaces the whole
	// result with a single `censored` key, keeping only `changed`, so a
	// task handling a credential cannot leak it through the callback —
	// including when it FAILS, which is when a result is dumped in full.
	NoLog bool

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
	return parse(data, ".", "")
}

// ParseFile reads and parses the playbook at path, resolving every
// file-referencing directive relative to path's directory (matching
// ansible-playbook, which resolves roles/ and included files relative
// to the playbook file, not the current working directory).
func ParseFile(path string) (Playbook, error) {
	return ParseFileWithVault(path, "")
}

// ParseFileWithVault is ParseFile with a vault password, so the playbook
// or any file it pulls in — a vars_files target, a role's own
// defaults/vars/tasks — may be vault-encrypted. An empty password
// behaves exactly like ParseFile.
func ParseFileWithVault(path, vaultPassword string) (Playbook, error) {
	data, err := readMaybeEncrypted(path, vaultPassword)
	if err != nil {
		return nil, err
	}
	return parse(data, filepath.Dir(path), vaultPassword)
}

func parse(data []byte, baseDir, vaultPassword string) (Playbook, error) {
	var raw []map[string]any
	if err := vault.UnmarshalYAML(data, vaultPassword, &raw); err != nil {
		return nil, fmt.Errorf("playbook: %w", err)
	}
	ctx := parseCtx{baseDir: baseDir, vaultPassword: vaultPassword}
	pb := make(Playbook, 0, len(raw))
	for i, m := range raw {
		m = normalizeKeys(m)
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

	// vaultPassword decrypts any file this parse reads that turns out to
	// be vault-encrypted — the playbook itself, a vars_files target, a
	// role's defaults/vars/tasks. Empty means no password was supplied,
	// and an encrypted file is then a clear error.
	vaultPassword string

	// roleStack is the chain of roles currently being resolved, used to
	// break a dependency cycle rather than recurse until the stack dies.
	roleStack []string
}

var playReservedKeys = map[string]bool{
	"name": true, "hosts": true, "gather_facts": true, "become": true,
	"become_user": true, "become_method": true, "vars": true, "vars_files": true,
	"tasks": true, "handlers": true, "roles": true, "tags": true, "serial": true,
	"strategy": true, "pre_tasks": true, "post_tasks": true, "vars_prompt": true,
	"module_defaults": true, "ignore_unreachable": true,
}

func parsePlay(ctx parseCtx, m map[string]any) (Play, error) {
	p := Play{
		Name:              str(m["name"]),
		Hosts:             str(m["hosts"]),
		GatherFacts:       boolDefault(m["gather_facts"], true),
		Become:            boolDefault(m["become"], false),
		BecomeUser:        strDefault(m["become_user"], "root"),
		BecomeMethod:      strDefault(m["become_method"], "sudo"),
		Vars:              toMap(m["vars"]),
		Tags:              toStringList(m["tags"]),
		Serial:            toSerialList(m["serial"]),
		Environment:       toMap(m["environment"]),
		Order:             str(m["order"]),
		CheckMode:         toBoolPtr(m["check_mode"]),
		Connection:        str(m["connection"]),
		RemoteUser:        str(m["remote_user"]),
		Port:              toInt(m["port"]),
		AnyErrorsFatal:    boolDefault(m["any_errors_fatal"], false),
		IgnoreUnreachable: boolDefault(m["ignore_unreachable"], false),
		MaxFailPercentage: toFloatPtr(m["max_fail_percentage"]),
	}
	if p.Hosts == "" {
		return p, fmt.Errorf("play %q: missing required field: hosts", p.Name)
	}
	var mdErr error
	if p.ModuleDefaults, mdErr = toModuleDefaults(m["module_defaults"]); mdErr != nil {
		return p, fmt.Errorf("play %q: module_defaults: %w", p.Name, mdErr)
	}
	p.Strategy = strDefault(m["strategy"], "linear")
	if p.Strategy != "linear" && p.Strategy != "free" {
		return p, fmt.Errorf("play %q: strategy %q not supported (only linear, free)", p.Name, p.Strategy)
	}

	var roleHandlers []Task
	ctx.roleHandlers = &roleHandlers

	p.VarsFiles = toStringList(m["vars_files"])
	for _, path := range p.VarsFiles {
		fileVars, err := loadYAMLMap(filepath.Join(ctx.baseDir, path), false, ctx.vaultPassword)
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
	propagateModuleDefaults(p.ModuleDefaults, p.Tasks)
	propagateModuleDefaults(p.ModuleDefaults, p.Handlers)

	if rawPrompts, ok := m["vars_prompt"]; ok {
		p.VarsPrompt, err = parseVarsPrompt(rawPrompts)
		if err != nil {
			return p, fmt.Errorf("vars_prompt: %w", err)
		}
	}

	return p, nil
}

// parseVarsPrompt matches ansible-core's own Play._load_vars_prompt +
// preprocess_vars exactly: a single mapping is treated as a one-item
// list, but every item must be a mapping with at least "name" — there
// is no bare-string shorthand, an item that isn't a mapping is a parse
// error there too, not silently accepted.
func parseVarsPrompt(v any) ([]VarPrompt, error) {
	var items []any
	switch val := v.(type) {
	case []any:
		items = val
	case map[string]any:
		items = []any{val}
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("expected a list or a mapping, got %T", v)
	}
	out := make([]VarPrompt, 0, len(items))
	for i, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("item %d: expected a mapping, got %T", i, item)
		}
		name := str(m["name"])
		if name == "" {
			return nil, fmt.Errorf("item %d: missing required key: name", i)
		}
		out = append(out, VarPrompt{
			Name:    name,
			Prompt:  str(m["prompt"]),
			Default: str(m["default"]),
			Private: boolDefault(m["private"], true),
			Confirm: boolDefault(m["confirm"], false),
		})
	}
	return out, nil
}

// propagateTags computes each task's effective tag set — its own tags
// unioned with every enclosing block's and the play's — once, at parse
// time, so the engine's tag filter (Engine.RunTags/SkipTags) can check
// a single flat list per task instead of walking ancestry at run time.
// Tag inheritance does not extend into Play.Handlers: real Ansible
// tag-filters ordinary tasks but runs a notified handler regardless of
// tags, and this port matches that by never calling propagateTags on
// handlers.
// toModuleDefaults parses a module_defaults: value into per-module
// argument maps. Real Ansible accepts either one mapping or a LIST of
// mappings (merged in order, a later one replacing an earlier one's
// whole entry for a module), and module names may be given
// fully-qualified — a play-level "ansible.builtin.copy" default was
// measured applying to a task written as bare "copy", so both sides are
// normalized to the short name here.
//
// A "group/..." key is REFUSED rather than ignored: it names an action
// group, which this port has no concept of, so honouring the playbook
// as written is impossible and silently dropping those defaults would
// run the module with arguments the playbook did not ask for. Real
// Ansible warns and ignores it when the action groups are unavailable.
func toModuleDefaults(v any) (map[string]map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	var entries []any
	switch val := v.(type) {
	case []any:
		entries = val
	case map[string]any:
		entries = []any{val}
	default:
		return nil, fmt.Errorf("want a mapping or a list of mappings, got %T", v)
	}
	out := map[string]map[string]any{}
	for _, entry := range entries {
		em, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("want a mapping, got %T", entry)
		}
		for name, raw := range em {
			if strings.HasPrefix(name, "group/") {
				return nil, fmt.Errorf("%q names an action group, which this port does not support", name)
			}
			args, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%q: want a mapping of arguments, got %T", name, raw)
			}
			// A nearer entry REPLACES the whole per-module map rather
			// than merging into it — Python's own dict.update at this
			// level, confirmed by measurement.
			out[modules.NormalizeName(name)] = args
		}
	}
	return out, nil
}

// propagateModuleDefaults pushes outer module_defaults down the task
// tree: each task ends up holding its own effective set, its own
// entries winning over the enclosing block's and the play's, per module
// name. Real Ansible walks the parent chain at run time instead; every
// include in this port is resolved statically at parse time, so doing
// it once here reaches exactly the same tasks.
func propagateModuleDefaults(outer map[string]map[string]any, tasks []Task) {
	for i := range tasks {
		merged := outer
		if len(tasks[i].ModuleDefaults) > 0 || len(outer) > 0 {
			merged = make(map[string]map[string]any, len(outer)+len(tasks[i].ModuleDefaults))
			for name, args := range outer {
				merged[name] = args
			}
			for name, args := range tasks[i].ModuleDefaults {
				merged[name] = args
			}
			tasks[i].ModuleDefaults = merged
		}
		propagateModuleDefaults(merged, tasks[i].Block)
		propagateModuleDefaults(merged, tasks[i].Rescue)
		propagateModuleDefaults(merged, tasks[i].Always)
	}
}

// argsWithDefaults returns the task's arguments with its effective
// module_defaults underneath — a default applies only where the task
// does not set that key itself. The result is what gets Jinja2-rendered,
// so a templated default ("mode: {{ m }}") resolves exactly as a
// templated argument does.
func (t Task) argsWithDefaults() map[string]any {
	defaults := t.ModuleDefaults[modules.NormalizeName(t.Module)]
	if len(defaults) == 0 {
		return t.Args
	}
	out := make(map[string]any, len(defaults)+len(t.Args))
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range t.Args {
		out[k] = v
	}
	return out
}

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
	"become_method": true, "notify": true, "listen": true, "action": true, "args": true, "local_action": true, "vars": true, "delegate_to": true,
	"block": true, "rescue": true, "always": true, "with_items": true,
	"until": true, "retries": true, "delay": true, "run_once": true,
	"async": true, "poll": true,

	// Honoured, and added here so they are not mistaken for a module
	// name — which is what a key this parser does not know becomes.
	"no_log": true, "environment": true, "any_errors_fatal": true,
	"module_defaults": true, "ignore_unreachable": true,
	"check_mode": true,
}

// unhonouredTaskKeys are real Ansible task keywords this port PARSES but
// does not act on. They are listed so a playbook using one gets an
// error that names it, rather than the "ambiguous module" that any
// unknown key used to produce — a key the parser does not know is
// treated as the module name, so `no_log: true` beside `debug:` read as
// two modules.
//
// They are rejected rather than ignored. Silently accepting
// `connection: local` or `environment:` on a task would mean running
// something OTHER than what the playbook says, which is worse than
// refusing; the keyword list real Ansible exposes
// (ansible.playbook.task.Task.fattributes, 42 entries) is the source of
// this list.
var unhonouredTaskKeys = map[string]string{
	"async_val":      "use async:",
	"become_exe":     "",
	"become_flags":   "",
	"collections":    "fully-qualified module names resolve without it",
	"connection":     "set it on the PLAY, which this port honours, or ansible_connection on the host",
	"debugger":       "",
	"delegate_facts": "",
	"diff":           "use the --diff flag, which this port honours",
	"loop_with":      "use loop: or with_items:",
	"port":           "set it on the PLAY, which this port honours, or ansible_port on the host",
	"remote_user":    "set it on the PLAY, which this port honours, or ansible_user on the host",
	"throttle":       "use serial: on the play, which this port honours",
	"timeout":        "",
}

// includeReservedKeys are the extra keys recognized on an
// include_tasks/import_tasks/include_role/import_role task, on top of
// taskReservedKeys — they configure the include itself rather than
// being module arguments.
var includeReservedKeys = map[string]bool{
	"include_tasks": true, "import_tasks": true,
	"include_role": true, "import_role": true,
}

// normalizeKeys returns m with every key carrying a known collection
// prefix (see modules.NormalizeName — ansible.legacy./ansible.builtin./
// ansible.posix./community.general.) renamed to its short form, so
// "ansible.builtin.include_tasks"/"community.general.ufw" are recognized
// the same way "include_tasks"/"ufw" are. Every one of the nine
// playbook-engine directives (include_tasks/import_tasks/include_role/
// import_role/import_playbook/meta/add_host/group_by/include_vars) is
// matched by an exact map-key or task.Module string elsewhere in this
// package and in engine.go, entirely outside modules.Registry — so
// Registry's own FQCN fallback (modules.Get) never sees these, and each
// site would otherwise need its own repeated FQCN check. Applying this
// once, wherever a task or play-list entry's raw map is first
// inspected, covers all of them (and every ordinary module reference)
// from one place. Returns m itself unchanged when no key needs
// renaming, to avoid an allocation on the overwhelmingly common case.
func normalizeKeys(m map[string]any) map[string]any {
	out := m
	renamed := false
	for k, v := range m {
		if short := modules.NormalizeName(k); short != k {
			if !renamed {
				out = make(map[string]any, len(m))
				for k2, v2 := range m {
					out[k2] = v2
				}
				renamed = true
			}
			delete(out, k)
			out[short] = v
		}
	}
	return out
}

// moduleFromValue reads the module name and arguments out of an
// action:/local_action: value, which carries both. Real accepts two
// shapes and so does this: a bare string whose FIRST WORD is the
// module and whose remainder is _raw_params ("command id -un"), and a
// mapping whose "module" key names it and whose other keys are the
// arguments.
func moduleFromValue(v any) (string, map[string]any, error) {
	switch val := v.(type) {
	case string:
		name, rest, _ := strings.Cut(strings.TrimSpace(val), " ")
		if name == "" {
			return "", nil, errors.New("names no module")
		}
		// The remainder is k=v, exactly as it is after a module key:
		// real runs parse_kv here too (parsing/mod_args.py). Taking the
		// whole remainder as _raw_params made `action: debug msg=x`
		// print debug's default message instead of x.
		args, err := parseKV(strings.TrimSpace(rest), freeformActions[name])
		if err != nil {
			return "", nil, err
		}
		if _, hasRaw := args["_raw_params"]; hasRaw && !rawParamModules[name] {
			return "", nil, fmt.Errorf("Action %q does not support raw params.", name)
		}
		return name, args, nil
	case map[string]any:
		name, _ := val["module"].(string)
		if name == "" {
			return "", nil, errors.New(`needs a "module" key naming the module`)
		}
		args := map[string]any{}
		for k, av := range val {
			if k != "module" {
				args[k] = av
			}
		}
		return name, args, nil
	default:
		return "", nil, fmt.Errorf("want a string or a mapping, got %T", v)
	}
}

// withExtraArgs folds an args: mapping into the task's arguments.
// Measured: it sits UNDER the module key's own arguments — a task
// with `command: echo from-module-key` and `args: {_raw_params: echo
// from-args}` runs the former — so it supplies what the module key
// does not set rather than overriding it.
func withExtraArgs(t Task, m map[string]any) (Task, error) {
	raw, ok := m["args"]
	if !ok {
		return t, nil
	}
	extra, ok := raw.(map[string]any)
	if !ok {
		return t, fmt.Errorf("task %q: args: want a mapping, got %T", t.Name, raw)
	}
	merged := make(map[string]any, len(extra)+len(t.Args))
	for k, v := range extra {
		merged[k] = v
	}
	for k, v := range t.Args {
		merged[k] = v
	}
	t.Args = merged
	return t, nil
}

func parseTask(ctx parseCtx, m map[string]any) (Task, error) {
	m = normalizeKeys(m)
	t := Task{
		Name:              str(m["name"]),
		When:              normalizeWhen(m["when"]),
		Loop:              m["loop"],
		LoopVar:           "item",
		Register:          str(m["register"]),
		IgnoreErrors:      boolDefault(m["ignore_errors"], false),
		ChangedWhen:       str(m["changed_when"]),
		FailedWhen:        str(m["failed_when"]),
		Tags:              toStringList(m["tags"]),
		BecomeUser:        str(m["become_user"]),
		Notify:            toStringList(m["notify"]),
		Listen:            toStringList(m["listen"]),
		Vars:              toMap(m["vars"]),
		DelegateTo:        str(m["delegate_to"]),
		Until:             normalizeWhen(m["until"]),
		Delay:             floatDefault(m["delay"], 5),
		NoLog:             boolDefault(m["no_log"], false),
		Environment:       toMap(m["environment"]),
		AnyErrorsFatal:    boolDefault(m["any_errors_fatal"], false),
		CheckMode:         toBoolPtr(m["check_mode"]),
		IgnoreUnreachable: toBoolPtr(m["ignore_unreachable"]),
		Async:             toInt(m["async"]),
	}
	if v, ok := m["run_once"]; ok {
		b := boolDefault(v, false)
		t.RunOnce = &b
	}
	var mdErr error
	if t.ModuleDefaults, mdErr = toModuleDefaults(m["module_defaults"]); mdErr != nil {
		return t, fmt.Errorf("task %q: module_defaults: %w", t.Name, mdErr)
	}
	if v, ok := m["retries"]; ok {
		n := toInt(v)
		t.Retries = &n
	}
	if v, ok := m["poll"]; ok {
		n := toInt(v)
		t.Poll = &n
	}
	if lc, ok := m["loop_control"].(map[string]any); ok {
		if lv := str(lc["loop_var"]); lv != "" {
			t.LoopVar = lv
		}
		if iv := str(lc["index_var"]); iv != "" {
			t.IndexVar = iv
		}
		t.LoopPause = floatDefault(lc["pause"], 0)
		t.LoopLabel = str(lc["label"])
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

	// with_<name>: is the <name> lookup plugin, wantlist forced on —
	// with_items is the items lookup, with_dict the dict one, and so
	// on. Recognised by asking the template engine whether such a
	// plugin exists, so a module whose name merely begins with
	// "with_" is still a module.
	for k, v := range m {
		name, ok := strings.CutPrefix(k, "with_")
		if !ok || !lookupNameExists(name) {
			continue
		}
		if t.Loop != nil {
			return t, fmt.Errorf("task %q: both loop: and %s: present", t.Name, k)
		}
		t.Loop, t.LoopWith = v, name
	}

	// action:/local_action: name the module INSIDE their value rather
	// than as their own key. local_action is exactly action plus
	// delegate_to: localhost — measured: its results banner
	// "[h1 -> localhost]", the same as any delegated task's.
	for _, key := range []string{"action", "local_action"} {
		v, ok := m[key]
		if !ok {
			continue
		}
		name, args, err := moduleFromValue(v)
		if err != nil {
			return t, fmt.Errorf("task %q: %s: %w", t.Name, key, err)
		}
		t.Module, t.Args = name, args
		if key == "local_action" && t.DelegateTo == "" {
			t.DelegateTo = "localhost"
		}
		return withExtraArgs(t, m)
	}

	var moduleKey string
	for k := range m {
		if taskReservedKeys[k] || includeReservedKeys[k] {
			continue
		}
		if name, ok := strings.CutPrefix(k, "with_"); ok && lookupNameExists(name) {
			continue // handled above, as a loop
		}
		// A real Ansible keyword this port does not honour is named as
		// such. Without this it would be taken for the module.
		if hint, known := unhonouredTaskKeys[k]; known {
			msg := fmt.Sprintf("task %q: %q is a real Ansible task keyword that this port does not support yet", t.Name, k)
			if hint != "" {
				msg += " — " + hint
			}
			return t, errors.New(msg)
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
		// The k=v form. It used to become _raw_params whole, so `file:
		// path=/tmp/x state=touch` reached the module with no path --
		// see parsekv.go. checkRaw is on only for the modules whose
		// argument string is a command line.
		args, err := parseKV(v, freeformActions[t.Module])
		if err != nil {
			return t, fmt.Errorf("task %q: module %q: %w", t.Name, moduleKey, err)
		}
		// Real refuses raw params for a module that does not take them,
		// rather than passing an argument the module will not read.
		if _, hasRaw := args["_raw_params"]; hasRaw && !rawParamModules[t.Module] {
			return t, fmt.Errorf("Action %q does not support raw params.", t.Module)
		}
		t.Args = args
	default:
		return t, fmt.Errorf("task %q: module %q: unsupported argument shape %T", t.Name, moduleKey, v)
	}
	return withExtraArgs(t, m)
}

// includeTasksTask resolves an include_tasks/import_tasks directive
// into a synthetic block task whose body is the referenced file's task
// list, parsed relative to ctx.baseDir.
// includeFilePath accepts both shapes real Ansible takes for an
// include's target: the bare string (include_tasks: more.yml) and the
// mapping form (include_tasks: {file: more.yml}), which also carries
// apply:/rescue-style options this port does not support. Only the
// bare string was accepted here, so the documented mapping form was a
// parse error.
func includeFilePath(v any) (string, error) {
	switch val := v.(type) {
	case string:
		return val, nil
	case map[string]any:
		file, ok := val["file"].(string)
		if !ok {
			return "", fmt.Errorf("expected a file: key naming the path, got %v", val)
		}
		for k := range val {
			if k != "file" {
				return "", fmt.Errorf("%q is not supported alongside file:", k)
			}
		}
		return file, nil
	default:
		return "", fmt.Errorf("expected a file path string or a mapping with file:, got %T", v)
	}
}

func includeTasksTask(ctx parseCtx, t Task, key string, v any) (Task, error) {
	path, err := includeFilePath(v)
	if err != nil {
		return t, fmt.Errorf("task %q: %s: %w", t.Name, key, err)
	}
	// Real Ansible announces a DYNAMIC include with a banner and an
	// "included: <path> for <host>" line, which counts toward ok in the
	// recap; a static import announces nothing. This port is static for
	// both (see below), so the announcement is carried on the synthetic
	// block rather than produced by a run-time resolution step.
	if key == "include_tasks" {
		// Real prints the ABSOLUTE resolved path, whatever the
		// playbook was invoked as.
		t.IncludedFile = filepath.Join(ctx.baseDir, path)
		if abs, err := filepath.Abs(t.IncludedFile); err == nil {
			t.IncludedFile = abs
		}
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
	roleT.RoleVarsScoped = key == "include_role"
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
	for _, seen := range ctx.roleStack {
		if seen == ref.Name {
			return Task{}, fmt.Errorf("role %q: dependency cycle: %s", ref.Name,
				strings.Join(append(append([]string{}, ctx.roleStack...), ref.Name), " -> "))
		}
	}

	role, err := loadRole(ctx, ref.Name)
	if err != nil {
		return Task{}, err
	}
	if len(role.Handlers) > 0 && ctx.roleHandlers != nil {
		*ctx.roleHandlers = append(*ctx.roleHandlers, role.Handlers...)
	}

	// A role's meta/main.yml dependencies run BEFORE its own tasks, and
	// each carries its own defaults/vars — real Ansible resolves them
	// depth-first and this port ran them not at all. They are prepended
	// as their own synthetic role blocks so each keeps its own variable
	// layers rather than being flattened into this role's.
	block := make([]Task, 0, len(role.Deps)+len(role.Tasks))
	depCtx := ctx
	depCtx.roleStack = append(append([]string{}, ctx.roleStack...), ref.Name)
	for _, dep := range role.Deps {
		depTask, err := roleTask(depCtx, dep)
		if err != nil {
			return Task{}, fmt.Errorf("role %q: %w", ref.Name, err)
		}
		block = append(block, depTask)
	}
	block = append(block, role.Tasks...)

	t := Task{
		Name:         "role: " + ref.Name,
		Block:        block,
		RoleDefaults: role.Defaults,
		RoleVars:     mergedRoleVars(role.Vars, ref.Vars),
		RoleDir:      role.Dir,
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
	Deps     []RoleRef
	Dir      string
}

func loadRole(ctx parseCtx, name string) (roleFile, error) {
	dir := filepath.Join(ctx.baseDir, "roles", name)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return roleFile{}, fmt.Errorf("role %q: no such directory %s", name, dir)
	}
	var r roleFile
	var err error
	roleCtx := parseCtx{baseDir: ctx.baseDir, roleHandlers: ctx.roleHandlers, vaultPassword: ctx.vaultPassword}
	if r.Tasks, err = loadYAMLTaskFile(roleCtx, filepath.Join(dir, "tasks", "main.yml"), true); err != nil {
		return r, fmt.Errorf("role %q: tasks: %w", name, err)
	}
	if r.Handlers, err = loadYAMLTaskFile(roleCtx, filepath.Join(dir, "handlers", "main.yml"), true); err != nil {
		return r, fmt.Errorf("role %q: handlers: %w", name, err)
	}
	if r.Defaults, err = loadYAMLMap(filepath.Join(dir, "defaults", "main.yml"), true, ctx.vaultPassword); err != nil {
		return r, fmt.Errorf("role %q: defaults: %w", name, err)
	}
	if r.Vars, err = loadYAMLMap(filepath.Join(dir, "vars", "main.yml"), true, ctx.vaultPassword); err != nil {
		return r, fmt.Errorf("role %q: vars: %w", name, err)
	}
	if r.Deps, err = loadRoleDependencies(filepath.Join(dir, "meta", "main.yml"), ctx.vaultPassword); err != nil {
		return r, fmt.Errorf("role %q: meta: %w", name, err)
	}
	r.Dir = dir
	stampRoleDir(r.Tasks, dir)
	stampRoleDir(r.Handlers, dir)
	return r, nil
}

// loadRoleDependencies reads a role's meta/main.yml dependencies: list.
// An entry is either a bare role name or a mapping carrying name:/role:
// plus vars:, the same two shapes include_role accepts.
func loadRoleDependencies(path, vaultPassword string) ([]RoleRef, error) {
	m, err := loadYAMLMap(path, true, vaultPassword)
	if err != nil {
		return nil, err
	}
	raw, ok := m["dependencies"].([]any)
	if !ok {
		return nil, nil
	}
	var out []RoleRef
	for _, entry := range raw {
		switch d := entry.(type) {
		case string:
			out = append(out, RoleRef{Name: d})
		case map[string]any:
			ref := RoleRef{Name: str(firstNonNil(d["name"], d["role"]))}
			if vv, ok := d["vars"].(map[string]any); ok {
				ref.Vars = vv
			}
			if ref.Name == "" {
				return nil, fmt.Errorf("dependency %v: missing role name", entry)
			}
			out = append(out, ref)
		default:
			return nil, fmt.Errorf("dependency %v: unsupported shape %T", entry, entry)
		}
	}
	return out, nil
}

// stampRoleDir records the owning role's directory on every task in the
// tree, so a nested block's tasks resolve their src: the same way.
func stampRoleDir(tasks []Task, dir string) {
	for i := range tasks {
		if tasks[i].RoleDir == "" {
			tasks[i].RoleDir = dir
		}
		stampRoleDir(tasks[i].Block, dir)
		stampRoleDir(tasks[i].Rescue, dir)
		stampRoleDir(tasks[i].Always, dir)
	}
}

// loadYAMLTaskFile reads and parses a task-list YAML file. optional
// controls whether a missing file is an error: true for a role's
// tasks/main.yml or handlers/main.yml (every role file is optional —
// a role may legitimately provide only vars, say), false for an
// include_tasks/import_tasks target (real Ansible errors on a missing
// include, and so does this port).
func loadYAMLTaskFile(ctx parseCtx, path string, optional bool) ([]Task, error) {
	data, err := readMaybeEncrypted(path, ctx.vaultPassword)
	if err != nil {
		if optional && os.IsNotExist(errors.Unwrap(err)) {
			return nil, nil
		}
		return nil, err
	}
	var raw []map[string]any
	if err := vault.UnmarshalYAML(data, ctx.vaultPassword, &raw); err != nil {
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
func loadYAMLMap(path string, optional bool, vaultPassword string) (map[string]any, error) {
	data, err := readMaybeEncrypted(path, vaultPassword)
	if err != nil {
		if optional && os.IsNotExist(errors.Unwrap(err)) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	// Both shapes a secret takes: the whole file encrypted (already
	// handled by the read above), or individual !vault-tagged scalars in
	// an otherwise-readable vars file, which is the common way to keep
	// one secret beside plaintext.
	var m map[string]any
	if err := vault.UnmarshalYAML(data, vaultPassword, &m); err != nil {
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

func floatDefault(v any, def float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return def
}

// toSerialList normalises a serial: value into the list real Ansible
// keeps internally. A scalar becomes a one-element list, and every
// entry is kept as TEXT because an entry may be either a count or a
// percentage — "50%" has no integer form to coerce to.
func toSerialList(v any) []string {
	if v == nil {
		return nil
	}
	if items, ok := v.([]any); ok {
		out := make([]string, 0, len(items))
		for _, it := range items {
			if s := serialEntry(it); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if s := serialEntry(v); s != "" {
		return []string{s}
	}
	return nil
}

func serialEntry(v any) string {
	switch n := v.(type) {
	case string:
		return strings.TrimSpace(n)
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	case float64:
		return strconv.Itoa(int(n))
	}
	return ""
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

// readMaybeEncrypted reads a file and decrypts it when it turns out to be
// vault-encrypted. Every YAML file this package loads goes through here,
// because real Ansible accepts an encrypted one anywhere it accepts a
// plaintext one and the call site cannot know which it has.
func readMaybeEncrypted(path, vaultPassword string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("playbook: %w", err)
	}
	plain, err := vault.MaybeDecrypt(data, vaultPassword)
	if err != nil {
		return nil, fmt.Errorf("playbook: %s: %w", path, err)
	}
	return plain, nil
}

// ListedTasks returns the tasks ansible-playbook's --list-tasks shows
// for this play, in order: every task that runs a module, descending
// into a block's block: but NOT into its rescue: or always:.
//
// That asymmetry is real Ansible's own, measured rather than assumed —
// a block's rescue and always tasks do not appear in the listing even
// though they are part of the play and will run. Role tasks DO appear,
// because a play's roles: are expanded into its task list at parse
// time; Task.RoleDir names the role each came from.
//
// Tags on the returned tasks are already the EFFECTIVE ones, since
// parsing pushes a play's and a block's tags down into the tasks they
// contain.
func (p Play) ListedTasks() []Task {
	var out []Task
	var walk func([]Task)
	walk = func(tasks []Task) {
		for _, t := range tasks {
			if t.IsBlock() {
				walk(t.Block)
				continue
			}
			out = append(out, t)
		}
	}
	walk(p.Tasks)
	return out
}

// DisplayName is the name Ansible shows for a task, in a run-time
// banner and in --list-tasks alike: the task's own name, or its module
// when it has none, prefixed with the role it came from —
// "myrole : the task", with spaces around the colon.
//
// Exported because the banner and the listing must agree: a
// --list-tasks that named tasks differently from the run it describes
// would be worse than none.
func DisplayName(role, name, module string) string {
	if name == "" {
		name = module
	}
	if role == "" {
		return name
	}
	return role + " : " + name
}

// roleName is the role a task came from, derived from the directory
// stamped on it at parse time.
func roleName(t Task) string {
	if t.RoleDir == "" {
		return ""
	}
	return filepath.Base(t.RoleDir)
}

// toFloatPtr reads a max_fail_percentage, which is absent far more
// often than it is zero — and the two mean opposite things, so it
// cannot collapse into a plain float.
func toFloatPtr(v any) *float64 {
	var f float64
	switch n := v.(type) {
	case nil:
		return nil
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case float64:
		f = n
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return nil
		}
		f = parsed
	default:
		return nil
	}
	return &f
}

// toBoolPtr reads a tri-state boolean keyword: absent means "inherit",
// which is distinct from an explicit false.
func toBoolPtr(v any) *bool {
	b, ok := v.(bool)
	if !ok {
		return nil
	}
	return &b
}

// lookupProbe answers "is there a lookup plugin of this name?" at
// parse time, so `with_<name>` is recognised from the plugin set
// itself rather than from a list kept in step by hand. One engine for
// the package: it is asked nothing but the question above, and
// building one per task to ask it would be waste.
var lookupProbe = template.New()

func lookupNameExists(name string) bool { return name != "" && lookupProbe.HasLookup(name) }
