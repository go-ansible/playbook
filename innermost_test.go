package playbook

import (
	"errors"
	"testing"
)

// TestInnermostBoundaries pins the two boundaries this port cuts a
// template error on. Both messages below are what gonja actually
// produced for the expression named beside them; the "want" is what
// ansible-core 2.21.4 prints for the same playbook, measured.
func TestInnermostBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			// lookup('vars', 7)
			name: "a plugin failure keeps the plugin's name",
			in:   `template: evaluating expression "query('vars', 7)": invalid call to function 'query': The lookup plugin 'vars' failed: Variable name must be 'str' not 'int'.`,
			want: `The lookup plugin 'vars' failed: Variable name must be 'str' not 'int'.`,
		},
		{
			// lookup('vars', 'nope_at_all')
			name: "an undefined value names no plugin",
			in:   `template: evaluating expression "query('vars', 'nope_at_all')": invalid call to function 'query': No variable named 'nope_at_all' was found.`,
			want: `No variable named 'nope_at_all' was found.`,
		},
		{
			// {{ nope }}
			name: "an undefined name keeps real's own wording",
			in:   `template: evaluating expression "nope": 'nope' is undefined`,
			want: `'nope' is undefined`,
		},
		{
			// no_such_name | mandatory
			name: "a filter failure keeps the filter's name",
			in:   `template: evaluating expression "no_such_name | mandatory": unable to evaluate filter &{<Token[Name] Val='mandatory' Pos=18 Line=1 Col=19> mandatory [] map[]}: invalid call to filter 'mandatory': The filter plugin 'ansible.builtin.mandatory' failed: Mandatory variable 'no_such_name' not defined.`,
			want: `The filter plugin 'ansible.builtin.mandatory' failed: Mandatory variable 'no_such_name' not defined.`,
		},
		{
			name: "anything else is left alone",
			in:   "something this port has no boundary for",
			want: "something this port has no boundary for",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := innermost(errors.New(tc.in)).Error(); got != tc.want {
				t.Errorf("innermost:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}
