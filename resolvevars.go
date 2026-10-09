package playbook

import (
	"regexp"
	"strings"

	"github.com/go-ansible/template"
	"github.com/go-ansible/vars"
)

// maxVarResolutionPasses bounds the walk below. Two variables that
// cite each other never settle, and real reports a recursion error
// there; this stops and hands back what it has, which keeps a
// playbook with one silly variable running rather than failing it.
const maxVarResolutionPasses = 10

// resolved renders any variable whose VALUE is itself a template.
//
// Real resolves a variable's value when it is REFERENCED rather than
// when it is defined, so
//
//	vars:
//	  a: hello
//	  b: "{{ a }}-world"
//
// reads back as "hello-world". This port handed back the raw text --
// for play vars, for group_vars, and inside nested lists and dicts --
// so `{{ b }}` rendered as "{{ a }}-world", and a file task given
// "{{ playbook_dir }}/sub" created a directory named literally
// "{{ playbook_dir }}".
//
// Resolution is eager and repeated until the set settles, which is
// close enough to real's laziness because the set is rebuilt for every
// task: a variable citing a fact set by an earlier task sees it by the
// time the later task renders.
//
// A value that cannot be resolved is LEFT AS IT IS rather than failing.
// Real only fails on a variable that is actually referenced, and
// failing here would break a playbook that merely DEFINES one it never
// uses -- which is why this cannot simply propagate the error.
func (e *Engine) resolved(m map[string]any) map[string]any {
	return e.resolvedExcept(m, nil)
}

// resolvedFor is resolved() with the brake on: the variables that came
// from a module result or a fact are DATA and are never re-rendered.
//
// ⛔ SECURITY. Without this, a value a MANAGED HOST chose is evaluated as
// a template on the CONTROL NODE. Measured against ansible-core 2.21.4
// with a command whose output is the text `{{ 7*7 }}`:
//
//	real:  r.stdout == "{{ 7*7 }}"   (literal, still a string)
//	ours:  r.stdout == 49            (evaluated, now a number)
//
// and with `{{ lookup('pipe','touch FILE') }}` as that output, this port
// CREATED THAT FILE ON THE CONTROLLER -- where the vault password, the
// fleet's SSH keys and the cloud credentials live. Real refuses because
// it wraps such data in AnsibleUnsafeText and its templar will not
// template those; this is the same brake, applied where the repeated
// resolution actually happens.
func (e *Engine) resolvedFor(vc *vars.Context) map[string]any {
	return e.resolvedExcept(vc.Merged(), untrustedKeys(vc))
}

// untrustedKeys names the variables that came from a module result, a
// register:, a set_fact or a gathered fact.
func untrustedKeys(vc *vars.Context) map[string]bool {
	if vc == nil {
		return nil
	}
	out := map[string]bool{
		// hostvars and groups are DERIVED STRUCTURES, never a place an
		// author writes a template -- and hostvars carries every host's
		// registered results and facts, so leaving it resolvable put
		// all of that untrusted data back under a trusted key. The
		// proof of concept kept firing through exactly this route after
		// the layers themselves were covered.
		"hostvars": true,
		"groups":   true,
	}
	for _, l := range []vars.Layer{vars.Facts, vars.Registered} {
		for k := range vc.Layer(l) {
			out[k] = true
		}
	}
	return out
}

func (e *Engine) resolvedExcept(m map[string]any, untrusted map[string]bool) map[string]any {
	if e.Template == nil || len(m) == 0 {
		return m
	}
	out := m
	copied := false
	// A key whose template REFERENCES untrusted data is rendered once
	// and then left alone, which closes the indirect route: a play var
	// `x: "{{ r.stdout }}"` renders to the literal text of a module's
	// output, and a later pass would otherwise render THAT.
	//
	// It keys on the reference rather than on "the result still looks
	// like a template", which was the first attempt and was wrong: a
	// perfectly ordinary chain (`c: "{{ b }}/c"`, `d: "{{ c }}/d"`)
	// produces a result that is still a template simply because its own
	// inputs have not been resolved yet, and that version stopped
	// resolving it -- TestResolvedFollowsAChain said so immediately.
	// Provenance is the distinction real makes too.
	var produced map[string]bool
	for pass := 0; pass < maxVarResolutionPasses; pass++ {
		changed := false
		for k, v := range out {
			if untrusted[k] || produced[k] {
				continue
			}
			tainted := referencesUntrusted(v, untrusted)
			nv, ch := e.resolveValue(v, out)
			if !ch {
				continue
			}
			if tainted {
				if produced == nil {
					produced = map[string]bool{}
				}
				produced[k] = true
			}
			if !copied {
				// Copy on first write: the common case is a variable
				// set with no templates in it at all, and copying that
				// for every task of every host would cost more than
				// this whole feature is worth.
				dup := make(map[string]any, len(out))
				for dk, dv := range out {
					dup[dk] = dv
				}
				out, copied = dup, true
			}
			out[k] = nv
			changed = true
		}
		if !changed {
			break
		}
	}
	return out
}

// resolveValue renders one value against data, reporting whether it
// changed. It walks maps and lists because a template inside one is
// just as common as a bare string: `lst: ["{{ a }}", plain]`.
func (e *Engine) resolveValue(v any, data map[string]any) (any, bool) {
	switch t := v.(type) {
	case string:
		if !mightBeTemplate(t) {
			return v, false
		}
		// RenderValue, not Render: a variable holding "{{ 1 + 1 }}"
		// resolves to the NUMBER 2, and rendering to text would make
		// every such variable a string.
		nv, err := e.Template.RenderValue(t, data)
		if err != nil {
			return v, false
		}
		if s, ok := nv.(string); ok && s == t {
			return v, false
		}
		return nv, true
	case map[string]any:
		var dup map[string]any
		for k, mv := range t {
			nv, ch := e.resolveValue(mv, data)
			if !ch {
				continue
			}
			if dup == nil {
				dup = make(map[string]any, len(t))
				for dk, dv := range t {
					dup[dk] = dv
				}
			}
			dup[k] = nv
		}
		if dup == nil {
			return v, false
		}
		return dup, true
	case []any:
		var dup []any
		for i, lv := range t {
			nv, ch := e.resolveValue(lv, data)
			if !ch {
				continue
			}
			if dup == nil {
				dup = append(dup, t...)
			}
			dup[i] = nv
		}
		if dup == nil {
			return v, false
		}
		return dup, true
	}
	return v, false
}

// mightBeTemplate is the cheap check that keeps this affordable: the
// merged variable set carries hostvars and groups for every host, and
// walking all of it per task is only tolerable because the vast
// majority of values are dismissed by two string scans.
func mightBeTemplate(s string) bool {
	return strings.Contains(s, "{{") || strings.Contains(s, "{%")
}

// identifier matches a bare name in a template expression.
var identifier = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// lookupCall matches a call to one of the lookup globals. A lookup
// RETURNS untrusted data -- a file's contents, a command's output, an
// API's answer -- so a template that calls one produces a value that
// must not be rendered again.
//
// ⛔ This is a SECOND source, distinct from the untrusted variable
// layers, and the brake that keys on untrusted NAMES cannot see it:
//
//	vars:
//	  from_file: "{{ lookup('file', 'data.txt') }}"
//
// names nothing untrusted, yet pulls a file's contents into a TRUSTED
// variable. Measured against ansible-core 2.21.4 with a data.txt holding
// `{{ lookup('pipe','touch FILE') }}`:
//
//	real:  GOT={{ lookup('pipe','touch FILE') }}   and no file
//	ours:  GOT=                                    and THE FILE EXISTED
//
// Real is safe because it marks a lookup's result unsafe; this is the
// same rule, at the only place a value is rendered twice.
var lookupCall = regexp.MustCompile(`\b(lookup|query|q)\s*\(`)

// referencesUntrusted reports whether v is (or contains) a template that
// names a variable holding untrusted data, or calls a lookup.
//
// It scans the identifiers in the template text rather than testing each
// untrusted name against it: there can be hundreds of ansible_* facts,
// and one pass over the text with a map lookup per word is cheap where
// hundreds of substring searches would not be.
func referencesUntrusted(v any, untrusted map[string]bool) bool {
	switch t := v.(type) {
	case string:
		if !template.IsTemplate(t) {
			return false
		}
		if lookupCall.MatchString(t) {
			return true
		}
		for _, word := range identifier.FindAllString(t, -1) {
			if untrusted[word] {
				return true
			}
		}
		return false
	case map[string]any:
		for _, mv := range t {
			if referencesUntrusted(mv, untrusted) {
				return true
			}
		}
	case []any:
		for _, lv := range t {
			if referencesUntrusted(lv, untrusted) {
				return true
			}
		}
	}
	return false
}
