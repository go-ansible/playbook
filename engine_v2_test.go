package playbook

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-ansible/inventory"
	"github.com/go-ansible/modules"
	remoteexec "github.com/go-remoteexec/transport"
)

// multiHostInventory returns n hosts (h1, h2, ...) each explicitly
// forced to the local connection, so serial/delegate_to tests can
// exercise more than one distinct host name without needing real SSH
// targets.
func multiHostInventory(t *testing.T, n int) *inventory.Inventory {
	t.Helper()
	doc := "all:\n  hosts:\n"
	for i := 1; i <= n; i++ {
		doc += "    h" + string(rune('0'+i)) + ":\n      ansible_connection: local\n"
	}
	inv, err := inventory.ParseYAML([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func resultsFor(rr *RunResult, host string) []Result {
	var out []Result
	for _, p := range rr.Plays {
		for _, r := range p.Results {
			if r.Host == host {
				out = append(out, r)
			}
		}
	}
	return out
}

// TestEngineMagicVariablesInventoryHostnameAndPlaybookDir is a
// regression test for a gap found by a real benchmarks run diffing
// go-ansible's rendered template output against real ansible-core's:
// inventory_hostname and playbook_dir were never populated, so any
// playbook referencing them (both are common) silently rendered empty
// instead of erroring or matching real Ansible.
func TestEngineMagicVariablesInventoryHostnameAndPlaybookDir(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: check magic vars
      debug:
        msg: "{{ inventory_hostname }}|{{ playbook_dir }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.BaseDir = "/some/playbook/dir"
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "check magic vars" && r.Msg != "localhost|/some/playbook/dir" {
			t.Fatalf("msg = %q, want %q", r.Msg, "localhost|/some/playbook/dir")
		}
	}
}

// TestEngineSetupTaskFactsUseAnsibleFactsConvention is a regression
// test found by a real compiled-binary smoke test: an explicit `setup:`
// task's facts were being merged bare-name-only (set_fact's
// convention), so `{{ ansible_facts.os_family }}`/`{{ ansible_os_family
// }}` silently rendered empty after `- setup:` even though the exact
// same data works fine when gathered automatically via `gather_facts:
// true`. Real Ansible exposes system facts identically either way.
func TestEngineSetupTaskFactsUseAnsibleFactsConvention(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - setup: {}
    - name: check nested form
      debug:
        msg: "{{ ansible_facts.os_family | default('MISSING') }}"
    - name: check flattened alias
      debug:
        msg: "{{ ansible_os_family | default('MISSING') }}"
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
	for _, r := range resultsFor(rr, "localhost") {
		if (r.Task == "check nested form" || r.Task == "check flattened alias") && r.Msg == "MISSING" {
			t.Fatalf("task %q: msg = %q, want a real os_family value after an explicit setup: task", r.Task, r.Msg)
		}
	}
}

func TestEngineBlockWhenSkipsWholeBlockIncludingRescueAlways(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - block:
        - name: should not run
          fail:
            msg: boom
      rescue:
        - name: rescue should not run
          debug: {}
      always:
        - name: always should not run
          debug: {}
      when: false
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
		t.Fatalf("run should not have failed: %+v", rr.Plays)
	}
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "should not run" || r.Task == "rescue should not run" || r.Task == "always should not run" {
			t.Fatalf("task %q ran despite the block's when being false", r.Task)
		}
	}
}

func TestEngineTagsRunTagsFilters(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: tagged a
      tags: [a]
      debug: {}
    - name: tagged b
      tags: [b]
      debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.RunTags = []string{"a"}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	var ranA, skippedB bool
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "tagged a" && !r.Skipped {
			ranA = true
		}
		if r.Task == "tagged b" && r.Skipped {
			skippedB = true
		}
	}
	if !ranA {
		t.Error("tagged a should have run")
	}
	if !skippedB {
		t.Error("tagged b should have been skipped (not in RunTags)")
	}
}

func TestEngineTagsSkipTagsFilters(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: tagged skip-me
      tags: [skip-me]
      debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.SkipTags = []string{"skip-me"}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "tagged skip-me" && !r.Skipped {
			t.Fatal("task tagged skip-me should have been skipped")
		}
	}
}

// TestEngineTagsDoNotFilterHandlers is a regression test: --tags
// filtering was originally applied uniformly by runSingleTask, which
// runHandlers also calls — so an untagged handler (the normal case)
// was silently skipped whenever RunTags was non-empty, even though it
// had genuinely been notified. Real Ansible always runs a notified
// handler regardless of tags. Caught by a real end-to-end smoke test
// with roles+tags+notify together, not by any narrower unit test.
func TestEngineTagsDoNotFilterHandlers(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: change something
      tags: [selected]
      command: "true"
      changed_when: true
      notify: my handler
  handlers:
    - name: my handler
      debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.RunTags = []string{"selected"}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "my handler" && r.Skipped {
			t.Fatal("a notified handler must run regardless of --tags, even though it carries no tags of its own")
		}
	}
}

func TestEngineTagsAlwaysBypassesRunTags(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: always runs
      tags: [always]
      debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.RunTags = []string{"unrelated"}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "always runs" && r.Skipped {
			t.Fatal("a task tagged always should run regardless of RunTags")
		}
	}
}

// trackingConnector counts concurrently-open connections, to observe
// serial's batching: with serial N, the maximum ever seen should equal
// N (or the host count, whichever is smaller); with no serial, it
// should equal the full host count (all batches are really just one).
type trackingConnector struct {
	mu      sync.Mutex
	current int
	maxSeen int
}

func (tc *trackingConnector) connect(ctx context.Context, hostName string, hostVars map[string]any) (remoteexec.Connection, error) {
	tc.mu.Lock()
	tc.current++
	if tc.current > tc.maxSeen {
		tc.maxSeen = tc.current
	}
	tc.mu.Unlock()
	return &countingConn{Connection: remoteexec.NewLocal(), tc: tc}, nil
}

type countingConn struct {
	remoteexec.Connection
	tc *trackingConnector
}

func (c *countingConn) Close() error {
	c.tc.mu.Lock()
	c.tc.current--
	c.tc.mu.Unlock()
	return c.Connection.Close()
}

func TestEngineSerialLimitsConcurrentConnections(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  serial: 1
  tasks:
    - debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	tc := &trackingConnector{}
	e := New(multiHostInventory(t, 3))
	e.Connect = tc.connect
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if tc.maxSeen != 1 {
		t.Fatalf("maxSeen concurrent connections = %d, want 1 with serial: 1", tc.maxSeen)
	}
}

func TestEngineNoSerialConnectsAllHostsAtOnce(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	tc := &trackingConnector{}
	e := New(multiHostInventory(t, 3))
	e.Connect = tc.connect
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if tc.maxSeen != 3 {
		t.Fatalf("maxSeen concurrent connections = %d, want 3 with no serial", tc.maxSeen)
	}
}

// labelConn is a minimal Connection stub whose Exec always reports
// which label it was constructed with, so a delegate_to test can prove
// which target actually ran a task without needing distinct real
// hosts.
type labelConn struct{ label string }

func (c *labelConn) Exec(ctx context.Context, cmd string, stdin io.Reader) (remoteexec.Result, error) {
	return remoteexec.Result{Stdout: c.label, RC: 0}, nil
}
func (c *labelConn) Put(ctx context.Context, localPath, remotePath string, opts remoteexec.PutOptions) error {
	return nil
}
func (c *labelConn) Fetch(ctx context.Context, remotePath, localPath string) error { return nil }
func (c *labelConn) Remove(ctx context.Context, remotePath string) error           { return nil }
func (c *labelConn) TempPath(base string) string                                   { return "/tmp/" + base }
func (c *labelConn) Close() error                                                  { return nil }

func TestEngineDelegateToRunsAgainstDelegateConnection(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: delegated
      delegate_to: delegate-target
      register: out
      command: whoami
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.Connect = func(ctx context.Context, hostName string, hostVars map[string]any) (remoteexec.Connection, error) {
		return &labelConn{label: "ran-on:" + hostName}, nil
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	var found bool
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "delegated" {
			found = true
			if r.Extra["stdout"] != "ran-on:delegate-target" {
				t.Fatalf("delegated task ran against %v, want the delegate target's connection", r.Extra["stdout"])
			}
		}
	}
	if !found {
		t.Fatal("delegated task result not found")
	}
}

func TestEngineMetaFlushHandlersRunsNotifiedHandlersImmediately(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: change something
      command: "true"
      changed_when: true
      notify: my handler
    - name: flush now
      meta: flush_handlers
    - name: after flush
      debug: {}
  handlers:
    - name: my handler
      debug:
        msg: handled
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
	var handlerIdx, flushIdx, endOfPlayHandlerCount int
	results := resultsFor(rr, "localhost")
	for i, r := range results {
		if r.Task == "my handler" {
			if handlerIdx == 0 {
				handlerIdx = i
			}
			endOfPlayHandlerCount++
		}
		if r.Task == "flush now" {
			flushIdx = i
		}
	}
	if handlerIdx == 0 || handlerIdx > flushIdx {
		t.Fatalf("handler should have run during the flush (index %d), flush at %d", handlerIdx, flushIdx)
	}
	if endOfPlayHandlerCount != 1 {
		t.Fatalf("handler ran %d times, want exactly 1 (not re-run at end of play)", endOfPlayHandlerCount)
	}
}

func TestEngineMetaClearFacts(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - set_fact:
        myfact: value1
    - meta: clear_facts
    - name: check
      debug:
        msg: "{{ myfact | default('gone') }}"
      register: out
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
	// Measured against real ansible-core 2.21.4: meta: clear_facts drops
	// GATHERED facts, and a set_fact survives it. This test used to
	// assert the opposite, which only held here because set_fact wrote
	// into the same layer as gathered facts.
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "check" && r.Msg != "value1" {
			t.Fatalf("msg = %v, want the set_fact to survive meta: clear_facts", r.Msg)
		}
	}
}

func TestEngineMetaUnsupportedActionErrors(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - meta: end_play
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
		t.Fatal("meta: end_play is not supported and should fail loudly, not silently no-op")
	}
}

func TestEngineAddHostReachesLaterPlay(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - add_host:
        name: dynamic1
        groups: dynamic_group
        ansible_connection: local

- hosts: dynamic_group
  gather_facts: false
  tasks:
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
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	if len(resultsFor(rr, "dynamic1")) == 0 {
		t.Fatal("second play should have matched the dynamically added host")
	}
}

func TestEngineGroupByAddsCurrentHostToGroup(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - group_by:
        key: mygroup

- hosts: mygroup
  gather_facts: false
  tasks:
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
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	if len(resultsFor(rr, "localhost")) == 0 {
		t.Fatal("expected results for localhost")
	}
	found := false
	for _, r := range rr.Plays[1].Results {
		if r.Host == "localhost" {
			found = true
		}
	}
	if !found {
		t.Fatal("second play (hosts: mygroup) should have matched localhost after group_by")
	}
}

func TestEngineIncludeVarsSetsBareNameVars(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "extra.yml", "myvar: from_include_vars\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  gather_facts: false
  tasks:
    - include_vars: extra.yml
    - name: check
      debug:
        msg: "{{ myvar }}"
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.BaseDir = dir
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "check" && r.Msg != "from_include_vars" {
			t.Fatalf("msg = %v", r.Msg)
		}
	}
}

// TestEngineRoleDefaultsPersistAfterARolesEntry: a roles: entry is
// static, and real ansible-core 2.21.4 keeps its defaults and vars
// resolvable for the rest of the play — measured. Only include_role,
// which is dynamic, scopes them to the role (see the include/import
// pair below).
func TestEngineRoleDefaultsPersistAfterARolesEntry(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/r1/tasks/main.yml", `
- name: inside role
  debug:
    msg: "{{ x }}"
`)
	writePlaybookFile(t, dir, "roles/r1/defaults/main.yml", "x: role_default\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  gather_facts: false
  roles:
    - r1
  tasks:
    - name: after role
      debug:
        msg: "{{ x | default('unset-outside-role') }}"
`)
	pb, err := ParseFile(pbPath)
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
	var insideMsg, afterMsg any
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "inside role" {
			insideMsg = r.Msg
		}
		if r.Task == "after role" {
			afterMsg = r.Msg
		}
	}
	if insideMsg != "role_default" {
		t.Fatalf("inside role: msg = %v", insideMsg)
	}
	if afterMsg != "role_default" {
		t.Fatalf("after role: msg = %v, want the role default still resolvable after a roles: entry", afterMsg)
	}
}

// TestEngineNestedRoleInheritsAndRestoresEnclosingVars locks in the fix
// for pushRoleVars replacing (instead of merging onto) the enclosing
// role's RoleDefaults/RoleVars layers: a real ansible-playbook run of
// this exact fixture (outer role with x in defaults and y/z in vars,
// inner role included from outer's own tasks with only y in its vars
// and no defaults/main.yml at all) produces x=outer-default and
// z=outer-vars-z throughout, y=outer-vars outside inner and
// y=inner-vars only while inner's own task runs — proving real Ansible
// keeps every currently active role's defaults/vars in scope at once
// rather than having an inner role's absence of a key blank it out.
func TestEngineNestedRoleInheritsAndRestoresEnclosingVars(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/outer/defaults/main.yml", "x: outer-default\n")
	writePlaybookFile(t, dir, "roles/outer/vars/main.yml", "y: outer-vars\nz: outer-vars-z\n")
	writePlaybookFile(t, dir, "roles/outer/tasks/main.yml", `
- name: outer before
  debug:
    msg: "x={{ x }} y={{ y }} z={{ z }}"
- include_role:
    name: inner
- name: outer after
  debug:
    msg: "x={{ x }} y={{ y }} z={{ z }}"
`)
	writePlaybookFile(t, dir, "roles/inner/vars/main.yml", "y: inner-vars\n")
	writePlaybookFile(t, dir, "roles/inner/tasks/main.yml", `
- name: inner task
  debug:
    msg: "x={{ x }} y={{ y }} z={{ z }}"
`)
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  gather_facts: false
  roles:
    - outer
`)
	pb, err := ParseFile(pbPath)
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
	want := map[string]string{
		"outer before": "x=outer-default y=outer-vars z=outer-vars-z",
		"inner task":   "x=outer-default y=inner-vars z=outer-vars-z",
		"outer after":  "x=outer-default y=outer-vars z=outer-vars-z",
	}
	got := map[string]string{}
	for _, r := range resultsFor(rr, "localhost") {
		got[r.Task] = r.Msg
	}
	for task, wantMsg := range want {
		if got[task] != wantMsg {
			t.Errorf("task %q: msg = %q, want %q", task, got[task], wantMsg)
		}
	}
}

// TestEngineRegisterSetFactExposesAnsibleFacts locks in the fix for
// resultToMap only ever flattening Result.Extra: set_fact (and setup)
// return their output via Result.Facts instead, so a registered
// set_fact result was silently missing it entirely. Real Ansible
// nests it under ansible_facts on the registered result (confirmed
// against a real ansible-playbook run: `sf_result.keys()` there is
// exactly ["changed", "failed", "ansible_facts"], and
// `sf_result.ansible_facts` is the bare {"myvar": "hello"} dict set by
// set_fact — no msg key, since set_fact doesn't set one).
func TestEngineRegisterSetFactExposesAnsibleFacts(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: sf
      set_fact:
        myvar: hello
      register: sf_result
    - name: check
      debug:
        msg: "{{ sf_result.ansible_facts.myvar }}"
      when: sf_result.ansible_facts.myvar is defined
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
	var checkMsg any
	var checkSkipped bool
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "check" {
			checkMsg = r.Msg
			checkSkipped = r.Skipped
		}
	}
	if checkSkipped {
		t.Fatal("check task was skipped: sf_result.ansible_facts.myvar was not defined")
	}
	if checkMsg != "hello" {
		t.Fatalf("check: msg = %v, want %q", checkMsg, "hello")
	}
}

// TestEngineFreeStrategyRunsHostsIndependently locks in the "free"
// strategy: a real ansible-playbook run of an equivalent fixture (a
// fast host with no delay and a slow host that sleeps) shows the fast
// host completing its ENTIRE task list — including a second task after
// the sleep — before the slow host's own sleep task even finishes,
// proving free has no per-task barrier across hosts (unlike linear,
// where every host must finish task N before any host starts N+1).
// This drives real local `sleep` commands rather than a fake
// connection, so the timing difference is genuine.
func TestEngineFreeStrategyRunsHostsIndependently(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  strategy: free
  tasks:
    - name: sleep
      command: "sleep {{ sleep_secs }}"
    - name: after sleep
      debug:
        msg: done
`))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := inventory.ParseYAML([]byte(`
all:
  hosts:
    fast:
      ansible_connection: local
      sleep_secs: 0
    slow:
      ansible_connection: local
      sleep_secs: 1
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(inv)
	var mu sync.Mutex
	var order []string
	e.OnResult = func(r Result) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, r.Host+":"+r.Task)
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}

	mu.Lock()
	defer mu.Unlock()
	fastDoneIdx, slowSleepIdx := -1, -1
	for i, ev := range order {
		if ev == "fast:after sleep" {
			fastDoneIdx = i
		}
		if ev == "slow:sleep" {
			slowSleepIdx = i
		}
	}
	if fastDoneIdx == -1 || slowSleepIdx == -1 {
		t.Fatalf("missing expected events in recorded order: %v", order)
	}
	if fastDoneIdx > slowSleepIdx {
		t.Fatalf("free strategy should let fast finish its whole task list before slow's first task completes; order = %v", order)
	}
}

// TestEngineUntilRetriesSucceedsPartway locks in the common, successful
// case of a task retry loop: a real shell command that only succeeds on
// its 3rd real execution (verified against a counter file, not a fake
// connection), retried via until:/retries:, reports attempts=3 exactly
// matching the real invocation count — confirmed against a real
// ansible-playbook run of an equivalent fixture.
func TestEngineUntilRetriesSucceedsPartway(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "counter")
	script := filepath.Join(dir, "try.sh")
	// A real script file, not an inline shell one-liner threaded through
	// YAML — sidesteps three layers of quoting (Go -> YAML -> shell) that
	// have nothing to do with what this test actually verifies.
	if err := os.WriteFile(script, []byte(fmt.Sprintf(
		"#!/bin/sh\necho x >> %s\nn=$(wc -l < %s)\ntest \"$n\" -ge 3\n", counter, counter,
	)), 0o755); err != nil {
		t.Fatal(err)
	}
	pb, err := Parse([]byte(fmt.Sprintf(`
- hosts: all
  gather_facts: false
  tasks:
    - name: succeeds on 3rd try
      command: %s
      register: r
      until: r.rc == 0
      retries: 5
      delay: 0
    - name: report
      debug:
        msg: "attempts={{ r.attempts }}"
`, script)))
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
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task != "report" {
			continue
		}
		if r.Msg != "attempts=3" {
			t.Fatalf("reportMsg = %q, want %q", r.Msg, "attempts=3")
		}
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != 3 {
		t.Fatalf("real invocations = %d, want 3", got)
	}
}

// TestEngineUntilRetriesExhaustedAttemptsOffByOne locks in a real,
// verified quirk in ansible-core's own retry loop (task_executor.py's
// for-else branch): when every attempt is exhausted without until ever
// passing, the registered result's .attempts field reads
// (1+retries)-1, ONE LESS than the real number of module executions —
// confirmed both empirically against a real ansible-playbook run
// (retries: 3 → 4 real shell invocations, but .attempts reads 3) and in
// ansible-core's source. Reproduced here rather than "fixed", since a
// real playbook may already read .attempts expecting this exact value.
func TestEngineUntilRetriesExhaustedAttemptsOffByOne(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "counter")
	script := filepath.Join(dir, "always-fails.sh")
	if err := os.WriteFile(script, []byte(fmt.Sprintf(
		"#!/bin/sh\necho x >> %s\nexit 1\n", counter,
	)), 0o755); err != nil {
		t.Fatal(err)
	}
	pb, err := Parse([]byte(fmt.Sprintf(`
- hosts: all
  gather_facts: false
  tasks:
    - name: always fails
      command: %s
      register: r
      until: r.rc == 0
      retries: 3
      delay: 0
      ignore_errors: true
    - name: report
      debug:
        msg: "attempts={{ r.attempts }}"
`, script)))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	var reportMsg any
	var taskFailed bool
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "always fails" {
			taskFailed = r.Failed
		}
		if r.Task == "report" {
			reportMsg = r.Msg
		}
	}
	if !taskFailed {
		t.Fatal("exhausting every retry without until passing should fail the task")
	}
	if reportMsg != "attempts=3" {
		t.Fatalf("reportMsg = %v, want %q (off-by-one: 4 real executions, attempts reads retries-1=3)", reportMsg, "attempts=3")
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != 4 {
		t.Fatalf("real invocations = %d, want 4 (1 + retries:3)", got)
	}
}

// TestEngineRetriesWithoutUntilRetriesOnFailure locks in a real behavior
// confirmed directly from ansible-core's own source (task_executor.py):
// retries: alone, with no until: at all, still activates the retry
// loop — the implicit condition becomes "not failed", i.e. retry on
// failure until success or exhaustion. Verified against a real
// ansible-playbook run of an equivalent fixture (succeeds on the 2nd of
// up to 4 allowed attempts, .attempts reads 2).
func TestEngineRetriesWithoutUntilRetriesOnFailure(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "counter")
	script := filepath.Join(dir, "try.sh")
	if err := os.WriteFile(script, []byte(fmt.Sprintf(
		"#!/bin/sh\necho x >> %s\nn=$(wc -l < %s)\ntest \"$n\" -ge 2\n", counter, counter,
	)), 0o755); err != nil {
		t.Fatal(err)
	}
	pb, err := Parse([]byte(fmt.Sprintf(`
- hosts: all
  gather_facts: false
  tasks:
    - name: retries without until
      command: %s
      register: r
      retries: 3
      delay: 0
    - name: report
      debug:
        msg: "attempts={{ r.attempts }}"
`, script)))
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
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "report" && r.Msg != "attempts=2" {
			t.Fatalf("reportMsg = %q, want %q", r.Msg, "attempts=2")
		}
	}
}

// TestEngineRunOnceExecutesOnceAndBroadcastsRegister locks in run_once,
// verified against a real ansible-playbook run of an equivalent
// fixture: the task's module actually runs on only the first active
// host, but its registered result is visible on every host in a later
// task, not just the one that ran it.
func TestEngineRunOnceExecutesOnceAndBroadcastsRegister(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: run once task
      debug:
        msg: "hello-{{ inventory_hostname }}"
      run_once: true
      register: r
    - name: report
      debug:
        msg: "host={{ inventory_hostname }} got={{ r.msg }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(multiHostInventory(t, 3))
	var mu sync.Mutex
	var ranOn []string
	e.OnResult = func(r Result) {
		if r.Task == "run once task" {
			mu.Lock()
			ranOn = append(ranOn, r.Host)
			mu.Unlock()
		}
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	if len(ranOn) != 1 {
		t.Fatalf("run once task executed on %d hosts (%v), want exactly 1", len(ranOn), ranOn)
	}
	executor := ranOn[0]
	for _, h := range []string{"h1", "h2", "h3"} {
		var reportMsg any
		for _, r := range resultsFor(rr, h) {
			if r.Task == "report" {
				reportMsg = r.Msg
			}
		}
		want := fmt.Sprintf("host=%s got=hello-%s", h, executor)
		if reportMsg != want {
			t.Errorf("host %s: reportMsg = %v, want %q", h, reportMsg, want)
		}
	}
}

// TestEngineForksLimitsConcurrency locks in Engine.Forks actually
// throttling concurrency, using real wall-clock timing (like
// TestEngineFreeStrategyRunsHostsIndependently) rather than a fake
// connection: 4 hosts each sleep 200ms; with Forks: 2 that can only
// overlap two at a time, so the whole batch takes at least two
// sequential rounds (~400ms) — comfortably more than the ~200ms it
// would take with unlimited concurrency, without asserting a flaky
// tight bound.
func TestEngineForksLimitsConcurrency(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: sleep
      command: "sleep 0.2"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(multiHostInventory(t, 4))
	e.Forks = 2
	start := time.Now()
	rr, err := e.RunPlaybook(context.Background(), pb)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	if elapsed < 350*time.Millisecond {
		t.Fatalf("elapsed = %v, want >= ~400ms (2 sequential rounds of 4 hosts at Forks:2) — Forks doesn't appear to be throttling concurrency", elapsed)
	}
}

// TestEngineDefaultForksIsFive locks in New's default matching real
// Ansible's own DEFAULT_FORKS/ANSIBLE_FORKS default of 5, rather than
// this port's prior unlimited-concurrency behavior — a caller that
// wants the old behavior back can still set Forks to 0 after New
// returns.
func TestEngineDefaultForksIsFive(t *testing.T) {
	e := New(localhostInventory())
	if e.Forks != 5 {
		t.Fatalf("New's default Forks = %d, want 5", e.Forks)
	}
}

// queuePrompt returns an Engine.Prompt double that answers from a
// fixed queue in order, recording every (msg, private) it was asked —
// for asserting the exact prompts a vars_prompt run produces.
func queuePrompt(answers []string) (fn func(msg string, private bool) (string, error), asked *[]string) {
	i := 0
	var log []string
	return func(msg string, private bool) (string, error) {
		log = append(log, msg)
		if i >= len(answers) {
			return "", nil
		}
		a := answers[i]
		i++
		return a, nil
	}, &log
}

// TestVarsPromptBasicAndDefault locks in vars_prompt's core behavior —
// prompted values land in play vars, an empty answer with a default
// falls back to it — verified against a real ansible-playbook run of
// an equivalent fixture (piped/non-interactive there falls back to
// default the same way, just via a different path: no TTY at all
// rather than an empty typed answer).
func TestVarsPromptBasicAndDefault(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  vars_prompt:
    - name: username
      prompt: "Enter username"
      default: "anon"
    - name: password
      prompt: "Enter password"
      private: true
  tasks:
    - name: report
      debug:
        msg: "user={{ username }} pass={{ password }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	prompt, asked := queuePrompt([]string{"", "secret123"})
	e.Prompt = prompt
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "report" && r.Msg != "user=anon pass=secret123" {
			t.Fatalf("reportMsg = %q, want %q", r.Msg, "user=anon pass=secret123")
		}
	}
	wantAsked := []string{"Enter username [anon]: ", "Enter password: "}
	if len(*asked) != len(wantAsked) || (*asked)[0] != wantAsked[0] || (*asked)[1] != wantAsked[1] {
		t.Fatalf("asked = %v, want %v", *asked, wantAsked)
	}
}

// TestVarsPromptSkippedWhenExtraVarSet locks in real Ansible's own
// skip-if-already-an-extra-var behavior (verified against a real
// ansible-playbook run: no prompt happens at all when -e already
// supplies the name, confirmed there by the absence of even the
// "Not prompting" warning that a piped, non-preseeded run shows).
func TestVarsPromptSkippedWhenExtraVarSet(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  vars_prompt:
    - name: username
      default: "anon"
  tasks:
    - name: report
      debug:
        msg: "user={{ username }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.ExtraVars = map[string]any{"username": "preseeded"}
	prompt, asked := queuePrompt(nil)
	e.Prompt = prompt
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	if len(*asked) != 0 {
		t.Fatalf("asked = %v, want no prompt at all when the var is already an extra-var", *asked)
	}
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "report" && r.Msg != "user=preseeded" {
			t.Fatalf("reportMsg = %q, want %q (extra-vars must still win over the play var)", r.Msg, "user=preseeded")
		}
	}
}

// TestVarsPromptConfirmRetriesUntilMatch locks in confirm:, matching
// ansible-core's own do_var_prompt: asks twice, retries (both prompts
// again) until the two answers match.
func TestVarsPromptConfirmRetriesUntilMatch(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  vars_prompt:
    - name: password
      confirm: true
  tasks:
    - name: report
      debug:
        msg: "pass={{ password }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	// First pair mismatches (retry), second pair matches.
	prompt, asked := queuePrompt([]string{"first", "second", "match", "match"})
	e.Prompt = prompt
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "report" && r.Msg != "pass=match" {
			t.Fatalf("reportMsg = %q, want %q", r.Msg, "pass=match")
		}
	}
	if len(*asked) != 4 {
		t.Fatalf("asked %d times, want 4 (one mismatched pair, then a matching pair)", len(*asked))
	}
}

func TestParseVarsPromptRequiresName(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  vars_prompt:
    - prompt: "no name given"
  tasks: []
`))
	if err == nil {
		t.Fatal("want an error for a vars_prompt item missing name")
	}
}

// TestEngineAsyncPollZeroFiresAndForgets locks in async:/poll: 0
// (fire-and-forget), verified against a real ansible-playbook run of
// an equivalent fixture first: the task returns immediately with
// started=true, finished=false, and a real ansible_job_id — not
// waiting for the backgrounded command to actually finish.
func TestEngineAsyncPollZeroFiresAndForgets(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: async fire and forget
      command: "sleep 3"
      async: 30
      poll: 0
      register: job
    - name: report
      debug:
        msg: "started={{ job.started }} finished={{ job.finished }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	start := time.Now()
	rr, err := e.RunPlaybook(context.Background(), pb)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("elapsed = %v, want well under the backgrounded sleep 3 — poll:0 should not wait for it", elapsed)
	}
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "report" && r.Msg != "started=True finished=False" {
			t.Fatalf("reportMsg = %q, want %q", r.Msg, "started=True finished=False")
		}
	}
}

// TestEngineAsyncPollWaitsForCompletion locks in async:/poll: N>0
// (wait, checking every N seconds), verified against a real
// ansible-playbook run of an equivalent fixture first: the task blocks
// until the backgrounded command actually finishes, then reports its
// real rc/stdout.
func TestEngineAsyncPollWaitsForCompletion(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: async poll wait
      shell: "sleep 0.3; echo waited-ok"
      async: 10
      poll: 1
      register: job
    - name: report
      debug:
        msg: "finished={{ job.finished }} rc={{ job.rc }} stdout={{ job.stdout }}"
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
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "report" && r.Msg != "finished=True rc=0 stdout=waited-ok\n" {
			t.Fatalf("reportMsg = %q, want %q", r.Msg, "finished=True rc=0 stdout=waited-ok\n")
		}
	}
}

func TestEngineAsyncUnsupportedModuleFailsLoud(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: async on debug (unsupported)
      debug:
        msg: hi
      async: 30
      poll: 0
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
		t.Fatal("want the run to fail: async: is only supported for command/shell")
	}
}

// TestEngineAsyncTimeoutFailsWithoutKillingJob locks in the
// controller-side half of the timeout contract: a job still running
// once async: 's limit passes fails the task — the one disclosed
// difference from real Ansible (see modules.AsyncLaunch's doc comment)
// is that the job itself is NOT killed on the target, only confirmable
// by checking it's still genuinely running afterward via the same
// job id.
func TestEngineAsyncTimeoutFailsWithoutKillingJob(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: async times out
      command: "sleep 3"
      async: 1
      poll: 1
      register: job
      ignore_errors: true
    - name: report
      debug:
        msg: "finished={{ job.finished }} jid={{ job.ansible_job_id }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	var jid string
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "async times out" && !r.Failed {
			t.Fatal("want the timed-out task itself to be Failed")
		}
		if r.Task == "report" {
			if !strings.Contains(r.Msg, "finished=False") {
				t.Fatalf("reportMsg = %q, want finished=False", r.Msg)
			}
			if i := strings.Index(r.Msg, "jid="); i >= 0 {
				jid = r.Msg[i+len("jid="):]
			}
		}
	}
	if jid == "" {
		t.Fatal("no job id captured from the report task")
	}
	// The job itself was NOT killed — confirm it's still genuinely
	// running (or has since finished on its own), i.e. still found.
	conn := remoteexec.NewLocal()
	found, _, _, _, _, err := modules.AsyncCheck(context.Background(), conn, jid)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("job should still be found on the target after a controller-side timeout (not actively killed)")
	}
	_ = modules.AsyncCleanup(context.Background(), conn, jid)
}
