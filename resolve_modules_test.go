package playbook

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-ansible/modules"
	remoteexec "github.com/go-remoteexec/transport"
)

// TestNothingRunsBeforeAnUnresolvedModule is the one that matters: real
// refuses the whole playbook and leaves the machine alone, where this
// port used to run every task up to the bad one.
//
// The witness is a FILE, not stdout. The first probe for this compared
// real's stdout for the first task's output and found it -- because
// real's error message QUOTES the offending source lines, including the
// neighbouring one. A `command: touch` witness cannot be faked by an
// error message.
func TestNothingRunsBeforeAnUnresolvedModule(t *testing.T) {
	dir := t.TempDir()
	witness := filepath.Join(dir, "witness.txt")

	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - name: first
      command: touch ` + witness + `
    - name: second
      nosuchmodule: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	_, err = e.RunPlaybook(context.Background(), pb)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	var ue *UnresolvedModulesError
	if !errors.As(err, &ue) {
		t.Fatalf("want an *UnresolvedModulesError so a caller can exit 4 rather than 2; got %T: %v", err, err)
	}
	if _, statErr := os.Stat(witness); statErr == nil {
		t.Error("the first task RAN: a typo in a later task applied the earlier ones for real")
	}
	// Real ansible-core 2.21.4's own wording, measured from a run.
	if !strings.Contains(err.Error(), `couldn't resolve module/action "nosuchmodule"`) {
		t.Errorf("message does not match real's: %v", err)
	}
}

// The control for the test above: the SAME playbook without the bad
// module must create the witness. Otherwise "the file is absent" would
// also be satisfied by a port that cannot run `command` at all.
func TestTheWitnessIsCreatedWhenEveryModuleResolves(t *testing.T) {
	dir := t.TempDir()
	witness := filepath.Join(dir, "witness.txt")

	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - name: first
      command: touch ` + witness + `
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(localhostInventory()).RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(witness); err != nil {
		t.Fatalf("control failed: the witness was not created even with no bad module (%v)", err)
	}
}

// Every place a task can hide. Measured on real, which refuses each one
// with exit 4: inside a block, inside rescue/always, in a handler, and
// behind a bad collection prefix. Roles need no case of their own --
// parseRoles turns a play's roles: list into synthetic block tasks at
// parse time, so the block case covers them -- but one is included
// anyway, because that is an implementation detail that could change.
func TestUnresolvedModulesFoundEverywhereATaskCanHide(t *testing.T) {
	roleDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(roleDir, "roles", "r1", "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "roles", "r1", "tasks", "main.yml"),
		[]byte("- nosuchmodule: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, src string }{
		{"plain task", "tasks:\n    - nosuchmodule: {}\n"},
		{"inside a block", "tasks:\n    - block:\n        - nosuchmodule: {}\n"},
		{"inside rescue", "tasks:\n    - block:\n        - debug: {msg: x}\n      rescue:\n        - nosuchmodule: {}\n"},
		{"inside always", "tasks:\n    - block:\n        - debug: {msg: x}\n      always:\n        - nosuchmodule: {}\n"},
		{"in a handler", "handlers:\n    - {name: h, nosuchmodule: {}}\n  tasks:\n    - debug: {msg: x}\n"},
		{"a bad collection prefix", "tasks:\n    - community.nonexistent.thing: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pb, err := Parse([]byte("- name: p\n  hosts: all\n  gather_facts: false\n  " + tc.src))
			if err != nil {
				t.Fatal(err)
			}
			if got := New(localhostInventory()).UnresolvedModules(pb); len(got) == 0 {
				t.Errorf("not caught; the task would have run for real")
			}
		})
	}

	t.Run("a role's tasks", func(t *testing.T) {
		pbPath := filepath.Join(roleDir, "site.yml")
		if err := os.WriteFile(pbPath,
			[]byte("- {name: p, hosts: all, gather_facts: false, roles: [r1]}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		pb, err := ParseFile(pbPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := New(localhostInventory()).UnresolvedModules(pb); len(got) == 0 {
			t.Error("a role's bad module was not caught")
		}
	})
}

// A templated module name is resolved later, and real ACCEPTS it --
// measured: `action: "{{ mod }}"` with mod=debug exits 0. Refusing it
// would break a playbook that works against real.
func TestATemplatedModuleNameIsNotRefused(t *testing.T) {
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  vars: {mod: debug}
  tasks:
    - action: "{{ mod }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := New(localhostInventory()).UnresolvedModules(pb); len(got) != 0 {
		t.Errorf("a templated module name was refused: %+v", got)
	}
}

// A caller may register a module of their own -- Registry.Register
// exists for exactly that. The check must consult the ENGINE's registry,
// not the default one, or this feature would start refusing playbooks
// that used to work.
func TestACustomRegisteredModuleResolves(t *testing.T) {
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - mine_only: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	reg := modules.Default()
	reg.Register("mine_only", func(context.Context, remoteexec.Connection, map[string]any) (modules.Result, error) {
		return modules.Ok("mine"), nil
	})
	e.Modules = reg

	if got := e.UnresolvedModules(pb); len(got) != 0 {
		t.Errorf("a custom-registered module was refused: %+v", got)
	}
	// And the default registry still refuses it, so the case above is
	// not passing because the name happens to exist upstream.
	if got := New(localhostInventory()).UnresolvedModules(pb); len(got) != 1 {
		t.Errorf("control: the default registry should not know mine_only; got %+v", got)
	}
}

// TestEveryEngineDirectiveResolves reads runDirective's OWN switch out
// of engine.go and asserts every name it handles is in
// engineDirectives (or in the registry). The list of directives and the
// code that dispatches them are two places that must agree, and a
// directive added to one and not the other would be refused before it
// ever ran -- a new feature breaking with "couldn't resolve
// module/action", which is the least helpful possible message for it.
func TestEveryEngineDirectiveResolves(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "engine.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runDirective" {
			return true
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					names = append(names, strings.Trim(lit.Value, `"`))
				}
			}
			return true
		})
		return false
	})
	if len(names) < 5 {
		t.Fatalf("read only %d case names out of runDirective; the walk is broken, "+
			"and a passing assertion below would mean nothing: %q", len(names), names)
	}
	reg := modules.Default()
	for _, n := range names {
		if engineDirectives[n] {
			continue
		}
		if _, ok := reg.Get(n); ok {
			continue
		}
		t.Errorf("runDirective handles %q but neither engineDirectives nor the registry has it, "+
			"so a playbook using it would be refused before it ran", n)
	}
}
