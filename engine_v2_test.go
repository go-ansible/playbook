package playbook

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/go-ansible/inventory"
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
	for _, r := range resultsFor(rr, "localhost") {
		if r.Task == "check" && r.Msg != "gone" {
			t.Fatalf("msg = %v, want the fact cleared by meta: clear_facts", r.Msg)
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

func TestEngineRoleDefaultsVisibleInsideRoleAndRestoredAfter(t *testing.T) {
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
	if afterMsg != "unset-outside-role" {
		t.Fatalf("after role: msg = %v, want the role default restored/gone once the role's block ends", afterMsg)
	}
}
