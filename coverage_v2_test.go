package playbook

import (
	"context"
	"fmt"
	"testing"

	remoteexec "github.com/go-remoteexec/transport"
)

func TestParseFileMissingErrors(t *testing.T) {
	if _, err := ParseFile("/no/such/playbook.yml"); err == nil {
		t.Fatal("want an error for a missing playbook file")
	}
}

func TestIncludeRoleUnsupportedShapeErrors(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  tasks:
    - include_role: 5
`))
	if err == nil {
		t.Fatal("want an error for an include_role value that's neither a string nor a mapping")
	}
}

func TestIncludeRoleMissingNameErrors(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  tasks:
    - include_role: {}
`))
	if err == nil {
		t.Fatal("want an error for include_role with no name")
	}
}

func TestIncludeTasksNonStringErrors(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  tasks:
    - include_tasks:
        foo: bar
`))
	if err == nil {
		t.Fatal("want an error for include_tasks given a mapping instead of a path string")
	}
}

func TestParseRolesNonListErrors(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  roles: notalist
`))
	if err == nil {
		t.Fatal("want an error when roles: is not a list")
	}
}

func TestParseRoleRefUnsupportedTypeErrors(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  roles:
    - 5
`))
	if err == nil {
		t.Fatal("want an error for a roles: entry that's neither a string nor a mapping")
	}
}

func TestParseRoleRefMapMissingNameErrors(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  roles:
    - foo: bar
`))
	if err == nil {
		t.Fatal("want an error for a roles: map entry with no role/name key")
	}
}

func TestNormalizeWhenUnsupportedTypeYieldsNoCondition(t *testing.T) {
	if got := normalizeWhen(42); got != "" {
		t.Fatalf("normalizeWhen(42) = %q, want empty (no condition) for an unsupported type", got)
	}
}

func TestBatchHostsSerialEqualsHostCount(t *testing.T) {
	hosts := multiHostInventory(t, 3)
	matched, err := hosts.Match("all")
	if err != nil {
		t.Fatal(err)
	}
	batches := batchHosts(matched, 3)
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want 1 when serial == host count", len(batches))
	}
}

func TestBatchHostsSerialLargerThanHostCount(t *testing.T) {
	hosts := multiHostInventory(t, 2)
	matched, err := hosts.Match("all")
	if err != nil {
		t.Fatal(err)
	}
	batches := batchHosts(matched, 100)
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want 1 when serial exceeds host count", len(batches))
	}
}

func TestBatchHostsUnevenSplit(t *testing.T) {
	hosts := multiHostInventory(t, 3)
	matched, err := hosts.Match("all")
	if err != nil {
		t.Fatal(err)
	}
	batches := batchHosts(matched, 2)
	if len(batches) != 2 || len(batches[0]) != 2 || len(batches[1]) != 1 {
		t.Fatalf("batches = %v", batches)
	}
}

func TestEngineAddHostMissingNameErrors(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - add_host: {}
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
		t.Fatal("add_host with no name should fail")
	}
}

func TestEngineAddHostGroupsAsList(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - add_host:
        name: dyn2
        groups:
          - g1
          - g2
        ansible_connection: local

- hosts: g1
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
	if len(resultsFor(rr, "dyn2")) == 0 {
		t.Fatal("second play should have matched dyn2 via the list-form groups:")
	}
}

func TestEngineGroupByMissingKeyErrors(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - group_by: {}
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
		t.Fatal("group_by with no key should fail")
	}
}

func TestEngineIncludeVarsMissingFileErrors(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - include_vars: {}
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
		t.Fatal("include_vars with no file argument should fail")
	}
}

func TestEngineIncludeVarsFileNotFoundErrors(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - include_vars: nope.yml
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
		t.Fatal("include_vars pointing at a missing file should fail")
	}
}

func TestEngineDelegateToTemplateErrorFails(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: bad delegate template
      delegate_to: "{{ unclosed"
      debug: {}
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
		t.Fatal("an unrenderable delegate_to template should fail the task")
	}
}

func TestEngineDelegateToConnectErrorFails(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: delegate to unreachable
      delegate_to: unreachable-host
      debug: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.Connect = func(ctx context.Context, hostName string, hostVars map[string]any) (remoteexec.Connection, error) {
		if hostName == "unreachable-host" {
			return nil, fmt.Errorf("simulated dial failure")
		}
		return remoteexec.NewLocal(), nil
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if !rr.Failed() {
		t.Fatal("a delegate_to target that fails to connect should fail the task")
	}
}

func TestParseVarsFilesMalformedYAMLErrors(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "vars/bad.yml", "not: [valid")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  vars_files:
    - vars/bad.yml
  tasks: []
`)
	if _, err := ParseFile(pbPath); err == nil {
		t.Fatal("want an error for malformed YAML in a vars_files entry")
	}
}

func TestParseIncludeTasksMalformedYAMLErrors(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "bad.yml", "not: [valid")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  tasks:
    - include_tasks: bad.yml
`)
	if _, err := ParseFile(pbPath); err == nil {
		t.Fatal("want an error for malformed YAML in an include_tasks target")
	}
}

func TestParseRoleDefaultsMalformedYAMLErrors(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/r1/tasks/main.yml", "- debug: {}\n")
	writePlaybookFile(t, dir, "roles/r1/defaults/main.yml", "not: [valid")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  roles:
    - r1
`)
	if _, err := ParseFile(pbPath); err == nil {
		t.Fatal("want an error for malformed YAML in a role's defaults/main.yml")
	}
}

func TestParseRoleHandlersMalformedYAMLErrors(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/r1/tasks/main.yml", "- debug: {}\n")
	writePlaybookFile(t, dir, "roles/r1/handlers/main.yml", "not: [valid")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  roles:
    - r1
`)
	if _, err := ParseFile(pbPath); err == nil {
		t.Fatal("want an error for malformed YAML in a role's handlers/main.yml")
	}
}

func TestParseIncludeRoleWithoutOwnNameFallsBackToRoleName(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/r1/tasks/main.yml", "- debug: {}\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  tasks:
    - include_role:
        name: r1
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	if pb[0].Tasks[0].Name != "role: r1" {
		t.Fatalf("Name = %q, want the default role-name fallback", pb[0].Tasks[0].Name)
	}
}

func TestParseRoleEmptyTasksFileYieldsEmptyBlock(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/r1/defaults/main.yml", "x: 1\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  roles:
    - r1
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !pb[0].Tasks[0].IsBlock() || len(pb[0].Tasks[0].Block) != 0 {
		t.Fatalf("Tasks[0] = %+v, want an empty (but present) block for a role with no tasks/main.yml", pb[0].Tasks[0])
	}
}

func TestEngineCloseDelegatesDoesNotDoubleCloseBatchHostConnection(t *testing.T) {
	// delegate_to a host that's also this batch's own single host: the
	// delegate connection cache reuses that host's connection (see
	// delegateConn), so closeDelegates must not close it a second time
	// itself — runBatch's own defer closes it once. This just proves
	// the run completes cleanly (a double-Close on remoteexec.NewLocal()
	// wouldn't error either way, but this exercises the "reused, skip"
	// branch of closeDelegates for coverage).
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: delegate to self
      delegate_to: localhost
      debug: {}
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
}
