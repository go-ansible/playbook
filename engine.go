package playbook

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
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

	// Warn reports a non-fatal diagnostic about the run's inputs — a
	// host pattern that matched nothing, for one. New installs a Warner
	// writing real Ansible's "[WARNING]: " lines to stderr; a caller
	// that already has one (go-ansible/cli builds the Warner it uses for
	// its own inventory warnings) should assign THAT one here, so the
	// whole process shares a single deduplication set the way real's one
	// Display singleton does.
	//
	// A nil Warn is silent, not a panic.
	Warn Warner

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

	// CheckMode runs the playbook without changing anything —
	// ansible-playbook's --check. A module that honours a dry run is told
	// so and reports what it WOULD do; one that does not is skipped
	// rather than run, which is what real Ansible does and what keeps a
	// dry run safe while modules gain support one at a time.
	CheckMode bool

	// Limit restricts every play to hosts matching this pattern as well
	// as their own — ansible-playbook's --limit. It takes the same
	// pattern language as a play's hosts:, so "web:!web3" and "@file"
	// style subsets work the same way, and it INTERSECTS rather than
	// replaces: a play already narrower than the limit stays narrow.
	//
	// A limit that leaves the whole inventory with nothing to target is
	// an error in real Ansible rather than a silent no-op — but that
	// check belongs to the CLI, which makes it ONCE against "all"
	// before any play runs (ansible/cli/__init__.py get_host_list).
	// That is why a play whose own hosts: matches nothing is not an
	// error while a --limit matching nothing is.
	Limit string

	// StartAtTask skips every task until one whose NAME matches, then
	// runs from there on — ansible-playbook's --start-at-task. The
	// match is exact OR a shell glob (real Ansible uses fnmatch) and is
	// case-sensitive, so "two" and "tw*" and "*wo" all find a task
	// named two while "tw" and "TWO" do not. It is tried against a
	// task's own name and against the "role : name" form a role task
	// displays, both of which real Ansible tries.
	//
	// Once a task has matched, the skipping stops for the REST OF THE
	// RUN — across later plays, and across later playbooks run through
	// the same Engine. That is what makes --start-at-task resume a
	// multi-play run at a point rather than re-skipping inside every
	// play.
	//
	// A skipped task produces no result at all: no banner, no
	// "skipping:" line, nothing in the recap, exactly as a task
	// excluded by tags does.
	StartAtTask string

	// startAtDone records that StartAtTask has been reached. It is
	// guarded because strategy: free runs a whole task list per host
	// concurrently (see runFree), so several hosts can reach the
	// matching task at once.
	startAtMu   sync.Mutex
	startAtDone bool

	// ForceHandlers runs a host's notified handlers even when that host
	// has already failed — ansible-playbook's --force-handlers.
	// Normally a failed host runs nothing further, handlers included,
	// which can leave a service stopped because the handler that would
	// have restarted it never ran.
	ForceHandlers bool

	// AllowBrokenConditionals permits a when:/until:/changed_when:
	// whose result is not a boolean, applying ordinary truthiness
	// instead of refusing — real Ansible's ALLOW_BROKEN_CONDITIONALS,
	// which defaults to off there too and which upstream plans to
	// remove in 2.23.
	//
	// A playbook that needs it is a playbook real ansible-core also
	// refuses, so this exists to unblock a migration rather than to be
	// left on.
	AllowBrokenConditionals bool

	// DiffMode makes modules report what they changed —
	// ansible-playbook's --diff. A module that supports it returns the
	// before and after contents, which the callbacks render as a unified
	// diff; one that does not simply reports nothing extra, exactly as
	// in real Ansible, so turning it on is never destructive and never
	// fails a run.
	//
	// It composes with CheckMode: --diff --check shows the diff of a
	// change that is deliberately not made.
	DiffMode bool

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

	// VaultPassword decrypts a vault-encrypted file loaded at RUN time —
	// today that means include_vars. Files pulled in while parsing (the
	// playbook, vars_files, a role's own files) take their password from
	// ParseFileWithVault instead, since parsing happens before an Engine
	// exists.
	VaultPassword string
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
		Warn:      defaultWarner(),
		// Real Ansible reads the same variable, and defaults to off.
		AllowBrokenConditionals: envBool("ANSIBLE_ALLOW_BROKEN_CONDITIONALS", false),
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
	name string
	conn remoteexec.Connection
	vc   *vars.Context

	// connErr is why this host has no connection, kept rather than
	// reported at connect time. Real Ansible connects per TASK, so an
	// unreachable host reports UNREACHABLE under the first task that
	// actually needs a connection — "Gathering Facts", or the first
	// module task — not under a synthetic step of its own. Keeping the
	// error here is what lets this port report it in the same place,
	// and lets a task whose ignore_unreachable said to carry on be
	// followed by another that tries again.
	connErr error

	// currentTask is what this host is running right now, and
	// lastFailedTask/lastFailedResult are the most recent failure —
	// what a rescue: block reads back as ansible_failed_task and
	// ansible_failed_result. Recorded per host, and only ever touched
	// by that host's own goroutine.
	currentTask Task

	// pending buffers this host's results while a task runs several
	// hosts CONCURRENTLY, so they can be emitted in host order
	// afterwards. Touched only by this host's own goroutine.
	pending          []Result
	buffering        bool
	lastFailedTask   Task
	lastFailedResult map[string]any

	failed bool
	// notify holds the INDICES of the play's handlers this host has
	// notified, not their names: a notification can reach several
	// handlers at once through listen:, and two handlers may share a
	// name.
	notify map[int]bool
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

	// playAborted records that any_errors_fatal or max_fail_percentage
	// tripped. It stops the whole PLAY, not just this batch: a rolling
	// update that gives up must not roll on to the next batch.
	playAborted bool
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

	hosts, unmatched, err := e.Inventory.MatchReport(play.Hosts)
	if err != nil {
		return pr, fmt.Errorf("play %q: %w", play.Name, err)
	}
	// A pattern term that matched nothing is worth saying out loud:
	// without it, a play whose hosts: is a typo is indistinguishable
	// from one that deliberately selects nothing, and both just print
	// "skipping: no hosts matched". Real says so once per term, on
	// stderr, which is why this does not go through the callback.
	for _, term := range unmatched {
		e.warn("Could not match supplied host pattern, ignoring: " + term)
	}
	if hosts, err = e.applyLimit(hosts); err != nil {
		return pr, fmt.Errorf("play %q: %w", play.Name, err)
	}

	// serial: splits the matched hosts into batches, each batch running
	// every task and then every handler to completion before the next
	// batch starts (real Ansible's rolling-update semantics). serial<=0
	// (the default, "all hosts at once") is one batch containing every
	// host, identical to pre-serial behavior.
	//
	// The play-start hook fires once per BATCH, not once per play: real
	// Ansible re-banners the play for each batch, which is what makes
	// the boundaries of a rolling update visible while it runs. A play
	// without serial: has one batch, so this is unchanged for it — and
	// a play that matched no hosts still has one (empty) batch, so it
	// is still bannered.
	for _, batch := range batchHosts(hosts, play.Serial) {
		names := make([]string, 0, len(batch))
		for _, h := range batch {
			names = append(names, h.Name)
		}
		for _, cb := range e.Callbacks {
			cb.OnPlayStart(play, names)
		}
		if e.runBatch(ctx, play, batch, pr) {
			// any_errors_fatal or max_fail_percentage gave up: the
			// remaining batches are not attempted.
			break
		}
	}
	return pr, nil
}

// batchHosts splits hosts into the rolling-update batches serial asks
// for — a port of real Ansible's _get_serialized_batches
// (ansible/executor/playbook_executor.py).
//
// Each entry is a count or a percentage; the list is consumed in order
// and its LAST entry repeats until every host has run, so
// `serial: [1, 2]` over five hosts gives batches of 1, 2 and 2. An
// entry that resolves to zero or less means "all the rest, in one
// batch", which is what an absent serial: does.
//
// A percentage is always of the play's TOTAL host count, not of the
// hosts still waiting.
func batchHosts(hosts []*inventory.Host, serial []string) [][]*inventory.Host {
	// A play whose pattern matched nothing still gets one (empty)
	// batch, so it is still bannered and still reports that it matched
	// no hosts. Real Ansible produces no batches here and handles that
	// case outside the loop; one empty batch is the same thing in the
	// shape this engine uses.
	if len(hosts) == 0 {
		return [][]*inventory.Host{nil}
	}
	if len(serial) == 0 {
		serial = []string{"-1"}
	}

	total := len(hosts)
	remaining := hosts
	var out [][]*inventory.Host
	for cur := 0; len(remaining) > 0; {
		n := pctToInt(serial[cur], total)
		if n <= 0 {
			// Not an error: real Ansible treats it as "everything that
			// is left", which is how serial: 0 means no batching.
			out = append(out, remaining)
			break
		}
		if n > len(remaining) {
			n = len(remaining)
		}
		out = append(out, remaining[:n])
		remaining = remaining[n:]
		if cur < len(serial)-1 {
			cur++
		}
	}
	return out
}

// pctToInt is real Ansible's own helper (ansible/utils/helpers.py): a
// "N%" entry becomes a fraction of total, anything else is read as a
// plain integer. A percentage that works out to zero becomes one, so a
// tiny percentage of a small fleet still makes progress rather than
// looping forever.
//
// The percentage is computed in FLOATING POINT and truncated, exactly
// as Python's int((pct / 100.0) * total) does, because the two disagree:
// 29% of 100 hosts is 28 there (0.29*100 is 28.999999999999996), not
// 29. Measured against a real 100-host run, which batches 28/28/28/16.
func pctToInt(value string, total int) int {
	if pct, ok := strings.CutSuffix(value, "%"); ok {
		p, err := strconv.Atoi(strings.TrimSpace(pct))
		if err != nil {
			return 0
		}
		if n := int(float64(p) / 100.0 * float64(total)); n != 0 {
			return n
		}
		return 1
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return n
}

// Returns whether a safety limit stopped the play, in which case no
// later batch runs.
func (e *Engine) runBatch(ctx context.Context, play Play, hosts []*inventory.Host, pr *PlayResult) (aborted bool) {
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
		order = append(order, h.Name)
	}
	// The inventory-wide magic variables are the same for every host,
	// so they are built ONCE: groups is every group's membership and
	// hostvars every host's variables, which is how a playbook
	// templates one host's config from another's facts.
	groupsVar := e.groupsVar()
	hostvarsVar := e.hostvarsVar(groupsVar)
	playHosts := append([]string{}, order...)
	// Absolute, as real reports it: a relative playbook path still
	// yields a full directory.
	playbookDir := e.BaseDir
	if abs, err := filepath.Abs(playbookDir); err == nil {
		playbookDir = abs
	}

	for _, h := range hosts {
		vc := vars.New()
		vc.Set(vars.Inventory, e.Inventory.HostVars(h.Name))
		// Ansible's "magic variables" — set after HostVars so a
		// literal host_var of the same name can't shadow them,
		// matching how hard these are to override in real Ansible.
		for k, v := range magicHostVars(e.Inventory, h.Name) {
			vc.SetVar(vars.Inventory, k, v)
		}
		vc.SetVar(vars.Inventory, "groups", groupsVar)
		vc.SetVar(vars.Inventory, "hostvars", hostvarsVar)
		vc.SetVar(vars.Inventory, "ansible_play_hosts", playHosts)
		vc.SetVar(vars.Inventory, "ansible_play_batch", playHosts)
		vc.SetVar(vars.Inventory, "inventory_dir", e.Inventory.SourceDir)
		vc.SetVar(vars.Inventory, "playbook_dir", playbookDir)
		vc.Set(vars.ExtraVars, e.ExtraVars)
		vc.Set(vars.PlayVars, play.Vars)
		ec.states[h.Name] = &hostState{name: h.Name, vc: vc, notify: map[int]bool{}}
	}
	order = orderHosts(order, play.Order)

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
		return ec.playAborted
	}
	active = ec.runTaskList(ctx, play.Tasks, active, pr)
	ec.runHandlers(ctx, order, pr)
	return ec.playAborted
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

// gatheringFactsTask is what real Ansible banners its implicit
// fact-gathering step as — "TASK [Gathering Facts]". This port called
// it "(gather_facts)", which diverged on the second line of every
// transcript of a play that gathers facts, i.e. the default.
const gatheringFactsTask = "Gathering Facts"

// connectionlessModules are the tasks real Ansible runs without ever
// touching the connection. The list is its own: the action plugins in
// ansible/plugins/action/ that call neither _execute_module nor
// _low_level_execute_command in 2.21.4, plus meta, which the strategy
// handles rather than any action plugin. Confirmed by running each one
// against an unreachable host — they all reported ok, while `command`
// reported UNREACHABLE.
//
// It only matters for a host that has no connection: real re-attempts
// the connection per task, so a debug after an ignored-unreachable
// ping still runs, while a command after it reports UNREACHABLE again.
var connectionlessModules = map[string]bool{
	"add_host":               true,
	"assert":                 true,
	"debug":                  true,
	"fail":                   true,
	"group_by":               true,
	"include_vars":           true,
	"meta":                   true,
	"pause":                  true,
	"set_fact":               true,
	"set_stats":              true,
	"validate_argument_spec": true,
}

// ignoreUnreachable reports whether an unreachable host should stay in
// the play for this task: the task's own setting if it has one, else
// the play's. Real Ansible decides per task, which is what makes an
// ignored-unreachable ping followed by an ordinary command report
// UNREACHABLE twice and drop the host only on the second.
func (ec *execCtx) ignoreUnreachable(task Task) bool {
	if task.IgnoreUnreachable != nil {
		return *task.IgnoreUnreachable
	}
	return ec.play.IgnoreUnreachable
}

// reportUnreachable records an unreachable result for one task and
// reports whether the host should now be dropped from the play. The
// caller must hold whatever lock guards ec.report at its call site.
func (ec *execCtx) reportUnreachable(pr *PlayResult, st *hostState, task Task, err error) bool {
	ignored := ec.ignoreUnreachable(task)
	ec.report(pr, Result{
		Host: st.name, Task: task.Name, Module: task.Module,
		Failed: true, Unreachable: true, Ignored: ignored, Msg: err.Error(),
	})
	if ignored {
		return false
	}
	st.failed = true
	return true
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
			conn, err := e.Connect(ctx, st.name, withPlayConnection(play, st.vc.Merged()))
			if err != nil {
				// Not reported here: a play that gathers no facts
				// needs no connection yet, and real Ansible says
				// nothing until a task actually wants one.
				mu.Lock()
				st.connErr = err
				if play.GatherFacts {
					ec.reportUnreachable(pr, st, Task{Name: gatheringFactsTask}, err)
				}
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
				ec.report(pr, Result{Host: st.name, Task: gatheringFactsTask, Failed: true, Msg: err.Error()})
				mu.Unlock()
				return
			}
			st.vc.Set(vars.Facts, vars.InjectFacts(gathered))
			mu.Lock()
			ec.report(pr, Result{Host: st.name, Task: gatheringFactsTask, Msg: "ok"})
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
		// any_errors_fatal / max_fail_percentage are checked after each
		// task, which is where real Ansible checks them.
		active = ec.applySafetyLimits(task, active, pr)
	}
	return active
}

// applySafetyLimits stops the play when any_errors_fatal or
// max_fail_percentage says a failure has gone far enough — a port of
// the two checks real Ansible's linear strategy makes after every task
// (plugins/strategy/linear.py).
//
// Both were PARSED and then ignored here, which is the worst shape for
// a safety brake: a play saying "if any host fails, stop" carried on
// deploying to the rest of the fleet, and said nothing.
//
// Returns the hosts that may continue — empty once a limit has tripped,
// with every remaining host marked failed, as real Ansible marks them.
func (ec *execCtx) applySafetyLimits(task Task, active []string, pr *PlayResult) []string {
	if len(active) == 0 {
		return active
	}

	// Cumulative across the batch, not just this task: real Ansible
	// compares its whole failed-host set against the batch size.
	failed := 0
	for _, st := range ec.states {
		if st.failed {
			failed++
		}
	}
	if failed == 0 {
		return active
	}

	// A task's own any_errors_fatal overrides the play's. run_once
	// implies it: real Ansible treats a failing run_once task as
	// fatal for everyone, since the one host stood in for all of them.
	fatal := ec.play.AnyErrorsFatal || task.AnyErrorsFatal || isRunOnce(task)

	overLimit := false
	if pct := ec.play.MaxFailPercentage; pct != nil && len(ec.states) > 0 {
		// Real Ansible's own arithmetic: failed/batch_size compared
		// against pct/100, strictly greater. So 20% failed with
		// max_fail_percentage: 20 CONTINUES, and 19 does not.
		overLimit = float64(failed)/float64(len(ec.states)) > *pct/100.0
	}
	if !fatal && !overLimit {
		return active
	}

	for _, h := range active {
		st := ec.states[h]
		if st.failed {
			continue
		}
		st.failed = true
	}
	ec.playAborted = true
	return nil
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
			ok, err := ec.evalWhen(task.When, st.vc.Merged())
			if err != nil {
				ec.report(pr, Result{Host: h, Task: task.Name, Failed: true, Msg: conditionalFailure("when", err)})
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
		if task.RoleVarsScoped {
			defer restore()
		}
	}

	// A dynamic include announces itself, once per host, before its
	// tasks run: a TASK banner and an "included: <path> for <host>"
	// line that counts toward ok in the recap. A static import
	// announces nothing, which is why this keys on IncludedFile rather
	// than on being a block.
	if task.IncludedFile != "" {
		for _, h := range active {
			ec.report(pr, Result{
				Host: h, Task: task.Name,
				Included: task.IncludedFile,
			})
		}
	}

	// A block's own vars: reach every task inside it and go out of
	// scope with it. The BlockVars layer existed, named and ordered
	// between the play's and the task's, and NOTHING ever wrote to it —
	// so `block: {vars: {v: x}}` was accepted and dropped, and so was
	// the vars: on an include_tasks/import_tasks, which parse into a
	// synthetic block. A playbook parameterising an included file got
	// the variable undefined.
	if len(task.Vars) > 0 {
		defer ec.pushBlockVars(active, task.Vars)()
	}

	originalActive := append([]string{}, active...)

	afterBlock := ec.runTaskList(ctx, task.Block, active, pr)
	newlyFailed := diff(active, afterBlock)

	stillFailed := newlyFailed
	if len(task.Rescue) > 0 && len(newlyFailed) > 0 {
		for _, h := range newlyFailed {
			st := ec.states[h]
			st.failed = false
			// Real sets these as nonpersistent FACTS the moment a
			// block starts rescuing, which is why they are still
			// readable in always: and after the block — measured, not
			// assumed.
			st.vc.SetVar(vars.Facts, "ansible_failed_task",
				failedTaskDict(st.lastFailedTask, ec.play, resolvedConnection(ec.play, st.vc.Merged())))
			st.vc.SetVar(vars.Facts, "ansible_failed_result", st.lastFailedResult)
		}
		afterRescue := ec.runTaskList(ctx, task.Rescue, newlyFailed, pr)
		rescueFailed := diff(newlyFailed, afterRescue)
		for _, h := range afterRescue {
			pr.recordRescued(h)
		}
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

// pushRoleVars merges a role's defaults/vars onto the RoleDefaults and
// RoleVars layers of every active host, and returns the function that
// unwinds them again. The CALLER decides whether to run it, because
// whether a role's variables outlive the role is a property of how the
// role was invoked, measured against real ansible-core 2.21.4 by running
// each form in isolation:
//
//   - include_role is dynamic, and its variables leave scope with it —
//     after it, the role's own vars read as undefined. Scoped.
//   - import_role and a roles: entry are static, and real Ansible
//     injects their variables for the whole play — after either, the
//     role's vars still resolve. Not scoped.
//
// This port unwound all three, so every role variable read as undefined
// the moment the role finished, including for roles: — the common case.
//
// Merging rather than replacing keeps nesting correct either way: a role
// included from inside another role's tasks stacks its own defaults/vars
// on top of the enclosing role's, so a variable the inner role does not
// define still resolves to the outer role's value, and unwinding returns
// to exactly what the enclosing role had.
// pushBlockVars layers vals over whatever block vars are already in
// scope for each host and returns the undo. Merging rather than
// replacing is what makes a block nested inside another see both sets,
// and restoring the PRIOR map (not deleting the layer) is what lets
// that nesting unwind to any depth.
func (ec *execCtx) pushBlockVars(active []string, vals map[string]any) func() {
	prior := make(map[string]map[string]any, len(active))
	for _, h := range active {
		st := ec.states[h]
		priorVars := st.vc.Layer(vars.BlockVars)
		prior[h] = priorVars
		st.vc.Set(vars.BlockVars, mergeOnto(priorVars, vals))
	}
	return func() {
		for _, h := range active {
			ec.states[h].vc.Set(vars.BlockVars, prior[h])
		}
	}
}

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
// filtered is false for handlers, which are exempt from --tags and
// --start-at-task (they run because something notified them) and are
// bannered differently.
func (ec *execCtx) runSingleTask(ctx context.Context, task Task, active []string, pr *PlayResult, filtered bool) []string {
	isHandler := !filtered
	// A task excluded by tags produces NO result at all — no banner, no
	// "skipping:" line, and nothing in the recap. Measured: real
	// ansible-core running this port's own tag playbook with
	// --tags alpha shows only the two selected tasks and reports
	// skipped=0, where this port showed three "skipping: [h1]" lines
	// and skipped=3.
	//
	// That is the difference between SELECTION and skipping: tags
	// decide which tasks are in the play at all, whereas when: skips a
	// task that is. Reporting the first as the second also made the
	// recap's own skipped count wrong.
	if filtered && !tagsMatch(task.Tags, ec.engine.RunTags, ec.engine.SkipTags) {
		return active
	}
	// --start-at-task, applied after the tag filter so a task excluded
	// by tags cannot be the one that starts the run.
	if filtered && !ec.engine.startAtReached(task) {
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
	if isRunOnce(task) && len(active) > 1 {
		runOn = active[:1]
		passthrough = active[1:]
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := make(map[string]bool, len(runOn))

	// With a single fork there is no concurrency to gain, and letting
	// goroutines race for the one slot makes the ORDER they run in
	// arbitrary — which defeats order: and makes `-f 1` output differ
	// between runs. Real ansible-core at forks 1 runs hosts strictly in
	// order, so this does too.
	if ec.engine.Forks == 1 {
		for _, h := range runOn {
			st := ec.states[h]
			if ec.runTaskOnHost(ctx, task, st, pr, isHandler) {
				st.failed = true
			} else {
				succeeded[h] = true
			}
		}
	} else {
		// Hold each host's output and emit it in host order once they
		// have all finished — the work stays concurrent, the
		// TRANSCRIPT stops depending on which goroutine won.
		ec.bufferHosts(runOn)
		for _, h := range runOn {
			wg.Add(1)
			go func(h string) {
				defer wg.Done()
				release := ec.acquire()
				defer release()
				st := ec.states[h]
				failed := ec.runTaskOnHost(ctx, task, st, pr, isHandler)
				mu.Lock()
				if failed {
					st.failed = true
				} else {
					succeeded[h] = true
				}
				mu.Unlock()
			}(h)
		}
		wg.Wait()
		ec.flushPending(pr, runOn)
	}
	wg.Wait()

	// Rebuilt by walking runOn in order rather than by appending as each
	// goroutine finishes, so the active list stays in INVENTORY order.
	// It used to come back in completion order, which made run_once pick
	// whichever host happened to win the race: real Ansible always runs
	// it on the play's first host, and measuring four runs gave h1 four
	// times there against h2,h2,h2,h1 here. A run_once task that
	// registers a variable would otherwise record a different
	// inventory_hostname from one run to the next.
	stillActive := make([]string, 0, len(runOn))
	for _, h := range runOn {
		if succeeded[h] {
			stillActive = append(stillActive, h)
		}
	}

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

// untaggedTag is the tag real Ansible implicitly gives a task that
// declares none (Taggable.untagged, a frozenset of exactly this one
// name). It is what makes `--tags untagged` and `--skip-tags untagged`
// able to select those tasks at all.
const untaggedTag = "untagged"

// tagsMatch is a port of real Ansible's Taggable.evaluate_tags
// (ansible/playbook/taggable.py), which is more than an intersection
// test: `all`, `tagged`, `untagged`, `always` and `never` are special
// names on both sides, and the run side is evaluated BEFORE the skip
// side rather than the other way round.
//
// An EMPTY run list means `all` here, not "no filter". Real Ansible does
// that substitution in its CLI rather than its config
// (ansible/cli/__init__.py post_process_args, whose own comment explains
// why: making ["all"] the config default would turn `--tags foo` into
// ["all", "foo"]). It is applied here instead of in this port's CLI so
// that every entry point gets it — a task tagged `never` running by
// default is a safety failure, and guarding a destructive task is the
// only thing `never` is for.
func tagsMatch(effective, run, skip []string) bool {
	tags := effectiveTagSet(effective)
	// "tags != self.untagged" in the original: the set is EXACTLY the
	// implicit one, not merely small.
	onlyUntagged := len(tags) == 1 && tags[untaggedTag]

	only := tagSet(run)
	if len(only) == 0 {
		only = map[string]bool{"all": true}
	}

	shouldRun := false
	switch {
	case tags["always"]:
		shouldRun = true
	case only["all"] && !tags["never"]:
		shouldRun = true
	case intersectsSet(tags, only):
		shouldRun = true
	case only["tagged"] && !onlyUntagged && !tags["never"]:
		shouldRun = true
	}

	if shouldRun && len(skip) > 0 {
		sk := tagSet(skip)
		switch {
		case sk["all"]:
			// Everything goes, except `always` tasks — unless `always`
			// was itself named as something to skip.
			if !tags["always"] || sk["always"] {
				shouldRun = false
			}
		case intersectsSet(tags, sk):
			shouldRun = false
		case sk["tagged"] && !onlyUntagged:
			shouldRun = false
		}
	}
	return shouldRun
}

// effectiveTagSet is a task's own tags, or the implicit {"untagged"}
// when it declares none.
func effectiveTagSet(tags []string) map[string]bool {
	set := tagSet(tags)
	if len(set) == 0 {
		return map[string]bool{untaggedTag: true}
	}
	return set
}

func tagSet(tags []string) map[string]bool {
	set := make(map[string]bool, len(tags))
	for _, t := range tags {
		set[t] = true
	}
	return set
}

func intersectsSet(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
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

func (ec *execCtx) runTaskOnHost(ctx context.Context, task Task, st *hostState, pr *PlayResult, isHandler bool) bool {
	// Noted here rather than at each of the six places a task can
	// fail, so ec.report can attribute ANY failure to the task that
	// caused it — a when: that would not evaluate included.
	st.currentTask = task
	scope := st.vc.Child()
	scope.Set(vars.TaskVars, task.Vars)

	// A task that does NOT loop evaluates its when: once, here. A
	// looping one evaluates it PER ITEM instead (below), because the
	// condition normally mentions `item` — and `item` does not exist
	// yet at this point.
	//
	// Evaluating it here for a looping task was wrong in both
	// directions, measured against real ansible-core:
	// `when: "item != 2"` ran every iteration (undefined != 2 is true)
	// where real skips the second, and `when: "item == 1"` skipped the
	// whole task where real runs the first. A playbook that says to
	// skip an item acted on it anyway.
	if task.When != "" && task.Loop == nil {
		ok, err := ec.evalWhen(task.When, scope.Merged())
		if err != nil {
			ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Ignored: task.IgnoreErrors, Msg: conditionalFailure("when", err)})
			return !task.IgnoreErrors
		}
		if !ok {
			ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Skipped: true})
			return false
		}
	}

	// meta is not a module at all — real Ansible's strategy executes it
	// directly. It banners its task and reports NO result line, and
	// contributes nothing to the recap. flush_handlers runs its
	// handlers AFTER that banner, which is why this is handled here,
	// ahead of the ordinary result path, rather than inside
	// runDirective: reporting the meta result afterwards put the
	// handler's own banner above the meta task's.
	if modules.NormalizeName(task.Module) == "meta" {
		ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, BannerOnly: true})
		if err := ec.runMeta(ctx, task.Args, st, pr); err != nil {
			ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Ignored: task.IgnoreErrors, Msg: err.Error()})
			st.failed = true
			return !task.IgnoreErrors
		}
		return false
	}

	items := []any{nil}
	looping := task.Loop != nil
	if looping {
		rendered, err := ec.engine.Template.RenderValue(task.Loop, scope.Merged())
		if err != nil {
			ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Ignored: task.IgnoreErrors, Msg: "loop: " + err.Error()})
			return !task.IgnoreErrors
		}
		if list, ok := rendered.([]any); ok {
			items = list
		} else {
			items = []any{rendered}
		}
		// with_<name>: the rendered value supplies the lookup's TERMS
		// — a list spreads into several (with_nested: [[1,2],[a,b]]
		// is two terms), anything else is one (with_dict: "{{ d }}").
		// The plugin then produces the items to loop over.
		if task.LoopWith != "" {
			produced, lerr := ec.engine.Template.Lookup(task.LoopWith, items, scope.Merged(), nil)
			if lerr != nil {
				ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Ignored: task.IgnoreErrors,
					Msg: "with_" + task.LoopWith + ": " + lerr.Error()})
				return !task.IgnoreErrors
			}
			items = produced
		}
	}

	anyChanged, anyFailed := false, false
	var lastResult modules.Result
	var lastExtra map[string]any
	// Where the task actually ran, for the result to report. Stays
	// empty for a task that never connected — which is why a skipped
	// task shows no delegate, matching real Ansible's own
	// "skipping: [h1]" for a delegated task.
	delegate := ""

	// loopResults collects one entry per iteration for a looped task's
	// registered value — real Ansible's own `results` list. Nil for a
	// task that isn't looping, which registers its module fields flat.
	var loopResults []any

	// argsFailed records that an ITERATION could not have its
	// arguments finalized, which real closes the task with a summary
	// line for.
	argsFailed := false

	for index, item := range items {
		iter := scope
		// What this iteration prints in its "(item=...)": the item
		// itself, unless loop_control.label named something else.
		// Declared out here because every report site in the
		// iteration uses it, not just the ones inside the loop setup.
		label := item
		if looping {
			iter = scope.Child()
			iter.SetVar(vars.TaskVars, task.LoopVar, item)
			if task.LoopLabel != "" {
				// Rendered with the item already in scope, so
				// `label: "{{ item.name }}"` resolves per iteration.
				if rendered, lerr := ec.engine.Template.Render(task.LoopLabel, iter.Merged()); lerr == nil {
					label = rendered
				}
			}
			if task.IndexVar != "" {
				// loop_control.index_var, 0-based as in real Ansible.
				iter.SetVar(vars.TaskVars, task.IndexVar, index)
			}

			// Now that `item` is bound, the condition can be asked
			// about THIS item.
			if task.When != "" {
				ok, werr := ec.evalWhen(task.When, iter.Merged())
				if werr != nil {
					ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Ignored: task.IgnoreErrors, Msg: conditionalFailure("when", werr), Item: item, ItemLabel: label, LoopVar: task.LoopVar, Looped: true})
					anyFailed = true
					if !task.IgnoreErrors {
						break
					}
					continue
				}
				if !ok {
					ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Skipped: true, Item: item, ItemLabel: label, LoopVar: task.LoopVar, Looped: true})
					continue
				}
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

			args, err := ec.renderArgs(task, mergedVars)
			if err != nil {
				// An args failure inside a LOOP is reported per item,
				// like any other iteration failure, and the task then
				// closes with real's own summary line. Measured: that
				// summary belongs to an args-finalization failure and
				// NOT to a module failure, which ends on its per-item
				// lines alone.
				ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module,
					Failed: true, Ignored: task.IgnoreErrors, Msg: err.Error(),
					Item: item, ItemLabel: label, LoopVar: task.LoopVar, Looped: looping,
					ArgsFailed: true})
				anyFailed = true
				aborted = true
				argsFailed = looping
				break
			}
			if task.Module == "template" {
				args["_vars"] = mergedVars
			}

			var handled bool
			var derr error
			result, handled, derr = ec.runDirective(ctx, task, st, args, mergedVars, pr)
			if derr != nil {
				ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Ignored: task.IgnoreErrors, Msg: derr.Error()})
				anyFailed = true
				aborted = true
				break
			}
			if !handled && st.conn == nil && st.connErr != nil &&
				!connectionlessModules[modules.NormalizeName(task.Module)] && task.DelegateTo == "" {
				// The host has no connection and this task needs one.
				// Real Ansible re-attempts the connection for every
				// such task, so this reports UNREACHABLE again rather
				// than skipping silently — and drops the host unless
				// THIS task says to ignore it.
				if ec.reportUnreachable(pr, st, task, st.connErr) {
					return true
				}
				return false
			}
			if !handled {
				conn, dname, cerr := ec.connectionFor(ctx, task, mergedVars, st)
				delegate = dname
				if cerr != nil {
					ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module, Failed: true, Ignored: task.IgnoreErrors, Msg: cerr.Error(), Delegate: delegate})
					anyFailed = true
					aborted = true
					break
				}
				resolveRoleSrc(task, args)
				// Real Ansible sends _ansible_diff to every module,
				// whether or not it knows what to do with one.
				if ec.engine.DiffMode && args != nil {
					args[modules.DiffModeKey] = true
				}
				if env := ec.taskEnvironment(task, mergedVars); len(env) > 0 && args != nil {
					args[modules.EnvironmentKey] = env
				}
				if skip, res := ec.checkModeGate(task, args); skip {
					result = res
				} else if task.Async > 0 {
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
			// A changed_when:/failed_when: that will not EVALUATE is a
			// task failure, not something to shrug off. Both errors
			// were dropped here, so a broken expression left the task
			// reporting whatever the module said — green, on a
			// condition that never ran.
			if task.ChangedWhen != "" {
				ok, cerr := ec.evalWhen(task.ChangedWhen, withResult(mergedVars, resultView))
				if cerr != nil {
					result.Failed = true
					result.Msg = conditionalFailure("changed_when", cerr)
					result = result.WithExtra("changed_when_result", conditionalResultOf(cerr))
				} else {
					result.Changed = ok
				}
			}
			if task.FailedWhen != "" && !result.Failed {
				ok, ferr := ec.evalWhen(task.FailedWhen, withResult(mergedVars, resultView))
				if ferr != nil {
					result.Failed = true
					result.Msg = conditionalFailure("failed_when", ferr)
					result = result.WithExtra("failed_when_result", conditionalResultOf(ferr))
				} else {
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
				if ok, uerr := ec.evalWhen(cond, withResult(mergedVars, attemptView)); uerr == nil {
					passed = ok
				} else {
					passed = false
				}
			}
			if passed {
				break
			}
			if attempt < totalAttempts {
				// Real Ansible counts down the retries REMAINING, so a
				// task with retries: 3 reports 2, then 1, then 0 before
				// its final failure. totalAttempts is 1 + retries.
				for _, cb := range ec.engine.Callbacks {
					cb.OnTaskRetry(Result{
						Host: st.name, Task: task.Name, Module: task.Module,
						Failed: true, Msg: result.Msg, Extra: result.Extra,
						Delegate: delegate,
					}, totalAttempts-1-attempt)
				}
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
			// Real Ansible carries attempts in the RESULT too, not only
			// in the registered variable, so a fatal line reports
			// "attempts": 3 alongside rc and stderr.
			result = result.WithExtra("attempts", totalAttempts-1)
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
			Changed: result.Changed, Failed: result.Failed,
			Skipped: result.Skipped,
			Ignored: result.Failed && task.IgnoreErrors,
			Msg:     result.Msg, Extra: result.Extra,
			Diffs:    result.Diffs,
			Delegate: delegate,
			Handler:  isHandler,
			Role:     roleName(task),
			NoLog:    task.NoLog,
			Item:     item, ItemLabel: label, LoopVar: task.LoopVar,
			Looped: looping,
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
		// Where a module's facts land is a PRECEDENCE decision, not a
		// storage one. Real Ansible puts gathered facts near the bottom
		// of the ladder but set_fact near the top, above play vars — so
		// a play var and a set_fact of the same name resolve to the
		// set_fact. Sending both to the Facts layer made the play var
		// win, measured against real ansible-core 2.21.4.
		factVars := result.Facts
		layer := vars.Registered
		if task.Module == "setup" || task.Module == "gather_facts" {
			factVars = vars.InjectFacts(result.Facts)
			layer = vars.Facts
		}
		for k, v := range factVars {
			st.vc.SetVar(layer, k, v)
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
			for _, i := range ec.handlersFor(name) {
				st.notify[i] = true
			}
		}
	}

	// Real closes a loop whose arguments would not finalize with one
	// task-level line after the per-item ones. It does NOT do this for
	// a loop whose items merely failed in the module — measured both
	// ways, which is why this keys on argsFailed rather than on
	// anyFailed.
	if argsFailed {
		ec.report(pr, Result{Host: st.name, Task: task.Name, Module: task.Module,
			Failed: true, Ignored: task.IgnoreErrors, Msg: "One or more items failed",
			ArgsFailed: true, DisplayOnly: true})
	}

	return anyFailed && !task.IgnoreErrors
}

// connectionFor resolves the Connection an ordinary module task should
// run against: the delegate_to target's connection if set (rendered as
// a template, since delegate_to may reference a variable), otherwise
// this host's own connection — wrapped in Become if escalation applies.
// It also returns the delegate's resolved name, so the result can
// report WHERE the task ran; empty when the task ran on its own host.
func (ec *execCtx) connectionFor(ctx context.Context, task Task, mergedVars map[string]any, st *hostState) (remoteexec.Connection, string, error) {
	conn := st.conn
	delegate := ""
	if task.DelegateTo != "" {
		delegateName, err := ec.engine.Template.Render(task.DelegateTo, mergedVars)
		if err != nil {
			return nil, "", fmt.Errorf("delegate_to: %w", err)
		}
		delegate = delegateName
		delegateVars := mergedVars
		if hv := ec.engine.Inventory.HostVars(delegateName); len(hv) > 0 {
			delegateVars = hv
		}
		dconn, err := ec.delegateConn(ctx, delegateName, delegateVars)
		if err != nil {
			return nil, delegate, fmt.Errorf("delegate_to %s: %w", delegateName, err)
		}
		conn = dconn
	}
	if becomeCfg, ok := becomeConfigFor(ec.play, task, mergedVars); ok {
		// become_user is templated, as it is there: `become_user:
		// "{{ deploy_user }}"` reached sudo as the literal braces here,
		// so the escalation failed with "unknown user {{ ... }}".
		if rendered, rerr := ec.engine.Template.Render(becomeCfg.User, mergedVars); rerr == nil {
			becomeCfg.User = rendered
		}
		conn = remoteexec.Become(conn, becomeCfg)
	}
	return conn, delegate, nil
}

// runDirective handles the small set of task "modules" that must run
// inside the engine itself rather than through modules.Registry,
// because they mutate engine-level state (the inventory, this host's
// own variable layers, or the play's handler-notify set) instead of
// just talking to a Connection. handled reports whether task.Module
// named one of these — when false, the caller falls through to the
// ordinary module dispatch.
func (ec *execCtx) runDirective(ctx context.Context, task Task, st *hostState, args map[string]any, mergedVars map[string]any, pr *PlayResult) (result modules.Result, handled bool, err error) {
	switch task.Module {
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
	case "debug":
		// Only the var: form needs the engine. `var` names a variable to
		// look up, and only the engine holds the host's variables — which
		// is why real Ansible's debug is an action plugin. The msg: form
		// needs nothing and falls through to the module.
		name, ok := args["var"].(string)
		if !ok {
			return modules.Result{}, false, nil
		}
		return ec.runDebugVar(name, mergedVars), true, nil
	default:
		return modules.Result{}, false, nil
	}
}

// runDebugVar implements `debug: {var: NAME}`. Real Ansible reports the
// variable's VALUE keyed by its own name — {"d": {"a": 1}} — and sets no
// msg at all. This port used to hand the module the bare name, so it
// echoed the string "d" as both msg and var, printing the name of the
// variable instead of what was in it.
//
// A name that resolves to nothing reports real Ansible's own
// "VARIABLE IS NOT DEFINED!" rather than an empty value, so a typo in a
// debug task looks like a typo.
func (ec *execCtx) runDebugVar(name string, merged map[string]any) modules.Result {
	value, ok := merged[name]
	if !ok {
		// The name may be an expression rather than a bare variable
		// ("ansible_facts.os_family"), which real Ansible also accepts.
		//
		// A nil result counts as UNDEFINED: the template engine returns
		// no error for a name it cannot resolve, so without this every
		// undefined variable printed as null instead of saying so. The
		// narrow cost is that an expression resolving to a genuine null
		// reads as undefined too; a bare name set to null is unaffected,
		// because the lookup above finds it first.
		if v, err := ec.engine.Template.Eval(name, merged); err == nil && v != nil {
			value, ok = v, true
		}
	}
	if !ok {
		// Real ansible-core 2.21's own wording, measured — the leading
		// "1" is a fixed per-result error index, not a counter: three
		// undefined variables across two hosts all report error 1.
		value = fmt.Sprintf("<< error 1 - '%s' is undefined >>", name)
	}
	return modules.Result{Extra: map[string]any{name: value}}.
		WithExtra(verboseAlwaysKey, true)
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
			return modules.Fail(conditionalFailure("", err)), nil
		}
		if truthy {
			continue
		}
		// Real's own failure shape: fail_msg (aliased msg), defaulting
		// to a bare "Assertion failed" with the condition reported
		// under its own `assertion` key rather than glued onto the
		// message, plus evaluated_to. This port printed
		// "Assertion failed: 1 == 2" and neither key.
		msg := stringArg(args, "fail_msg", stringArg(args, "msg", "Assertion failed"))
		res := modules.Fail(msg).
			WithExtra("assertion", fmt.Sprintf("%v", cond)).
			WithExtra("evaluated_to", false).
			WithExtra("changed", false)
		return withAssertVerbosity(res, args), nil
	}
	res := modules.Ok(stringArg(args, "success_msg", "All assertions passed")).
		WithExtra("changed", false)
	return withAssertVerbosity(res, args), nil
}

// withAssertVerbosity reproduces real Ansible's assert action plugin
// setting _ansible_verbose_always on its result unless quiet: true —
// which is why a passing assert prints its whole result dict ("msg":
// "All assertions passed") rather than a bare "ok: [h]". This port
// printed the bare line, so an assert looked like it had not run.
func withAssertVerbosity(res modules.Result, args map[string]any) modules.Result {
	if quiet, _ := args["quiet"].(bool); quiet {
		return res
	}
	// Real's assert sets result['changed'] = False explicitly on the
	// success path, so its dump carries a "changed": false that
	// debug's — which sets no such key — does not. Two action plugins,
	// two result shapes; copied rather than unified.
	return res.WithExtra(verboseAlwaysKey, true)
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
		return ec.evalWhen(c, vars)
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
func (ec *execCtx) runMeta(ctx context.Context, args map[string]any, st *hostState, pr *PlayResult) error {
	action, _ := args["_raw_params"].(string)
	switch action {
	case "noop":
		// Real Ansible's own no-op. It is what a generated or
		// conditionally-templated playbook falls back to, so refusing
		// it broke playbooks that do nothing.
		return nil
	case "flush_handlers":
		for i, handler := range ec.play.Handlers {
			if st.notify[i] {
				// A handler, however it was reached — meta:
				// flush_handlers banners it the same way the end-of-play
				// run does.
				ec.runTaskOnHost(ctx, handler, st, pr, true)
			}
		}
		st.notify = map[int]bool{}
		return nil
	case "clear_facts":
		st.vc.Set(vars.Facts, map[string]any{})
		return nil
	default:
		return fmt.Errorf("meta: %q not supported (only noop, flush_handlers, clear_facts)", action)
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
	loaded, err := loadYAMLMap(filepath.Join(ec.engine.BaseDir, path), false, ec.engine.VaultPassword)
	if err != nil {
		return modules.Result{}, fmt.Errorf("include_vars: %w", err)
	}
	// include_vars sits above play vars and role vars in real Ansible's
	// ladder, not down with gathered facts.
	for k, v := range loaded {
		st.vc.SetVar(vars.Registered, k, v)
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

// resolvedConnection is the connection name real reports in
// ansible_failed_task, fully qualified as it is there
// ("ansible.builtin.local").
func resolvedConnection(play Play, hostVars map[string]any) string {
	name := strVar(hostVars, "ansible_connection", play.Connection)
	if name == "" {
		name = "ssh"
	}
	return "ansible.builtin." + modules.NormalizeName(name)
}

// The helpers below exist because real reports an UNSET optional
// attribute as null, where Go's zero value is "" or false. That
// difference ran through ansible_failed_task on every key a playbook
// had not written.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilIfNoStrings(v []string) any {
	if len(v) == 0 {
		return nil
	}
	return v
}

func boolPtrOrNil(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}

// registerDict is real's own shape for a register:, which is not the
// name but a map of it to an internal sentinel — {"r":
// "_task.polymorphic_result"}. Reproduced rather than simplified,
// because the point of this dict is to read like real's.
func registerDict(name string) any {
	if name == "" {
		return nil
	}
	return map[string]any{name: "_task.polymorphic_result"}
}

// isRunOnce reads the tri-state RunOnce as the boolean the engine
// needs. nil — the playbook never mentioned it — is false here and
// null in ansible_failed_task, which is the only place the
// distinction shows.
func isRunOnce(t Task) bool { return t.RunOnce != nil && *t.RunOnce }

func becomeOf(t Task, play Play) bool {
	if t.Become != nil {
		return *t.Become
	}
	return play.Become
}

func boolPtrOr(task, play *bool, def bool) bool {
	if task != nil {
		return *task
	}
	if play != nil {
		return *play
	}
	return def
}

func intPtrOr(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

func intPtrOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// mergedTaskEnvironment is the play's environment with the task's over
// it, the same merge taskEnvironment does at run time but without a
// host's variables to render against — this is a report of what the
// task DECLARED, which is what real's attribute dump carries too.
func mergedTaskEnvironment(t Task, play Play) map[string]any {
	out := map[string]any{}
	for k, v := range play.Environment {
		out[k] = v
	}
	for k, v := range t.Environment {
		out[k] = v
	}
	return out
}

// moduleDefaultsList reports module_defaults the way real does: a LIST
// of per-module maps, empty when there are none.
func moduleDefaultsList(t Task) []any {
	if len(t.ModuleDefaults) == 0 {
		return []any{}
	}
	out := map[string]any{}
	for name, args := range t.ModuleDefaults {
		out[name] = args
	}
	return []any{out}
}

// failedResultDict is what a rescue: reads back as
// ansible_failed_result: the failing task's whole result, the shape
// the callback would have dumped.
//
// "exception" is real's, wording included. It looks like a Python
// detail but is not one: real fills this slot on EVERY failed module
// result, and for a plain non-zero exit — no exception raised at all —
// it writes exactly "(traceback unavailable)". Which is also true
// here, for a different reason.
func failedResultDict(r Result) map[string]any {
	out := map[string]any{
		"changed":   r.Changed,
		"exception": "(traceback unavailable)",
		"failed":    true,
		"msg":       r.Msg,
	}
	for k, v := range r.Extra {
		if strings.HasPrefix(k, "_ansible_") {
			continue
		}
		out[k] = v
	}
	return out
}

// failedTaskDict is what a rescue: reads back as ansible_failed_task.
//
// Real dumps every one of a task's 43 field attributes
// (Task.dump_attrs), including keywords this port refuses and a couple
// of values that are pure ansible-core internals — `register` comes
// out as {"r": "_task.polymorphic_result"} there. This dict carries
// the attributes this port actually models, under real's own names and
// shapes, which covers what a rescue is written to read:
// .name, .action and .args. It is NARROWER than real's, named here
// rather than left to be discovered.
func failedTaskDict(t Task, play Play, connection string) map[string]any {
	// changed_when/failed_when/until are LISTS there, empty when
	// unset — not the bare strings this port keeps them as.
	asList := func(s string) []any {
		if s == "" {
			return []any{}
		}
		return []any{s}
	}
	args := map[string]any{}
	for k, v := range t.Args {
		args[k] = v
	}
	return map[string]any{
		"name":             t.Name,
		"action":           t.Module,
		"_resolved_action": "ansible.builtin." + t.Module,
		"args":             args,
		"any_errors_fatal": t.AnyErrorsFatal,
		"async":            t.Async,
		"become_user":      nilIfEmpty(t.BecomeUser),
		"changed_when":     asList(t.ChangedWhen),
		"delay":            t.Delay,
		"delegate_to":      nilIfEmpty(t.DelegateTo),
		"failed_when":      asList(t.FailedWhen),
		"ignore_errors":    t.IgnoreErrors,
		"loop":             t.Loop,
		"loop_control": map[string]any{
			"break_when":        []any{},
			"extended":          nil,
			"extended_allitems": true,
			"index_var":         nilIfEmpty(t.IndexVar),
			"label":             nil,
			"loop_var":          t.LoopVar,
			"pause":             t.LoopPause,
		},
		"no_log":   t.NoLog,
		"notify":   nilIfNoStrings(t.Notify),
		"register": registerDict(t.Register),
		"run_once": boolPtrOrNil(t.RunOnce),
		"tags":     t.Tags,
		"until":    asList(t.Until),
		"vars":     t.Vars,
		"when":     asList(t.When),

		// The rest of real's attribute set. Those this port MODELS
		// carry its own value; those it refuses can only ever be at
		// real's default, so reporting that default is accurate
		// rather than invented. Emitting them is what makes
		// ansible_failed_task.keys() match real's.
		"become":             becomeOf(t, play),
		"become_method":      play.BecomeMethod,
		"check_mode":         boolPtrOr(t.CheckMode, play.CheckMode, false),
		"connection":         connection,
		"diff":               false,
		"environment":        []any{mergedTaskEnvironment(t, play)},
		"ignore_unreachable": boolPtrOr(t.IgnoreUnreachable, nil, play.IgnoreUnreachable),
		"module_defaults":    moduleDefaultsList(t),
		"poll":               intPtrOr(t.Poll, 15),
		"retries":            intPtrOrNil(t.Retries),

		// Keywords this port refuses by name (unhonouredTaskKeys):
		// never anything but real's default.
		"async_val":      t.Async,
		"become_exe":     nil,
		"become_flags":   nil,
		"collections":    []any{},
		"debugger":       nil,
		"delegate_facts": nil,
		"loop_with":      nil,
		"port":           nil,
		"remote_user":    nil,
		"throttle":       0,
		"timeout":        0,
	}
}

func (ec *execCtx) report(pr *PlayResult, r Result) {
	// A failure a rescue: could catch is remembered on its host, for
	// ansible_failed_task/ansible_failed_result. An IGNORED failure is
	// not one — real only sets these when a block is actually
	// rescuing — and neither is an unreachable host, which no rescue
	// gets to run for.
	st := ec.states[r.Host]
	if r.Failed && !r.Ignored && !r.Unreachable && st != nil {
		// Recorded immediately, not at flush time: a rescue: reads it
		// back while the block is still deciding what to do.
		st.lastFailedTask = st.currentTask
		st.lastFailedResult = failedResultDict(r)
	}
	// While a task is running several hosts at once, a host's results
	// are held and emitted in HOST ORDER when they have all finished.
	// Emitting as each goroutine happened to finish made the
	// transcript of an ordinary two-host play differ between runs —
	// the DEFAULT strategy, not an exotic one. Real is stable there.
	if st != nil && st.buffering {
		st.pending = append(st.pending, r)
		return
	}
	ec.emit(pr, r)
}

// emit delivers one result to the recap and every callback.
func (ec *execCtx) emit(pr *PlayResult, r Result) {
	pr.record(r)
	if ec.engine.OnResult != nil {
		ec.engine.OnResult(r)
	}
	for _, cb := range ec.engine.Callbacks {
		cb.OnTaskResult(r)
	}
}

// bufferHosts holds each host's results until flushPending emits them
// in the given order. The work still runs concurrently; only its
// REPORTING is serialized, which is what real's own single-threaded
// result loop amounts to.
func (ec *execCtx) bufferHosts(hosts []string) {
	for _, h := range hosts {
		if st := ec.states[h]; st != nil {
			st.buffering, st.pending = true, nil
		}
	}
}

func (ec *execCtx) flushPending(pr *PlayResult, hosts []string) {
	for _, h := range hosts {
		st := ec.states[h]
		if st == nil {
			continue
		}
		st.buffering = false
		for _, r := range st.pending {
			ec.emit(pr, r)
		}
		st.pending = nil
	}
}

// handlersFor resolves a notify: name to the handlers it triggers, as
// indices into the play's handler list.
//
// Two separate matches, both measured against real ansible-core
// 2.21.4 rather than read off its source — which matters here,
// because the source comment says "last handler loaded with the same
// name wins" and RUNNING it shows the FIRST one winning:
//
//   - the first handler with that name, and only that one: a second
//     handler sharing the name never runs;
//   - then every handler whose listen: carries the name, in
//     definition order, deduplicated by handler name.
//
// Both can fire for one notification: a handler named "overlap" and
// two others listening to "overlap" all run.
func (ec *execCtx) handlersFor(name string) []int {
	var out []int
	for i, h := range ec.play.Handlers {
		if h.Name != "" && h.Name == name {
			out = append(out, i)
			break
		}
	}
	seen := map[string]bool{}
	for i, h := range ec.play.Handlers {
		if !contains(h.Listen, name) {
			continue
		}
		if h.Name != "" {
			if seen[h.Name] {
				continue
			}
			seen[h.Name] = true
		}
		out = append(out, i)
	}
	return out
}

func (ec *execCtx) runHandlers(ctx context.Context, order []string, pr *PlayResult) {
	for i, handler := range ec.play.Handlers {
		var toRun []string
		for _, h := range order {
			st := ec.states[h]
			if (!st.failed || ec.engine.ForceHandlers) && st.notify[i] {
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

// roleSrcDirs names, per module, the role subdirectory real Ansible
// searches for a relative src:. These are the file-carrying modules
// whose src is a path on the CONTROLLER; a module whose src names
// something on the target (or a URL) must not appear here.
var roleSrcDirs = map[string]string{
	"copy":      "files",
	"script":    "files",
	"unarchive": "files",
	"template":  "templates",
}

// resolveRoleSrc rewrites a relative src: to the role's own copy of the
// file, which is what makes `copy: {src: hello.txt}` inside a role find
// roles/<name>/files/hello.txt. Real Ansible searches the role's
// files/ (templates/ for template) first; this port searched only the
// process working directory, so the task failed outright with "no such
// file or directory" for every role that ships a file.
//
// Only a task that came from a role is touched, only a relative src, and
// only when the role actually has that file — anything else is left
// exactly as written, so a playbook-level task keeps resolving the way
// it always did.
func resolveRoleSrc(task Task, args map[string]any) {
	sub, ok := roleSrcDirs[modules.NormalizeName(task.Module)]
	if !ok || task.RoleDir == "" || args == nil {
		return
	}
	src, ok := args["src"].(string)
	if !ok || src == "" || filepath.IsAbs(src) {
		return
	}
	for _, candidate := range []string{
		filepath.Join(task.RoleDir, sub, src),
		filepath.Join(task.RoleDir, src),
	} {
		if _, err := os.Stat(candidate); err == nil {
			args["src"] = candidate
			return
		}
	}
}

// checkModeGate decides what a task does under --check. A module that
// declares support is handed real Ansible's own _ansible_check_mode flag
// and runs; one that does not is skipped with real Ansible's own reason,
// never executed for real.
func (ec *execCtx) checkModeGate(task Task, args map[string]any) (skip bool, res modules.Result) {
	if !ec.inCheckMode(task) {
		return false, modules.Result{}
	}
	if !modules.SupportsCheckMode(task.Module) {
		return true, modules.Skipped("remote module (" + task.Module + ") does not support check mode")
	}
	if args != nil {
		args[modules.CheckModeKey] = true
	}
	return false, modules.Result{}
}

// applyLimit intersects a play's matched hosts with Engine.Limit, which
// is ansible-playbook's --limit. An empty limit is not a filter at all.
//
// The intersection is by NAME rather than by re-matching the play's
// pattern, so a play whose hosts: is already narrower than the limit
// keeps its own narrower set — the limit can only ever remove hosts.
func (e *Engine) applyLimit(hosts []*inventory.Host) ([]*inventory.Host, error) {
	if e.Limit == "" {
		return hosts, nil
	}
	allowed, unmatched, err := e.Inventory.MatchReport(e.Limit)
	if err != nil {
		return nil, fmt.Errorf("--limit %q: %w", e.Limit, err)
	}
	// A --limit term that matched nothing warns exactly as a play's
	// own pattern does — real emits the same sentence for both, from
	// the same place. This runs once per play while real evaluates the
	// subset once, which the Warner's deduplication makes invisible:
	// the line is written the first time and suppressed after.
	for _, term := range unmatched {
		e.warn("Could not match supplied host pattern, ignoring: " + term)
	}
	keep := make(map[string]bool, len(allowed))
	for _, h := range allowed {
		keep[h.Name] = true
	}
	out := make([]*inventory.Host, 0, len(hosts))
	for _, h := range hosts {
		if keep[h.Name] {
			out = append(out, h)
		}
	}
	return out, nil
}

// TagsSelect reports whether a task carrying these effective tags is
// selected by the given --tags and --skip-tags lists. It is the same
// rule the engine applies when deciding what to run, exported because
// ansible-playbook's --list-tasks and --list-tags must show exactly
// what a run would do — answering that question twice, in two places,
// is how the two drift apart.
//
// effective is a task's tags AFTER its play's and blocks' have been
// pushed down, which is what Parse already stores on Task.Tags. An
// empty run list means "all", which is what excludes a task tagged
// never; see tagsMatch for the full algorithm and where it comes from.
func TagsSelect(effective, run, skip []string) bool {
	return tagsMatch(effective, run, skip)
}

// startAtReached reports whether the run has reached Engine.StartAtTask
// yet — true for every task once it has, and true always when no
// --start-at-task was given.
func (e *Engine) startAtReached(task Task) bool {
	if e.StartAtTask == "" {
		return true
	}
	e.startAtMu.Lock()
	defer e.startAtMu.Unlock()
	if e.startAtDone {
		return true
	}
	if matchesStartAt(task, e.StartAtTask) {
		e.startAtDone = true
		return true
	}
	return false
}

// matchesStartAt is real Ansible's own test (play_iterator.py): the
// pattern matches a task's name exactly, or as a glob — and both the
// bare name and the "role : name" form a role task displays are tried.
func matchesStartAt(task Task, pattern string) bool {
	names := []string{task.Name}
	if task.RoleDir != "" && task.Name != "" {
		names = append(names, filepath.Base(task.RoleDir)+" : "+task.Name)
	}
	for _, name := range names {
		if name == pattern {
			return true
		}
		// A malformed pattern simply does not match, rather than
		// failing the run.
		if ok, err := path.Match(pattern, name); err == nil && ok {
			return true
		}
	}
	return false
}

// taskEnvironment merges the play's environment: with the task's own —
// the task wins on a key both set — and templates every value, which is
// what makes `TMPL: "hello-{{ who }}"` work.
//
// Returns nil when neither sets anything, so a task with no environment
// carries no wire flag at all.
func (ec *execCtx) taskEnvironment(task Task, mergedVars map[string]any) map[string]any {
	if len(ec.play.Environment) == 0 && len(task.Environment) == 0 {
		return nil
	}
	out := make(map[string]any, len(ec.play.Environment)+len(task.Environment))
	for _, layer := range []map[string]any{ec.play.Environment, task.Environment} {
		for k, v := range layer {
			// A value that fails to template is passed through as it
			// was written: the command still runs, and an obviously
			// wrong value in the environment is easier to see than a
			// task that did not run at all.
			rendered, err := ec.engine.Template.RenderValue(v, mergedVars)
			if err != nil {
				rendered = v
			}
			out[k] = rendered
		}
	}
	return out
}

// stripConditionDelimiters removes the {{ }} a conditional is sometimes
// written with. Real ansible-core accepts `when: "{{ flag }}"` and
// warns that it is deprecated ("Conditionals should not be surrounded
// by templating delimiters... will be removed from ansible-core version
// 2.23"); this port FAILED the task instead, because every conditional
// is evaluated by wrapping it in {{ }} — so one already wrapped became
// `{{ {{ flag }} }}`, a syntax error.
//
// Only a conditional that is ENTIRELY one expression is unwrapped:
// `{{ a }} and {{ b }}` is left alone, since removing the outer pair
// would not produce a valid expression either way.
func stripConditionDelimiters(cond string) string {
	t := strings.TrimSpace(cond)
	if !strings.HasPrefix(t, "{{") || !strings.HasSuffix(t, "}}") {
		return cond
	}
	inner := t[2 : len(t)-2]
	// A second block means this is not a single wrapped expression.
	if strings.Contains(inner, "{{") || strings.Contains(inner, "}}") {
		return cond
	}
	return strings.TrimSpace(inner)
}

// evalWhen evaluates a when:/until:/changed_when:/failed_when:
// expression, accepting the deprecated {{ }} wrapping real Ansible
// still accepts, and REQUIRING a boolean result unless
// Engine.AllowBrokenConditionals says otherwise.
//
// The boolean requirement is real ansible-core 2.21's, and the reason
// it exists is worth restating: a conditional that is not a boolean is
// usually a template used where one is not supported, and it then
// reads as ALWAYS TRUE — so the task runs every time, silently. That is
// an action difference, not a reporting one.
func (ec *execCtx) evalWhen(cond string, data map[string]any) (bool, error) {
	expr := stripConditionDelimiters(cond)

	value, err := ec.engine.Template.Eval(expr, data)
	if err != nil {
		return false, err
	}
	if b, ok := value.(bool); ok {
		return b, nil
	}

	// Not a boolean. Real Ansible refuses by default and offers one
	// temporary way out, which it plans to remove in 2.23.
	truthy, err := ec.engine.Template.EvalBool(expr, data)
	if err != nil {
		return false, err
	}
	if !ec.engine.AllowBrokenConditionals {
		// Real Ansible's own wording, minus the source position: this
		// port does not track where a conditional was written.
		return false, fmt.Errorf(
			"Conditional result (%s) was derived from value of type %q. Conditionals must have a boolean result.\n"+
				"Broken conditionals can be temporarily allowed with the ALLOW_BROKEN_CONDITIONALS configuration option.",
			pythonBool(truthy), template.PythonTypeName(value))
	}
	return truthy, nil
}

// pythonBool renders a Go bool the way Python prints one, which is how
// real Ansible words the conditional error.
func pythonBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// withPlayConnection adds the play's connection:, remote_user: and
// port: to a host's variables, WITHOUT overriding what the host itself
// declares. That is the precedence real Ansible applies — measured with
// a host var of ssh against a play keyword of local, where the host var
// won, and a task keyword against a play keyword, where the task won.
//
// Ignoring these was not a cosmetic gap: a play saying
// `connection: local` was connected to over SSH, so every task on it
// came back UNREACHABLE.
func withPlayConnection(play Play, vars map[string]any) map[string]any {
	defaults := map[string]any{}
	if play.Connection != "" {
		defaults["ansible_connection"] = play.Connection
	}
	if play.RemoteUser != "" {
		defaults["ansible_user"] = play.RemoteUser
	}
	if play.Port != 0 {
		defaults["ansible_port"] = play.Port
	}
	if len(defaults) == 0 {
		return vars
	}

	out := make(map[string]any, len(vars)+len(defaults))
	for k, v := range defaults {
		out[k] = v
	}
	// The host's own values go in second, so they win.
	for k, v := range vars {
		out[k] = v
	}
	return out
}

// inCheckMode decides whether one task is a dry run: its own
// check_mode: if it set one, else its play's, else the --check flag.
//
// The tri-state matters in both directions. A play saying
// `check_mode: true` is a dry run even without the flag — this port
// ignored that, so a playbook asking to change NOTHING wrote the file.
// And a task saying `check_mode: false` runs for real even under
// --check, which is how a playbook reads the state it needs in order to
// predict the rest.
func (ec *execCtx) inCheckMode(task Task) bool {
	if task.CheckMode != nil {
		return *task.CheckMode
	}
	if ec.play.CheckMode != nil {
		return *ec.play.CheckMode
	}
	return ec.engine.CheckMode
}

// orderHosts sequences a play's hosts the way its order: asks. The
// names arrive in inventory order, which is the default and what every
// other mode is derived from.
//
// Measured against real ansible-core 2.21.4 with five hosts: inventory
// and sorted both give h1..h5 there, reverse_sorted and
// reverse_inventory both give h5..h1, and shuffle is random. An
// unknown value is left alone rather than rejected, matching how this
// port treats the rest of a play it does not fully model.
func orderHosts(order []string, mode string) []string {
	out := append([]string(nil), order...)
	switch mode {
	case "", "inventory":
		return out
	case "sorted":
		sort.Strings(out)
	case "reverse_sorted":
		sort.Sort(sort.Reverse(sort.StringSlice(out)))
	case "reverse_inventory":
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	case "shuffle":
		rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	}
	return out
}

// renderArgs renders a task's arguments ONE TOP-LEVEL KEY AT A TIME,
// so a failure can say which argument it was — which is what real
// does, and why its message is useful:
//
//	Task failed: Finalization of task args for 'ansible.builtin.copy'
//	failed: Error while resolving value for 'content': 'nope' is undefined
//
// Rendering the whole map in one call, as this did, can only report
// the innermost error with no idea which argument produced it.
//
// Keys are rendered in sorted order so the argument NAMED is the same
// one on every run: a Go map would otherwise pick whichever failing
// key it reached first.
func (ec *execCtx) renderArgs(task Task, vars map[string]any) (map[string]any, error) {
	raw := task.argsWithDefaults()
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	args := make(map[string]any, len(raw))
	for _, k := range keys {
		if isConditionalArg(task.Module, k) {
			// A conditional is not templated on its way in: it is an
			// EXPRESSION, evaluated later with the delimiter rules
			// when: has. Rendering it first turned an undefined name
			// into an args-finalization failure, where real reports
			// "Error while evaluating conditional".
			args[k] = raw[k]
			continue
		}
		// Rendered INSIDE a one-key map rather than on its own: omit
		// is defined as dropping its containing entry, and a bare
		// value has no container — RenderValue rightly refuses it.
		rendered, err := ec.engine.Template.RenderValue(map[string]any{k: raw[k]}, vars)
		if err != nil {
			return nil, fmt.Errorf(
				"Task failed: Finalization of task args for '%s' failed: Error while resolving value for '%s': %w",
				fqcn(task.Module), k, innermost(err))
		}
		one, _ := rendered.(map[string]any)
		value, present := one[k]
		if !present {
			continue // omitted
		}
		args[k] = value
	}
	return args, nil
}

// conditionalFailure words a when:/changed_when:/failed_when: failure
// the way real does.
func conditionalFailure(keyword string, err error) string {
	// A conditional carrying {{ }} inside a larger expression is a
	// SYNTAX error there, with its own sentence — measured:
	// `that: "{{ n }} == 2"` is refused, while `that: "{{ n }}"`
	// (the whole thing) is the bare-template form and merely
	// deprecated. The detail after the colon is this port's parser
	// talking; the sentence in front of it is real's.
	if strings.Contains(err.Error(), "parsing expression") && strings.Contains(err.Error(), "{{") {
		return "Task failed: Syntax error in expression. Template delimiters are not supported in expressions: " +
			innermostDetail(err)
	}
	// assert's `that` has no keyword to name: real reports it as a
	// bare "Error while evaluating conditional".
	if keyword == "" {
		return fmt.Sprintf("Task failed: Error while evaluating conditional: %v", innermost(err))
	}
	if keyword == "when" {
		return fmt.Sprintf("Task failed: A 'when' expression failed: Error while evaluating conditional: %v", innermost(err))
	}
	return fmt.Sprintf("Task failed: Action failed: A '%s' expression failed: Error while evaluating conditional: %v", keyword, innermost(err))
}

// fqcn qualifies a module name the way real reports it in an error.
func fqcn(module string) string {
	if strings.Contains(module, ".") {
		return module
	}
	return "ansible.builtin." + module
}

// innermost strips this port's own wrapping off a template error, so
// the message ends on the part real also prints — "'nope' is
// undefined" — rather than on three layers of Go context in front of
// it.
func innermost(err error) error {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		if tail := msg[i+2:]; strings.HasSuffix(tail, "is undefined") {
			return errors.New(tail)
		}
	}
	return err
}

// conditionalResultOf is what real puts in changed_when_result /
// failed_when_result when the expression itself could not be
// evaluated: the inner reason, without the "Task failed: Action
// failed:" wrapping that the msg carries.
func conditionalResultOf(err error) string {
	return fmt.Sprintf("Error while evaluating conditional: %v", innermost(err))
}

// isConditionalArg reports whether a module argument holds a
// CONDITIONAL rather than a value — assert's `that` is the one this
// port implements. Real evaluates those with the same rules as
// `when:`, delimiters included: `that: "{{ x }}"` is the bare-template
// form, while `that: "{{ n }} == 2"` is a syntax error there because
// the delimiters sit inside a larger expression.
func isConditionalArg(module, key string) bool {
	return modules.NormalizeName(module) == "assert" && key == "that"
}

// innermostDetail is innermost's message without any "'x' is
// undefined" special-casing — the parser's own last clause, for an
// error whose shape this port does not reproduce word for word.
func innermostDetail(err error) string {
	// Split on the QUOTE that closes this port's own prefix
	// (`parsing expression "<src>": `) rather than on the last
	// ": " — a parser detail like `Expected ":" (Line: 1 Col: 9)`
	// has colons of its own, and splitting on the last one cut the
	// message in half.
	msg := err.Error()
	if i := strings.LastIndex(msg, `": `); i >= 0 {
		return msg[i+3:]
	}
	return msg
}

// magicHostVars are the per-host magic variables real sets on every
// host: its own name, the short form up to the first dot, and the
// groups it belongs to.
//
// group_names EXCLUDES "all" and is sorted — measured: a host in web
// and prod reports ['prod', 'web'], and a host in no real group
// reports ['ungrouped'] rather than an empty list.
func magicHostVars(inv *inventory.Inventory, host string) map[string]any {
	short := host
	if i := strings.Index(short, "."); i > 0 {
		short = short[:i]
	}
	var names []string
	for _, g := range inv.GroupsForHost(host) {
		if g.Name != "all" {
			names = append(names, g.Name)
		}
	}
	sort.Strings(names)
	if names == nil {
		names = []string{}
	}
	return map[string]any{
		"inventory_hostname":       host,
		"inventory_hostname_short": short,
		"group_names":              names,
	}
}

// groupsVar is real's `groups`: every group's host list, sorted, with
// "all" and "ungrouped" among them. It is what a playbook reads to
// write one host's config from the whole inventory —
// `{{ groups['web'] }}`.
func (e *Engine) groupsVar() map[string]any {
	members := make(map[string][]string, len(e.Inventory.Groups))
	for name := range e.Inventory.Groups {
		members[name] = nil
	}
	// Membership is TRANSITIVE: GroupsForHost walks the ancestry, so a
	// host in web lands in "all" too — which is what makes
	// groups['all'] the whole inventory while "all" itself lists no
	// host directly.
	for host := range e.Inventory.Hosts {
		for _, g := range e.Inventory.GroupsForHost(host) {
			members[g.Name] = append(members[g.Name], host)
		}
	}
	out := make(map[string]any, len(members))
	for name, hosts := range members {
		sort.Strings(hosts)
		if hosts == nil {
			hosts = []string{}
		}
		out[name] = hosts
	}
	return out
}

// hostvarsVar is real's `hostvars`: every host's variables, keyed by
// host name, each including that host's own magic variables — so
// `hostvars['web1']['inventory_hostname']` resolves, as it does there.
//
// The inventory-wide entries are shared rather than rebuilt per host:
// a hostvars entry that carried its own copy of hostvars would
// recurse.
func (e *Engine) hostvarsVar(groups map[string]any) map[string]any {
	out := make(map[string]any, len(e.Inventory.Hosts))
	for name := range e.Inventory.Hosts {
		vars := e.Inventory.HostVars(name)
		merged := make(map[string]any, len(vars)+4)
		for k, v := range vars {
			merged[k] = v
		}
		for k, v := range magicHostVars(e.Inventory, name) {
			merged[k] = v
		}
		merged["groups"] = groups
		out[name] = merged
	}
	return out
}
