package playbook

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	// wanting ordering should serialize themselves. It is the one-hook
	// shorthand for a caller that only wants results; a caller wanting
	// play and recap events too installs a Callback instead.
	OnResult func(Result)

	// Callbacks are reporting plugins observing the run as it happens —
	// this port's equivalent of real Ansible's own callback plugins, and
	// a list for the same reason real Ansible loads one stdout callback
	// alongside any number of notification ones. See Callback for the
	// concurrency contract every hook is held to.
	Callbacks []Callback

	// Forks caps how many hosts run concurrently at once — connecting/
	// gathering facts, and each task/handler fan-out (runSingleTask,
	// runFree) all respect it — matching real Ansible's own forks
	// setting (default 5; New sets this field to 5 for the same
	// reason). 0 or negative means unlimited concurrency, which was
	// this port's only behavior before Forks existed; New's default
	// changes that for anyone constructing an Engine the normal way,
	// but a caller that explicitly wants the old unbounded behavior can
	// still set Forks to 0 after New returns.
	Forks int

	// Prompt implements vars_prompt's actual interactive prompting:
	// given the fully-formatted message (already combining the prompt
	// text and "[default]" the way real Ansible's own do_var_prompt
	// does) and whether the input should be hidden, it returns what the
	// user entered. New's default (defaultPrompt) is a plain
	// bufio-over-os.Stdin read with no terminal awareness at all — it
	// works the same whether stdin is a real terminal or a pipe (never
	// hides input, and never detects a non-interactive session the way
	// real Ansible does to skip prompting and warn instead), which
	// matches a raw library caller or a piped/test invocation but not
	// real Ansible's actual interactive behavior. go-ansible/cli's
	// ansible-playbook overrides this with a real, terminal-aware
	// implementation (golang.org/x/term) for actual interactive use.
	Prompt func(msg string, private bool) (string, error)
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
		Forks:     envInt("ANSIBLE_FORKS", 5), // real Ansible's own default (DEFAULT_FORKS)
		Prompt:    defaultPrompt,
	}
}

// defaultPrompt is Engine.Prompt's default — see that field's doc
// comment for what it deliberately doesn't do (no terminal awareness).
func defaultPrompt(msg string, private bool) (string, error) {
	fmt.Fprint(os.Stderr, msg)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
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

	// sem caps concurrent per-host work at Engine.Forks — nil (Forks <=
	// 0) means unlimited, matching every fan-out point's own prior
	// behavior before Forks existed.
	sem chan struct{}
}

// acquire blocks until a fork slot is free (a no-op when ec.sem is nil,
// i.e. Forks <= 0) and returns the matching release func — always
// call it, typically via defer, even when acquire itself is a no-op.
func (ec *execCtx) acquire() func() {
	if ec.sem == nil {
		return func() {}
	}
	ec.sem <- struct{}{}
	return func() { <-ec.sem }
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
	// The stats hook fires even when a play errors out, so a recap still
	// covers whatever did run — real Ansible prints one there too.
	defer func() {
		for _, cb := range e.Callbacks {
			cb.OnStats(rr)
		}
	}()
	for _, play := range pb {
		if err := e.applyVarsPrompt(&play); err != nil {
			return rr, fmt.Errorf("play %q: vars_prompt: %w", play.Name, err)
		}
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

// applyVarsPrompt resolves play.VarsPrompt into play.Vars, once per
// play (not per host) — matching real Ansible's own
// playbook_executor.py exactly: a name already present in
// Engine.ExtraVars is left alone entirely (no prompt at all — a real
// var supplied via -e/--extra-vars always outranks a play var anyway,
// but real Ansible also skips the interactive prompt itself in that
// case, and this matches it for the same UX reason, not because the
// final value would otherwise differ). confirm loops asking twice
// until they match, exactly like ansible-core's own do_var_prompt — no
// escape from the loop besides matching (nor does real Ansible have
// one). An empty result falls back to Default when one is set,
// matching real Ansible's own "if not result and default is not None"
// check.
func (e *Engine) applyVarsPrompt(play *Play) error {
	for _, vp := range play.VarsPrompt {
		if _, extra := e.ExtraVars[vp.Name]; extra {
			continue
		}
		promptText := vp.Prompt
		if promptText == "" {
			promptText = vp.Name
		}
		msg := promptText + ": "
		if vp.Default != "" {
			msg = fmt.Sprintf("%s [%s]: ", promptText, vp.Default)
		}

		var result string
		for {
			var err error
			result, err = e.Prompt(msg, vp.Private)
			if err != nil {
				return err
			}
			if !vp.Confirm {
				break
			}
			second, err := e.Prompt("confirm "+msg, vp.Private)
			if err != nil {
				return err
			}
			if result == second {
				break
			}
		}
		if result == "" && vp.Default != "" {
			result = vp.Default
		}

		if play.Vars == nil {
			play.Vars = map[string]any{}
		}
		play.Vars[vp.Name] = result
	}
	return nil
}

func (e *Engine) runPlay(ctx context.Context, play Play) (*PlayResult, error) {
	pr := newPlayResult(play.Name)

	for _, cb := range e.Callbacks {
		cb.OnPlayStart(play)
	}

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
	if e.Forks > 0 {
		ec.sem = make(chan struct{}, e.Forks)
	}
	var order []string
	for _, h := range hosts {
		vc := vars.New()
		vc.Set(vars.Inventory, e.Inventory.HostVars(h.Name))
		// inventory_hostname/playbook_dir are Ansible's "magic
		// variables" — set after HostVars so a literal host_var of the
		// same name can't shadow them, matching how hard these are to
		// override in real Ansible.
		vc.SetVar(vars.Inventory, "inventory_hostname", h.Name)
		vc.SetVar(vars.Inventory, "playbook_dir", e.BaseDir)
		vc.Set(vars.ExtraVars, e.ExtraVars)
		vc.Set(vars.PlayVars, play.Vars)
		ec.states[h.Name] = &hostState{name: h.Name, vc: vc, notify: map[string]bool{}}
		order = append(order, h.Name)
	}

	ec.connectAndGatherFacts(ctx, play, pr)
	defer func() {
		ec.closeDelegates()
		for _, st := range ec.states {
			if st.conn != nil {
				st.conn.Close()
			}
		}
	}()

	active := activeHosts(ec.states, order)
	if play.Strategy == "free" {
		runFree(ctx, ec, play, active, pr)
		return
	}
	active = ec.runTaskList(ctx, play.Tasks, active, pr)
	ec.runHandlers(ctx, order, pr)
}

// runFree implements the "free" strategy: every host runs the play's
// entire task list, then its own notified handlers, independently and
// concurrently — unlike linear (the default), a fast host is never
// held back by a slow one. This reuses runTaskList/runBlock/
// runSingleTask/runHandlers completely unchanged: each is invoked here
// with a single-host active/order slice per goroutine instead of the
// whole batch, so runSingleTask's own goroutine-per-host fan-out (which
// creates linear's per-task wg.Wait barrier when there are several
// hosts in the slice) degenerates to one goroutine waiting on itself —
// exactly the "no cross-host barrier" behavior free needs. Every host
// already had its own hostState/vars.Context, and PlayResult.record
// and execCtx.delegateConn are already mutex-guarded for concurrent
// per-host access, so nothing about running a whole task list per host
// concurrently (rather than one task at a time across hosts) is new.
func runFree(ctx context.Context, ec *execCtx, play Play, active []string, pr *PlayResult) {
	var wg sync.WaitGroup
	for _, h := range active {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			ec.runTaskList(ctx, play.Tasks, []string{h}, pr)
			ec.runHandlers(ctx, []string{h}, pr)
		}(h)
	}
	wg.Wait()
}

func (ec *execCtx) connectAndGatherFacts(ctx context.Context, play Play, pr *PlayResult) {
	e := ec.engine
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, st := range ec.states {
		wg.Add(1)
		go func(st *hostState) {
			defer wg.Done()
			release := ec.acquire()
			defer release()
			conn, err := e.Connect(ctx, st.name, st.vc.Merged())
			if err != nil {
				mu.Lock()
				st.failed = true
				ec.report(pr, Result{Host: st.name, Task: "(connect)", Failed: true, Msg: err.Error()})
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
				ec.report(pr, Result{Host: st.name, Task: "(gather_facts)", Failed: true, Msg: err.Error()})
				mu.Unlock()
				return
			}
			st.vc.Set(vars.Facts, vars.InjectFacts(gathered))
			mu.Lock()
			ec.report(pr, Result{Host: st.name, Task: "(gather_facts)", Msg: "ok"})
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
			active = ec.runSingleTask(ctx, task, active, pr, true)
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
// host's prior layer content. A role included from inside another
// role's own tasks (roles: nesting an include_role/import_role, or one
// include_role nesting another) merges its own defaults/vars on top of
// whatever the enclosing role already pushed, rather than replacing it
// outright — this matches real Ansible, which keeps every currently
// "active" role's defaults/vars in scope at once (confirmed against a
// real ansible-playbook run: a variable the inner role does not define
// in its own defaults/main.yml or vars/main.yml still resolves to the
// outer role's value while the inner role's tasks run). restore()
// unwinds back to exactly the merged content the enclosing role had in
// place before this push, so nesting composes correctly to any depth.
func (ec *execCtx) pushRoleVars(active []string, defaults, roleVars map[string]any) func() {
	type saved struct{ defaults, vars map[string]any }
	prior := make(map[string]saved, len(active))
	for _, h := range active {
		st := ec.states[h]
		priorDefaults := st.vc.Layer(vars.RoleDefaults)
		priorVars := st.vc.Layer(vars.RoleVars)
		prior[h] = saved{defaults: priorDefaults, vars: priorVars}
		if defaults != nil {
			st.vc.Set(vars.RoleDefaults, mergeOnto(priorDefaults, defaults))
		}
		if roleVars != nil {
			st.vc.Set(vars.RoleVars, mergeOnto(priorVars, roleVars))
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

// mergeOnto returns a new map holding base's entries overlaid with
// overlay's (overlay wins on a shared key), without mutating either
// argument — both may still be a live layer map or a role's parsed
// content, neither of which pushRoleVars owns.
func mergeOnto(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// runSingleTask runs one leaf (non-block) task across active hosts,
// one goroutine per host. filterTags applies Engine.RunTags/SkipTags —
// false for a handler run: real Ansible always runs a notified handler
// regardless of tags (handlers aren't part of tag filtering at all, not
// merely excluded from tag inheritance — propagateTags already leaves
// Play.Handlers untouched, but that alone doesn't stop an empty tag set
// from being excluded whenever RunTags is non-empty, so runHandlers
// must skip the check entirely).
func (ec *execCtx) runSingleTask(ctx context.Context, task Task, active []string, pr *PlayResult, filterTags bool) []string {
	if filterTags && !tagsMatch(task.Tags, ec.engine.RunTags, ec.engine.SkipTags) {
		for _, h := range active {
			ec.report(pr, Result{Host: h, Task: task.Name, Module: task.Module, Skipped: true, Msg: "tags"})
		}
		return active
	}

	// run_once: execute on only the first active host — matching real
	// Ansible, the rest get no report at all for this task (not even a
	// Skipped one), and simply pass through as still-active. Only
	// restricts anything when active has more than one host to begin
	// with; under strategy: free, active is always exactly one host
	// here (see runFree), so this is a no-op there — see Task.RunOnce.
	runOn := active
	var passthrough []string
	if task.RunOnce && len(active) > 1 {
		runOn = active[:1]
		passthrough = active[1:]
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var stillActive []string
	for _, h := range runOn {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			release := ec.acquire()
			defer release()
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

	if len(passthrough) > 0 {
		// Broadcast the run_once result to every other active host, so
		// a later task on ANY of them can still read it by bare name —
		// verified against a real ansible-playbook run (register: on a
		// run_once task is visible on every host, not just the one that
		// actually ran it). A failure on the executing host does not
		// propagate to the others: real Ansible only marks the host
		// that actually ran the task as failed.
		executor := ec.states[runOn[0]]
		if task.Register != "" {
			if regValue, ok := executor.vc.Get(task.Register); ok {
				for _, h := range passthrough {
					ec.states[h].vc.SetVar(vars.Registered, task.Register, regValue)
				}
			}
		}
		stillActive = append(stillActive, passthrough...)
	}

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

// runAsyncTask implements async:/poll: for a command/shell task —
// see Task.Async's doc comment for the scope (command/shell only) and
// modules.AsyncLaunch's for the one disclosed limitation (no active
// kill on an overrunning job; this loop still gives up and reports a
// timeout failure once task.Async seconds pass, it just can't reach
// out and stop the job on the target the way real Ansible's wrapper
// does).
func (ec *execCtx) runAsyncTask(ctx context.Context, task Task, conn remoteexec.Connection, args map[string]any) modules.Result {
	if task.Module != "command" && task.Module != "shell" {
		return modules.Fail(fmt.Sprintf(
			"async: is only supported for command/shell in this port — %s's work happens as a sequence of calls from the control node, not one remote invocation that could be backgrounded on the target the way async requires",
			task.Module))
	}
	cmdLine, skip, skipMsg, err := modules.ComposeCommandLine(ctx, conn, task.Module, args)
	if err != nil {
		return modules.Fail(err.Error())
	}
	if skip {
		return modules.Ok(skipMsg)
	}
	jid, err := modules.AsyncLaunch(ctx, conn, cmdLine)
	if err != nil {
		return modules.Fail(err.Error())
	}

	pollInterval := 15 // real Ansible's own DEFAULT_POLL_INTERVAL
	if task.Poll != nil {
		pollInterval = *task.Poll
	}
	if pollInterval <= 0 {
		// Fire-and-forget: a later async_status: jid=... task can check
		// on it (see modules.moduleAsyncStatus).
		return modules.Result{Changed: true}.
			WithExtra("ansible_job_id", jid).
			WithExtra("started", true).
			WithExtra("finished", false)
	}

	deadline := time.Now().Add(time.Duration(task.Async) * time.Second)
	for {
		select {
		case <-ctx.Done():
			return modules.Fail("async: cancelled while waiting for job " + jid)
		case <-time.After(time.Duration(pollInterval) * time.Second):
		}
		found, done, rc, stdout, stderr, err := modules.AsyncCheck(ctx, conn, jid)
		if err != nil {
			return modules.Fail(err.Error())
		}
		if !found {
			return modules.Fail("async: job " + jid + " disappeared while waiting for it")
		}
		if done {
			r := modules.Result{Changed: true, Failed: rc != 0}
			if r.Failed {
				r.Msg = fmt.Sprintf("non-zero return code: %d", rc)
			}
			return r.WithExtra("ansible_job_id", jid).
				WithExtra("started", true).
				WithExtra("finished", true).
				WithExtra("rc", rc).
				WithExtra("stdout", stdout).
				WithExtra("stderr", stderr)
		}
		if time.Now().After(deadline) {
			return modules.Fail("async: timeout exceeded, job "+jid+" is still running").
				WithExtra("ansible_job_id", jid).
				WithExtra("started", true).
				WithExtra("finished", false)
		}
	}
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

	// loopResults collects one entry per iteration for a looped task's
	// registered value — real Ansible's own `results` list. Nil for a
	// task that isn't looping, which registers its module fields flat.
	var loopResults []any

	for index, item := range items {
		iter := scope
		if looping {
			iter = scope.Child()
			iter.SetVar(vars.TaskVars, task.LoopVar, item)
			if task.IndexVar != "" {
				// loop_control.index_var, 0-based as in real Ansible.
				iter.SetVar(vars.TaskVars, task.IndexVar, index)
			}
		}

		// Real Ansible's own retry-loop variable (task_executor.py):
		// "retries" there counts the default actual run PLUS configured
		// retries. Task.Retries nil (unset) means no retry loop at all
		// UNLESS Until is non-empty, in which case real Ansible defaults
		// it to 3 — deliberately not folded into parseTask, since that
		// needs to distinguish "Retries explicitly 0" (still no-op:
		// totalAttempts stays 1) from "Retries unset".
		totalAttempts := 1
		switch {
		case task.Retries != nil:
			if *task.Retries > 0 {
				totalAttempts += *task.Retries
			}
		case task.Until != "":
			totalAttempts += 3
		}
		delay := task.Delay
		if delay < 0 {
			delay = 1
		}

		var result modules.Result
		var mergedVars map[string]any
		var attemptView map[string]any
		aborted := false

		for attempt := 1; attempt <= totalAttempts; attempt++ {
			mergedVars = iter.Merged()

			renderedArgs, err := ec.engine.Template.RenderValue(map[string]any(task.Args), mergedVars)
			args, _ := renderedArgs.(map[string]any)
			if args == nil {
				args = map[string]any{}
			}
			if err != nil {
				ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Msg: "args: " + err.Error()})
				anyFailed = true
				aborted = true
				break
			}
			if task.Module == "template" {
				args["_vars"] = mergedVars
			}

			var handled bool
			var derr error
			result, handled, derr = ec.runDirective(ctx, task, st, args, pr)
			if derr != nil {
				ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Msg: derr.Error()})
				anyFailed = true
				aborted = true
				break
			}
			if !handled {
				conn, cerr := ec.connectionFor(ctx, task, mergedVars, st)
				if cerr != nil {
					ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Msg: cerr.Error()})
					anyFailed = true
					aborted = true
					break
				}
				if task.Async > 0 {
					result = ec.runAsyncTask(ctx, task, conn, args)
				} else {
					var rerr error
					result, rerr = ec.engine.Modules.Run(ctx, task.Module, conn, args)
					if rerr != nil {
						result = modules.Fail(rerr.Error())
					}
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

			// Fresh snapshot, taken after changed_when/failed_when so
			// it reflects their effect — this is what gets registered
			// mid-retry (so until: can reference it) and what the
			// final report/register below uses.
			attemptView = resultToMap(result)
			if totalAttempts == 1 {
				break // the overwhelmingly common case: no retry loop at all
			}
			attemptView["attempts"] = attempt

			// Registered on iter (this item's own scope, a snapshot
			// copy per vars.Context.Child's own doc comment — not a
			// live view of st.vc), not st.vc: mergedVars is re-merged
			// from iter right below, and st.vc's own copy won't be
			// visible there until the task-level register after this
			// whole retry loop sets it for real, for subsequent tasks
			// to see.
			if task.Register != "" {
				iter.SetVar(vars.Registered, task.Register, attemptView)
				mergedVars = iter.Merged()
			}

			cond := task.Until
			passed := !result.Failed
			if cond != "" {
				if ok, uerr := ec.engine.Template.EvalBool(cond, withResult(mergedVars, attemptView)); uerr == nil {
					passed = ok
				} else {
					passed = false
				}
			}
			if passed {
				break
			}
			if attempt < totalAttempts {
				attemptView["retries"] = totalAttempts
				attemptView["attempts"] = attempt + 1
				if task.Register != "" {
					iter.SetVar(vars.Registered, task.Register, attemptView)
				}
				select {
				case <-ctx.Done():
					aborted = true
				case <-time.After(time.Duration(delay * float64(time.Second))):
				}
				if aborted {
					break
				}
				continue
			}
			// Every attempt is exhausted without until (or, absent
			// until, plain success) ever passing — real Ansible fails
			// the task here regardless of the LAST attempt's own
			// Failed value, since the entire point of until is that
			// exhausting retries without it passing is itself a
			// failure. attempts is deliberately set to totalAttempts-1,
			// not totalAttempts: a genuine, verified quirk in
			// ansible-core's own retry loop (task_executor.py's
			// for-else branch), not a typo here — confirmed both
			// empirically (retries: 3 → 4 real module executions, but
			// the registered result's own .attempts field reads 3) and
			// in ansible-core's source itself. Reproduced rather than
			// "fixed", since a real playbook may already read
			// .attempts expecting this exact value.
			attemptView["attempts"] = totalAttempts - 1
			result.Failed = true
		}

		if aborted {
			continue
		}

		if result.Changed {
			anyChanged = true
		}
		if result.Failed {
			anyFailed = true
		}
		lastResult = result
		lastExtra = attemptView

		if looping {
			// One entry per iteration, shaped as real Ansible shapes it:
			// the module's own fields plus the item and the name of the
			// loop variable it was bound to.
			entry := map[string]any{}
			for k, v := range attemptView {
				entry[k] = v
			}
			entry["item"] = item
			entry["ansible_loop_var"] = task.LoopVar
			loopResults = append(loopResults, entry)
		}

		ec.report(pr, Result{
			Host: st.name, Task: task.Name, Module: task.Module,
			Changed: result.Changed, Failed: result.Failed, Msg: result.Msg, Extra: result.Extra,
		})

		// set_fact's/include_vars' variables are accessible by their
		// own bare name; setup/gather_facts run as an explicit task
		// (not just the play's automatic gather_facts: step, which
		// already goes through InjectFacts in connectAndGatherFacts)
		// must match that same ansible_facts.<name>/ansible_<name>
		// convention — real Ansible exposes system facts identically
		// either way, and a bare-name-only setup task would silently
		// break any template written the normal way. Both cases merge
		// per-key into the existing Facts layer rather than replacing
		// it, so neither erases facts the other already set.
		factVars := result.Facts
		if task.Module == "setup" || task.Module == "gather_facts" {
			factVars = vars.InjectFacts(result.Facts)
		}
		for k, v := range factVars {
			st.vc.SetVar(vars.Facts, k, v)
		}
	}

	if task.Register != "" && looping {
		// A looped task registers ONLY the aggregate plus the
		// per-iteration list — measured against real ansible-core 2.21.4,
		// whose registered value for one has exactly the keys
		// changed/failed/msg/results and none of the module's own fields.
		// `{{ r.results | map(attribute='stdout') }}` is everyday usage
		// and `results` did not exist here at all.
		if loopResults == nil {
			loopResults = []any{}
		}
		st.vc.SetVar(vars.Registered, task.Register, map[string]any{
			"changed": anyChanged,
			"failed":  anyFailed,
			"msg":     "All items completed",
			"results": loopResults,
		})
	} else if task.Register != "" {
		regValue := lastExtra
		if regValue == nil {
			regValue = map[string]any{}
		}
		regValue["changed"] = anyChanged
		regValue["failed"] = anyFailed
		if lastResult.Msg != "" {
			regValue["msg"] = lastResult.Msg
		}
		// setup/gather_facts and set_fact/include_vars put their
		// output in Result.Facts, not Result.Extra — resultToMap
		// (source of lastExtra) only ever flattens Extra, so without
		// this a registered setup/set_fact result would be missing
		// its facts entirely. Real Ansible's own registered-result
		// shape nests them under ansible_facts either way (confirmed
		// against a real ansible-playbook run for both modules), so
		// match that key rather than exposing them bare.
		if len(lastResult.Facts) > 0 {
			regValue["ansible_facts"] = lastResult.Facts
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
	case "assert":
		r, e := ec.runAssert(args, st)
		return r, true, e
	default:
		return modules.Result{}, false, nil
	}
}

// runAssert evaluates assert's `that` conditions here rather than in the
// module, because they are Jinja2 expressions over the host's own
// variables and only the engine holds those — which is exactly why real
// Ansible's assert is an action plugin rather than a module.
//
// The modules package already documented this contract ("the playbook
// engine evaluates each Jinja2 expression in `that` before invoking the
// module") and nothing ever honoured it, so every real
// `assert: that: ["x == 5"]` failed here with "did not evaluate to a
// boolean" while running fine on real ansible-core. A condition is
// evaluated the same way `when:` is, since real Ansible evaluates both
// through the same templar.
func (ec *execCtx) runAssert(args map[string]any, st *hostState) (modules.Result, error) {
	raw, ok := args["that"]
	if !ok {
		return modules.Fail("assert: missing required argument: that"), nil
	}
	conditions, ok := raw.([]any)
	if !ok {
		conditions = []any{raw}
	}

	vars := st.vc.Merged()
	for _, cond := range conditions {
		truthy, err := ec.evalCondition(cond, vars)
		if err != nil {
			return modules.Fail(fmt.Sprintf("assert: %v", err)), nil
		}
		if truthy {
			continue
		}
		msg := stringArg(args, "fail_msg", stringArg(args, "msg", fmt.Sprintf("Assertion failed: %v", cond)))
		return modules.Fail(msg), nil
	}
	return modules.Ok(stringArg(args, "success_msg", "All assertions passed")), nil
}

// evalCondition takes one assert condition. A bare bool is already
// decided — a caller can write `that: true`, and a whole-expression
// string like "{{ x }}" may already have been rendered to one by the
// time args reach here. Anything else is treated as expression source,
// exactly as `when:` treats it.
func (ec *execCtx) evalCondition(cond any, vars map[string]any) (bool, error) {
	switch c := cond.(type) {
	case bool:
		return c, nil
	case string:
		return ec.engine.Template.EvalBool(c, vars)
	default:
		return false, fmt.Errorf("condition %v is neither a boolean nor an expression (got %T)", cond, cond)
	}
}

func stringArg(args map[string]any, key, fallback string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return fallback
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

// report is the single chokepoint every task result goes through: it
// records the result on the play and raises it to the caller's own
// reporting hooks. Nothing should call pr.record directly — a result
// that skips this is a result no callback ever sees.
func (ec *execCtx) report(pr *PlayResult, r Result) {
	pr.record(r)
	if ec.engine.OnResult != nil {
		ec.engine.OnResult(r)
	}
	for _, cb := range ec.engine.Callbacks {
		cb.OnTaskResult(r)
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
		ec.runSingleTask(ctx, handler, toRun, pr, false)
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
