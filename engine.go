package playbook

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-ansible/facts"
	"github.com/go-ansible/inventory"
	"github.com/go-ansible/modules"
	"github.com/go-ansible/template"
	"github.com/go-ansible/vars"
	remoteexec "github.com/go-remoteexec/transport"
)

// Engine runs playbooks against an inventory. The zero value is not
// usable — use New.
type Engine struct {
	Inventory *inventory.Inventory
	Modules   *modules.Registry
	Template  *template.Engine
	ExtraVars map[string]any
	Connect   Connector

	// BaseDir resolves include_vars' file argument at run time (every
	// other file-referencing directive — vars_files, roles,
	// include_tasks/import_tasks, include_role/import_role — resolves
	// at parse time via ParseFile's directory instead). Defaults to "."
	// when unset, matching Parse.
	BaseDir string

	// RunTags/SkipTags filter which tasks execute, matching
	// ansible-playbook's --tags/--skip-tags: a task runs if its
	// effective tag set (its own tags unioned with every enclosing
	// block's and the play's, computed once at parse time — see
	// propagateTags) intersects RunTags, or RunTags is empty, or the
	// task carries the "always" tag; and does not intersect SkipTags.
	// Filtered-out tasks are reported Skipped (real Ansible omits them
	// from output entirely — this port reports them instead, for
	// visibility). Tag filtering does not apply to handlers.
	RunTags  []string
	SkipTags []string

	// OnResult, if set, is called synchronously as each task result is
	// produced (from whichever goroutine ran that host's task) — for
	// live progress reporting. It must not block or panic; callers
	// wanting ordering should serialize themselves.
	OnResult func(Result)
}

// New returns an Engine with the built-in module registry, a fresh
// template engine, and DefaultConnect.
func New(inv *inventory.Inventory) *Engine {
	return &Engine{
		Inventory: inv,
		Modules:   modules.Default(),
		Template:  template.New(),
		Connect:   DefaultConnect,
		BaseDir:   ".",
	}
}

type hostState struct {
	name   string
	conn   remoteexec.Connection
	vc     *vars.Context
	failed bool
	notify map[string]bool
}

// execCtx bundles one play-batch's mutable execution state: each
// host's connection/vars/failure status, plus a cache of connections
// opened for delegate_to targets (shared across the batch's hosts,
// guarded by a mutex since multiple host goroutines may delegate to
// the same target concurrently).
type execCtx struct {
	engine *Engine
	play   Play
	states map[string]*hostState

	delegateMu    sync.Mutex
	delegateConns map[string]remoteexec.Connection
}

// delegateConn returns the connection to use for a delegate_to target,
// opening and caching one on first use. A delegate target that is also
// one of this batch's own hosts reuses that host's connection instead
// of dialing a second one.
func (ec *execCtx) delegateConn(ctx context.Context, hostName string, hostVars map[string]any) (remoteexec.Connection, error) {
	ec.delegateMu.Lock()
	defer ec.delegateMu.Unlock()
	if conn, ok := ec.delegateConns[hostName]; ok {
		return conn, nil
	}
	if st, ok := ec.states[hostName]; ok && st.conn != nil {
		ec.delegateConns[hostName] = st.conn
		return st.conn, nil
	}
	conn, err := ec.engine.Connect(ctx, hostName, hostVars)
	if err != nil {
		return nil, err
	}
	ec.delegateConns[hostName] = conn
	return conn, nil
}

// closeDelegates closes every delegate connection this batch opened,
// except ones reused from a batch host's own connection (those are
// closed by runBatch's own cleanup instead, once).
func (ec *execCtx) closeDelegates() {
	for name, conn := range ec.delegateConns {
		if st, ok := ec.states[name]; ok && st.conn == conn {
			continue
		}
		conn.Close()
	}
}

// RunPlaybook runs every play in pb in order.
func (e *Engine) RunPlaybook(ctx context.Context, pb Playbook) (*RunResult, error) {
	rr := &RunResult{}
	for _, play := range pb {
		pr, err := e.runPlay(ctx, play)
		if pr != nil {
			rr.Plays = append(rr.Plays, *pr)
		}
		if err != nil {
			return rr, err
		}
	}
	return rr, nil
}

func (e *Engine) runPlay(ctx context.Context, play Play) (*PlayResult, error) {
	pr := newPlayResult(play.Name)

	hosts, err := e.Inventory.Match(play.Hosts)
	if err != nil {
		return pr, fmt.Errorf("play %q: %w", play.Name, err)
	}

	// serial: splits the matched hosts into batches, each batch running
	// every task and then every handler to completion before the next
	// batch starts (real Ansible's rolling-update semantics). serial<=0
	// (the default, "all hosts at once") is one batch containing every
	// host, identical to pre-serial behavior.
	for _, batch := range batchHosts(hosts, play.Serial) {
		e.runBatch(ctx, play, batch, pr)
	}
	return pr, nil
}

func batchHosts(hosts []*inventory.Host, serial int) [][]*inventory.Host {
	if serial <= 0 || serial >= len(hosts) {
		return [][]*inventory.Host{hosts}
	}
	var out [][]*inventory.Host
	for i := 0; i < len(hosts); i += serial {
		end := i + serial
		if end > len(hosts) {
			end = len(hosts)
		}
		out = append(out, hosts[i:end])
	}
	return out
}

func (e *Engine) runBatch(ctx context.Context, play Play, hosts []*inventory.Host, pr *PlayResult) {
	ec := &execCtx{
		engine:        e,
		play:          play,
		states:        make(map[string]*hostState, len(hosts)),
		delegateConns: map[string]remoteexec.Connection{},
	}
	var order []string
	for _, h := range hosts {
		vc := vars.New()
		vc.Set(vars.Inventory, e.Inventory.HostVars(h.Name))
		vc.Set(vars.ExtraVars, e.ExtraVars)
		vc.Set(vars.PlayVars, play.Vars)
		ec.states[h.Name] = &hostState{name: h.Name, vc: vc, notify: map[string]bool{}}
		order = append(order, h.Name)
	}

	e.connectAndGatherFacts(ctx, play, ec.states, pr)
	defer func() {
		ec.closeDelegates()
		for _, st := range ec.states {
			if st.conn != nil {
				st.conn.Close()
			}
		}
	}()

	active := activeHosts(ec.states, order)
	active = ec.runTaskList(ctx, play.Tasks, active, pr)
	ec.runHandlers(ctx, order, pr)
}

func (e *Engine) connectAndGatherFacts(ctx context.Context, play Play, states map[string]*hostState, pr *PlayResult) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, st := range states {
		wg.Add(1)
		go func(st *hostState) {
			defer wg.Done()
			conn, err := e.Connect(ctx, st.name, st.vc.Merged())
			if err != nil {
				mu.Lock()
				st.failed = true
				pr.record(Result{Host: st.name, Task: "(connect)", Failed: true, Msg: err.Error()})
				mu.Unlock()
				return
			}
			st.conn = conn
			if !play.GatherFacts {
				return
			}
			gathered, err := facts.Gather(ctx, conn)
			if err != nil {
				mu.Lock()
				st.failed = true
				pr.record(Result{Host: st.name, Task: "(gather_facts)", Failed: true, Msg: err.Error()})
				mu.Unlock()
				return
			}
			st.vc.Set(vars.Facts, vars.InjectFacts(gathered))
			mu.Lock()
			pr.record(Result{Host: st.name, Task: "(gather_facts)", Msg: "ok"})
			mu.Unlock()
		}(st)
	}
	wg.Wait()
}

// runTaskList runs tasks in order across active hosts, recursing into
// block/rescue/always, and returns the hosts still active afterward
// (those that neither failed nor were excluded).
func (ec *execCtx) runTaskList(ctx context.Context, tasks []Task, active []string, pr *PlayResult) []string {
	for _, task := range tasks {
		if len(active) == 0 {
			return active
		}
		if task.IsBlock() {
			active = ec.runBlock(ctx, task, active, pr)
		} else {
			active = ec.runSingleTask(ctx, task, active, pr)
		}
	}
	return active
}

func (ec *execCtx) runBlock(ctx context.Context, task Task, active []string, pr *PlayResult) []string {
	// A block's own when guards the whole block — block/rescue/always
	// alike — as if the block were entirely absent for a host it
	// excludes, not as a runtime failure (so rescue never runs for a
	// host the block's when already filtered out).
	if task.When != "" {
		var passed []string
		for _, h := range active {
			st := ec.states[h]
			ok, err := ec.engine.Template.EvalBool(task.When, st.vc.Merged())
			if err != nil {
				ec.report(pr, Result{Host: h, Task: task.Name, Failed: true, Msg: "when: " + err.Error()})
				continue
			}
			if ok {
				passed = append(passed, h)
			} else {
				ec.report(pr, Result{Host: h, Task: task.Name, Skipped: true})
			}
		}
		active = passed
	}
	if len(active) == 0 {
		return active
	}

	if task.RoleDefaults != nil || task.RoleVars != nil {
		restore := ec.pushRoleVars(active, task.RoleDefaults, task.RoleVars)
		defer restore()
	}

	originalActive := append([]string{}, active...)

	afterBlock := ec.runTaskList(ctx, task.Block, active, pr)
	newlyFailed := diff(active, afterBlock)

	stillFailed := newlyFailed
	if len(task.Rescue) > 0 && len(newlyFailed) > 0 {
		for _, h := range newlyFailed {
			ec.states[h].failed = false
		}
		afterRescue := ec.runTaskList(ctx, task.Rescue, newlyFailed, pr)
		rescueFailed := diff(newlyFailed, afterRescue)
		for _, h := range rescueFailed {
			ec.states[h].failed = true
		}
		stillFailed = rescueFailed
	}

	if len(task.Always) > 0 {
		saved := map[string]bool{}
		for _, h := range originalActive {
			saved[h] = contains(stillFailed, h)
			ec.states[h].failed = false
		}
		afterAlways := ec.runTaskList(ctx, task.Always, originalActive, pr)
		alwaysFailed := diff(originalActive, afterAlways)
		for _, h := range originalActive {
			ec.states[h].failed = saved[h] || contains(alwaysFailed, h)
		}
	} else {
		for _, h := range stillFailed {
			ec.states[h].failed = true
		}
	}

	var finalActive []string
	for _, h := range originalActive {
		if !ec.states[h].failed {
			finalActive = append(finalActive, h)
		}
	}
	return finalActive
}

// pushRoleVars sets RoleDefaults/RoleVars on every active host for the
// duration of a role's block, returning a function that restores each
// host's prior layer content. Single-level only: a role included from
// inside another role's own tasks leaves the inner role's vars/defaults
// in place for the rest of the outer role too, since restore only
// unwinds the layer it pushed — see Task.RoleDefaults/RoleVars.
func (ec *execCtx) pushRoleVars(active []string, defaults, roleVars map[string]any) func() {
	type saved struct{ defaults, vars map[string]any }
	prior := make(map[string]saved, len(active))
	for _, h := range active {
		st := ec.states[h]
		prior[h] = saved{defaults: st.vc.Layer(vars.RoleDefaults), vars: st.vc.Layer(vars.RoleVars)}
		if defaults != nil {
			st.vc.Set(vars.RoleDefaults, defaults)
		}
		if roleVars != nil {
			st.vc.Set(vars.RoleVars, roleVars)
		}
	}
	return func() {
		for _, h := range active {
			st := ec.states[h]
			st.vc.Set(vars.RoleDefaults, prior[h].defaults)
			st.vc.Set(vars.RoleVars, prior[h].vars)
		}
	}
}

func (ec *execCtx) runSingleTask(ctx context.Context, task Task, active []string, pr *PlayResult) []string {
	if !tagsMatch(task.Tags, ec.engine.RunTags, ec.engine.SkipTags) {
		for _, h := range active {
			ec.report(pr, Result{Host: h, Task: task.Name, Module: task.Module, Skipped: true, Msg: "tags"})
		}
		return active
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var stillActive []string
	for _, h := range active {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			st := ec.states[h]
			failed := ec.runTaskOnHost(ctx, task, st, pr)
			mu.Lock()
			if failed {
				st.failed = true
			} else {
				stillActive = append(stillActive, h)
			}
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	return stillActive
}

func tagsMatch(effective, run, skip []string) bool {
	if intersects(effective, skip) {
		return false
	}
	if len(run) == 0 {
		return true
	}
	if contains(effective, "always") {
		return true
	}
	return intersects(effective, run)
}

func intersects(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, x := range a {
		set[x] = true
	}
	for _, x := range b {
		if set[x] {
			return true
		}
	}
	return false
}

// runTaskOnHost runs task on one host (once per loop item, if looping)
// and reports whether the host should be excluded from the rest of the
// play (a failure not covered by ignore_errors).
func (ec *execCtx) runTaskOnHost(ctx context.Context, task Task, st *hostState, pr *PlayResult) bool {
	scope := st.vc.Child()
	scope.Set(vars.TaskVars, task.Vars)

	if task.When != "" {
		ok, err := ec.engine.Template.EvalBool(task.When, scope.Merged())
		if err != nil {
			ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Msg: "when: " + err.Error()})
			return !task.IgnoreErrors
		}
		if !ok {
			ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Skipped: true})
			return false
		}
	}

	items := []any{nil}
	looping := task.Loop != nil
	if looping {
		rendered, err := ec.engine.Template.RenderValue(task.Loop, scope.Merged())
		if err != nil {
			ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Msg: "loop: " + err.Error()})
			return !task.IgnoreErrors
		}
		if list, ok := rendered.([]any); ok {
			items = list
		} else {
			items = []any{rendered}
		}
	}

	anyChanged, anyFailed := false, false
	var lastResult modules.Result
	var lastExtra map[string]any

	for _, item := range items {
		iter := scope
		if looping {
			iter = scope.Child()
			iter.SetVar(vars.TaskVars, task.LoopVar, item)
		}
		mergedVars := iter.Merged()

		renderedArgs, err := ec.engine.Template.RenderValue(map[string]any(task.Args), mergedVars)
		args, _ := renderedArgs.(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		if err != nil {
			ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Msg: "args: " + err.Error()})
			anyFailed = true
			continue
		}
		if task.Module == "template" {
			args["_vars"] = mergedVars
		}

		result, handled, derr := ec.runDirective(ctx, task, st, args, pr)
		if derr != nil {
			ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Msg: derr.Error()})
			anyFailed = true
			continue
		}
		if !handled {
			conn, cerr := ec.connectionFor(ctx, task, mergedVars, st)
			if cerr != nil {
				ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Msg: cerr.Error()})
				anyFailed = true
				continue
			}
			result, err = ec.engine.Modules.Run(ctx, task.Module, conn, args)
			if err != nil {
				result = modules.Fail(err.Error())
			}
		}

		resultView := resultToMap(result)
		if task.ChangedWhen != "" {
			if ok, cerr := ec.engine.Template.EvalBool(task.ChangedWhen, withResult(mergedVars, resultView)); cerr == nil {
				result.Changed = ok
			}
		}
		if task.FailedWhen != "" {
			if ok, ferr := ec.engine.Template.EvalBool(task.FailedWhen, withResult(mergedVars, resultView)); ferr == nil {
				result.Failed = ok
			}
		}

		if result.Changed {
			anyChanged = true
		}
		if result.Failed {
			anyFailed = true
		}
		lastResult = result
		lastExtra = resultToMap(result)

		ec.report(pr, Result{
			Host: st.name, Task: task.Name, Module: task.Module,
			Changed: result.Changed, Failed: result.Failed, Msg: result.Msg, Extra: result.Extra,
		})

		// set_fact's/include_vars' variables are accessible by their
		// own bare name (unlike gathered system facts, which only
		// appear as ansible_facts.<name> / ansible_<name> — see
		// InjectFacts) — merged into the existing Facts layer, not
		// replacing it, so a set_fact after gather_facts doesn't erase
		// system facts.
		for k, v := range result.Facts {
			st.vc.SetVar(vars.Facts, k, v)
		}
	}

	if task.Register != "" {
		regValue := lastExtra
		if regValue == nil {
			regValue = map[string]any{}
		}
		regValue["changed"] = anyChanged
		regValue["failed"] = anyFailed
		if lastResult.Msg != "" {
			regValue["msg"] = lastResult.Msg
		}
		st.vc.SetVar(vars.Registered, task.Register, regValue)
	}

	if anyChanged {
		for _, name := range task.Notify {
			st.notify[name] = true
		}
	}

	return anyFailed && !task.IgnoreErrors
}

// connectionFor resolves the Connection an ordinary module task should
// run against: the delegate_to target's connection if set (rendered as
// a template, since delegate_to may reference a variable), otherwise
// this host's own connection — wrapped in Become if escalation applies.
func (ec *execCtx) connectionFor(ctx context.Context, task Task, mergedVars map[string]any, st *hostState) (remoteexec.Connection, error) {
	conn := st.conn
	if task.DelegateTo != "" {
		delegateName, err := ec.engine.Template.Render(task.DelegateTo, mergedVars)
		if err != nil {
			return nil, fmt.Errorf("delegate_to: %w", err)
		}
		delegateVars := mergedVars
		if hv := ec.engine.Inventory.HostVars(delegateName); len(hv) > 0 {
			delegateVars = hv
		}
		dconn, err := ec.delegateConn(ctx, delegateName, delegateVars)
		if err != nil {
			return nil, fmt.Errorf("delegate_to %s: %w", delegateName, err)
		}
		conn = dconn
	}
	if becomeCfg, ok := becomeConfigFor(ec.play, task, mergedVars); ok {
		conn = remoteexec.Become(conn, becomeCfg)
	}
	return conn, nil
}

// runDirective handles the small set of task "modules" that must run
// inside the engine itself rather than through modules.Registry,
// because they mutate engine-level state (the inventory, this host's
// own variable layers, or the play's handler-notify set) instead of
// just talking to a Connection. handled reports whether task.Module
// named one of these — when false, the caller falls through to the
// ordinary module dispatch.
func (ec *execCtx) runDirective(ctx context.Context, task Task, st *hostState, args map[string]any, pr *PlayResult) (result modules.Result, handled bool, err error) {
	switch task.Module {
	case "meta":
		r, e := ec.runMeta(ctx, args, st, pr)
		return r, true, e
	case "add_host":
		r, e := ec.runAddHost(args)
		return r, true, e
	case "group_by":
		r, e := ec.runGroupBy(args, st)
		return r, true, e
	case "include_vars":
		r, e := ec.runIncludeVars(args, st)
		return r, true, e
	default:
		return modules.Result{}, false, nil
	}
}

// runMeta implements Ansible's meta: task. Only flush_handlers (run
// every handler this host has notified so far, immediately, then clear
// the notify set so the play's end-of-play handler pass doesn't run
// them again) and clear_facts (drop the gathered-facts layer) are
// supported; other meta actions (end_play, end_host, reset_connection,
// ...) error rather than silently doing nothing.
func (ec *execCtx) runMeta(ctx context.Context, args map[string]any, st *hostState, pr *PlayResult) (modules.Result, error) {
	action, _ := args["_raw_params"].(string)
	switch action {
	case "flush_handlers":
		for _, handler := range ec.play.Handlers {
			if st.notify[handler.Name] {
				ec.runTaskOnHost(ctx, handler, st, pr)
			}
		}
		st.notify = map[string]bool{}
		return modules.Ok("flushed handlers"), nil
	case "clear_facts":
		st.vc.Set(vars.Facts, map[string]any{})
		return modules.Ok("cleared facts"), nil
	default:
		return modules.Result{}, fmt.Errorf("meta: %q not supported (only flush_handlers, clear_facts)", action)
	}
}

func (ec *execCtx) runAddHost(args map[string]any) (modules.Result, error) {
	name, _ := firstNonNil(args["name"], args["hostname"]).(string)
	if name == "" {
		return modules.Result{}, fmt.Errorf("add_host: missing required argument: name")
	}
	var groups []string
	switch g := firstNonNil(args["groups"], args["group"]).(type) {
	case string:
		groups = append(groups, splitCommaList(g)...)
	case []any:
		for _, item := range g {
			groups = append(groups, str(item))
		}
	}
	vals := map[string]any{}
	for k, v := range args {
		switch k {
		case "name", "hostname", "groups", "group":
			continue
		}
		vals[k] = v
	}
	ec.engine.Inventory.AddHost(name, vals, groups...)
	return modules.Changed("added host " + name), nil
}

func (ec *execCtx) runGroupBy(args map[string]any, st *hostState) (modules.Result, error) {
	key, _ := firstNonNil(args["key"], args["_raw_params"]).(string)
	if key == "" {
		return modules.Result{}, fmt.Errorf("group_by: missing required argument: key")
	}
	ec.engine.Inventory.AddToGroup(st.name, key)
	return modules.Changed("added " + st.name + " to group " + key), nil
}

// runIncludeVars loads a YAML file (path resolved relative to
// Engine.BaseDir) and merges it into this host's Facts layer — the
// same layer/bare-name-visibility convention set_fact uses, since both
// persist ordinary key/value vars across the rest of the play for this
// host.
func (ec *execCtx) runIncludeVars(args map[string]any, st *hostState) (modules.Result, error) {
	path, _ := firstNonNil(args["file"], args["_raw_params"]).(string)
	if path == "" {
		return modules.Result{}, fmt.Errorf("include_vars: missing required argument: file")
	}
	loaded, err := loadYAMLMap(filepath.Join(ec.engine.BaseDir, path), false)
	if err != nil {
		return modules.Result{}, fmt.Errorf("include_vars: %w", err)
	}
	for k, v := range loaded {
		st.vc.SetVar(vars.Facts, k, v)
	}
	return modules.Ok("included " + path), nil
}

func splitCommaList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (ec *execCtx) report(pr *PlayResult, r Result) {
	pr.record(r)
	if ec.engine.OnResult != nil {
		ec.engine.OnResult(r)
	}
}

func (ec *execCtx) runHandlers(ctx context.Context, order []string, pr *PlayResult) {
	for _, handler := range ec.play.Handlers {
		var toRun []string
		for _, h := range order {
			st := ec.states[h]
			if !st.failed && st.notify[handler.Name] {
				toRun = append(toRun, h)
			}
		}
		if len(toRun) == 0 {
			continue
		}
		ec.runSingleTask(ctx, handler, toRun, pr)
	}
}

func resultToMap(r modules.Result) map[string]any {
	out := map[string]any{
		"changed": r.Changed,
		"failed":  r.Failed,
		"msg":     r.Msg,
	}
	for k, v := range r.Extra {
		out[k] = v
	}
	return out
}

func withResult(vars map[string]any, result map[string]any) map[string]any {
	out := make(map[string]any, len(vars)+1)
	for k, v := range vars {
		out[k] = v
	}
	out["result"] = result
	return out
}

func activeHosts(states map[string]*hostState, order []string) []string {
	var out []string
	for _, h := range order {
		if !states[h].failed {
			out = append(out, h)
		}
	}
	return out
}

func diff(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, h := range b {
		inB[h] = true
	}
	var out []string
	for _, h := range a {
		if !inB[h] {
			out = append(out, h)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
