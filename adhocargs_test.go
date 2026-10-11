package playbook

import "testing"

// Every expectation here was measured against real ansible-core 2.21.4
// with `ansible -i inv all -m <module> -a '<args>'`, reading the msg it
// printed (or the error it refused with).
func TestParseModuleArgs(t *testing.T) {
	for _, c := range []struct {
		name, module, args string
		want               map[string]any
		wantErr            string
	}{
		{
			name: "a plain pair", module: "debug", args: "msg=hello",
			want: map[string]any{"msg": "hello"},
		},
		{
			// The one that was silently truncated: "world" has no
			// equals sign, so it is a raw param, and debug cannot take
			// one. Real refuses; the old ad-hoc parser dropped it and
			// ran with msg="hello".
			name:   "a stray word is a raw param, and debug refuses it",
			module: "debug", args: "msg=hello world",
			wantErr: "Action 'ansible.builtin.debug' does not support raw params.",
		},
		{
			name: "a quoted value keeps its spaces", module: "debug", args: `msg="hello world"`,
			want: map[string]any{"msg": "hello world"},
		},
		{
			// A Jinja block contains spaces and must survive the split.
			name: "a jinja block is one token", module: "debug", args: "msg={{ plaintext_var }}",
			want: map[string]any{"msg": "{{ plaintext_var }}"},
		},
		{
			name: "a nested jinja block", module: "debug", args: "msg={{ a[b | int] }} tail=x",
			want: map[string]any{"msg": "{{ a[b | int] }}", "tail": "x"},
		},
		{
			name: "only the FIRST equals splits", module: "debug", args: `msg="a=b"`,
			want: map[string]any{"msg": "a=b"},
		},
		{
			name: "the JSON form", module: "debug", args: `{"msg": "a b"}`,
			want: map[string]any{"msg": "a b"},
		},
		{
			// command is in MODULE_REQUIRE_ARGS, so its k=v-looking
			// text is free-form, not options.
			name: "command takes free-form text", module: "command", args: "echo hi",
			want: map[string]any{"_raw_params": "echo hi"},
		},
		{
			// ...and the old parser special-cased only command and
			// shell, so these four were wrong too.
			name: "raw also takes free-form text", module: "raw", args: "uptime",
			want: map[string]any{"_raw_params": "uptime"},
		},
		{
			// set_fact may receive raw params (RAW_PARAM_MODULES) but is
			// NOT in MODULE_REQUIRE_ARGS, so a pair stays a pair. Taking
			// check_raw off the wrong list would make this _raw_params.
			name: "set_fact keeps its pairs", module: "set_fact", args: "a=1",
			want: map[string]any{"a": "1"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseModuleArgs(c.module, c.args)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("wanted the refusal %q, got args %v and no error", c.wantErr, got)
				}
				if err.Error() != c.wantErr {
					t.Fatalf("error:\n got %q\nwant %q", err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseModuleArgs: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("%s = %#v, want %#v", k, got[k], v)
				}
			}
		})
	}
}
