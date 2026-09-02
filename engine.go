package playbook

import (
	"context"
	"fmt"
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
	}
}

type hostState struct {
	name   string
	conn   remoteexec.Connection
	vc     *vars.Context
	failed bool
	notify map[string]bool
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
	pr := &PlayResult{Play: play.Name}

	hosts, err := e.Inventory.Match(play.Hosts)
	if err != nil {
		return pr, fmt.Errorf("play %q: %w", play.Name, err)
	}

	states := make(map[string]*hostState, len(hosts))
	var order []string
	for _, h := range hosts {
		vc := vars.New()
		vc.Set(vars.Inventory, e.Inventory.HostVars(h.Name))
		vc.Set(vars.ExtraVars, e.ExtraVars)
		vc.Set(vars.PlayVars, play.Vars)
		states[h.Name] = &hostState{name: h.Name, vc: vc, notify: map[string]bool{}}
		order = append(order, h.Name)
	}

	e.connectAndGatherFacts(ctx, play, states, pr)
	defer func() {
		for _, st := range states {
			if st.conn != nil {
				st.conn.Close()
			}
		}
	}()

	active := activeHosts(states, order)
	e.runTaskList(ctx, play, play.Tasks, states, active, pr)
	e.runHandlers(ctx, play, states, order, pr)

	return pr, nil
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
func (e *Engine) runTaskList(ctx context.Context, play Play, tasks []Task, states map[string]*hostState, active []string, pr *PlayResult) []string {
	for _, task := range tasks {
		if len(active) == 0 {
			return active
		}
		if task.IsBlock() {
			active = e.runBlock(ctx, play, task, states, active, pr)
		} else {
			active = e.runSingleTask(ctx, play, task, states, active, pr)
		}
	}
	return active
}

func (e *Engine) runBlock(ctx context.Context, play Play, task Task, states map[string]*hostState, active []string, pr *PlayResult) []string {
	originalActive := append([]string{}, active...)

	afterBlock := e.runTaskList(ctx, play, task.Block, states, active, pr)
	newlyFailed := diff(active, afterBlock)

	stillFailed := newlyFailed
	if len(task.Rescue) > 0 && len(newlyFailed) > 0 {
		for _, h := range newlyFailed {
			states[h].failed = false
		}
		afterRescue := e.runTaskList(ctx, play, task.Rescue, states, newlyFailed, pr)
		rescueFailed := diff(newlyFailed, afterRescue)
		for _, h := range rescueFailed {
			states[h].failed = true
		}
		stillFailed = rescueFailed
	}

	if len(task.Always) > 0 {
		saved := map[string]bool{}
		for _, h := range originalActive {
			saved[h] = contains(stillFailed, h)
			states[h].failed = false
		}
		afterAlways := e.runTaskList(ctx, play, task.Always, states, originalActive, pr)
		alwaysFailed := diff(originalActive, afterAlways)
		for _, h := range originalActive {
			states[h].failed = saved[h] || contains(alwaysFailed, h)
		}
	} else {
		for _, h := range stillFailed {
			states[h].failed = true
		}
	}

	var finalActive []string
	for _, h := range originalActive {
		if !states[h].failed {
			finalActive = append(finalActive, h)
		}
	}
	return finalActive
}

func (e *Engine) runSingleTask(ctx context.Context, play Play, task Task, states map[string]*hostState, active []string, pr *PlayResult) []string {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var stillActive []string
	for _, h := range active {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			st := states[h]
			failed := e.runTaskOnHost(ctx, play, task, st, pr)
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

// runTaskOnHost runs task on one host (once per loop item, if looping)
// and reports whether the host should be excluded from the rest of the
// play (a failure not covered by ignore_errors).
func (e *Engine) runTaskOnHost(ctx context.Context, play Play, task Task, st *hostState, pr *PlayResult) bool {
	scope := st.vc.Child()
	scope.Set(vars.TaskVars, task.Vars)

	if task.When != "" {
		ok, err := e.Template.EvalBool(task.When, scope.Merged())
		if err != nil {
			pr.record(Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Msg: "when: " + err.Error()})
			return !task.IgnoreErrors
		}
		if !ok {
			pr.record(Result{Host: st.name, Task: task.Name, Module: task.Module, Skipped: true})
			return false
		}
	}

	items := []any{nil}
	looping := task.Loop != nil
	if looping {
		rendered, err := e.Template.RenderValue(task.Loop, scope.Merged())
		if err != nil {
			pr.record(Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Msg: "loop: " + err.Error()})
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

		renderedArgs, err := e.Template.RenderValue(map[string]any(task.Args), mergedVars)
		args, _ := renderedArgs.(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		if err != nil {
			pr.record(Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Msg: "args: " + err.Error()})
			anyFailed = true
			continue
		}
		if task.Module == "template" {
			args["_vars"] = mergedVars
		}

		conn := st.conn
		if becomeCfg, ok := becomeConfigFor(play, task, mergedVars); ok {
			conn = remoteexec.Become(conn, becomeCfg)
		}

		result, err := e.Modules.Run(ctx, task.Module, conn, args)
		if err != nil {
			result = modules.Fail(err.Error())
		}

		resultView := resultToMap(result)
		if task.ChangedWhen != "" {
			if ok, cerr := e.Template.EvalBool(task.ChangedWhen, withResult(mergedVars, resultView)); cerr == nil {
				result.Changed = ok
			}
		}
		if task.FailedWhen != "" {
			if ok, ferr := e.Template.EvalBool(task.FailedWhen, withResult(mergedVars, resultView)); ferr == nil {
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

		e.report(pr, Result{
			Host: st.name, Task: task.Name, Module: task.Module,
			Changed: result.Changed, Failed: result.Failed, Msg: result.Msg, Extra: result.Extra,
		})

		// set_fact's variables are accessible by their own bare name
		// (unlike gathered system facts, which only appear as
		// ansible_facts.<name> / ansible_<name> — see InjectFacts) —
		// merged into the existing Facts layer, not replacing it, so a
		// set_fact after gather_facts doesn't erase system facts.
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

func (e *Engine) report(pr *PlayResult, r Result) {
	pr.record(r)
	if e.OnResult != nil {
		e.OnResult(r)
	}
}

func (e *Engine) runHandlers(ctx context.Context, play Play, states map[string]*hostState, order []string, pr *PlayResult) {
	for _, handler := range play.Handlers {
		var toRun []string
		for _, h := range order {
			st := states[h]
			if !st.failed && st.notify[handler.Name] {
				toRun = append(toRun, h)
			}
		}
		if len(toRun) == 0 {
			continue
		}
		e.runSingleTask(ctx, play, handler, states, toRun, pr)
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
