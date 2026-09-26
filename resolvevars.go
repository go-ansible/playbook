package playbook

import "strings"

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
	if e.Template == nil || len(m) == 0 {
		return m
	}
	out := m
	copied := false
	for pass := 0; pass < maxVarResolutionPasses; pass++ {
		changed := false
		for k, v := range out {
			nv, ch := e.resolveValue(v, out)
			if !ch {
				continue
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
