package playbook

import (
	"fmt"
	"strings"

	"github.com/go-ansible/modules"
)

// engineDirectives are the task names this engine runs ITSELF rather
// than through modules.Registry, so a module lookup does not find them
// and a resolution check must not refuse them.
//
// Keeping the list here, beside the check, is the part that rots: a
// directive added to runDirective's switch and forgotten here would be
// refused before it ever ran. TestEveryEngineDirectiveResolves walks the
// engine's own dispatch and fails if one is missing, so the list cannot
// silently fall behind.
var engineDirectives = map[string]bool{
	"add_host":        true,
	"flush_handlers":  true,
	"group_by":        true,
	"import_playbook": true,
	"import_role":     true,
	"import_tasks":    true,
	"include_role":    true,
	"include_tasks":   true,
	"include_vars":    true,
	"meta":            true,
	"noop":            true,
}

// UnresolvedModule is one task naming something neither the registry nor
// the engine can run. Play and Task are for the message; a task with no
// name is reported by its module, as real does.
type UnresolvedModule struct {
	Play   string
	Task   string
	Module string
}

// UnresolvedModulesError is what RunPlaybook returns instead of running
// a playbook that names a module nothing can satisfy. Callers that map
// errors to exit statuses can match it with errors.As: real Ansible
// treats this as a PARSE error (exit 4), not a task failure (exit 2),
// because no task ever runs.
type UnresolvedModulesError struct {
	Unresolved []UnresolvedModule
}

func (e *UnresolvedModulesError) Error() string {
	var b strings.Builder
	for i, u := range e.Unresolved {
		if i > 0 {
			b.WriteString("\n")
		}
		// Real ansible-core 2.21.4's own wording, measured from a run.
		fmt.Fprintf(&b, "couldn't resolve module/action %q. This often indicates a misspelling, "+
			"missing collection, or incorrect module path.", u.Module)
	}
	return b.String()
}

// UnresolvedModules reports every task in pb whose module name neither
// this engine's registry nor its own directive set can run.
//
// ⚠ This exists because the port diverged from real in a way that can
// CHANGE A MACHINE. Real refuses the whole playbook before touching any
// host -- measured: a playbook whose SECOND task names a bad module
// leaves the first task's `touch` file uncreated, and exits 4. This port
// ran every preceding task and failed only when it reached the bad one,
// so a typo in task 5 applied tasks 1 through 4 for real. (The first
// probe for this looked for the first task's output in real's stdout and
// found it -- real's error message QUOTES the offending source lines,
// including the neighbouring one. The file witness is what settled it.)
//
// Roles need no special handling: parseRoles resolves a play's roles:
// list into synthetic block tasks at PARSE time, so the walk below
// reaches them. What it does NOT reach is a file pulled in at run time
// by include_tasks, whose target is read only when the task executes.
// Real refuses those too; this port still fails them where they run.
//
// A templated module name is left alone: `action: "{{ mod }}"` is
// resolved later, and real accepts it (measured, exit 0).
func (e *Engine) UnresolvedModules(pb Playbook) []UnresolvedModule {
	reg := e.Modules
	if reg == nil {
		reg = modules.Default()
	}
	var out []UnresolvedModule
	for _, play := range pb {
		walk := func(tasks []Task) {
			out = append(out, unresolvedIn(reg, play.Name, tasks)...)
		}
		walk(play.Tasks)
		walk(play.Handlers)
	}
	return out
}

func unresolvedIn(reg *modules.Registry, playName string, tasks []Task) []UnresolvedModule {
	var out []UnresolvedModule
	for _, t := range tasks {
		out = append(out, unresolvedIn(reg, playName, t.Block)...)
		out = append(out, unresolvedIn(reg, playName, t.Rescue)...)
		out = append(out, unresolvedIn(reg, playName, t.Always)...)

		if t.Module == "" || strings.Contains(t.Module, "{{") {
			continue
		}
		if engineDirectives[t.Module] {
			continue
		}
		if _, ok := reg.Get(t.Module); ok {
			continue
		}
		out = append(out, UnresolvedModule{Play: playName, Task: t.Name, Module: t.Module})
	}
	return out
}
