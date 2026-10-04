package playbook

import (
	"io"
	"strings"
	"testing"

	"github.com/go-ansible/modules"
)

// `exception` is in the RESULT and not in the PRINTED result, and both
// halves had to be got right for either to be useful.
//
// Measured against ansible-core 2.21.4. A failing looped fail prints
//
//	failed: [localhost] (item=a) => {"ansible_loop_var": "item", "changed": false, "item": "a", "msg": "boom a"}
//
// with no exception key -- real's _dump_results pops exception,
// warnings and deprecations unconditionally -- while `r.exception`
// registered from the same task is "(traceback unavailable)".
//
// Adding the key without the display filter changed every failure line
// in the output, which TestLoopedFailureLines caught. That test was
// right and the change was half-done: the filter is the other half.
func TestExceptionIsCarriedButNotPrinted(t *testing.T) {
	r := Result{
		Host: "localhost", Module: "fail", Failed: true, Msg: "boom a",
		Looped: true, Item: "a", LoopVar: "item",
		Extra: map[string]any{
			"exception":      "(traceback unavailable)",
			"warnings":       []any{"w"},
			"deprecations":   []any{"d"},
			"something_real": 1,
		},
	}
	c := NewDefaultCallback(io.Discard, false)
	line := c.resultJSON(r)
	for _, hidden := range []string{"exception", "warnings", "deprecations"} {
		if strings.Contains(line, hidden) {
			t.Errorf("the printed line shows %q; real's _dump_results pops it:\n  %s", hidden, line)
		}
	}
	if !strings.Contains(line, "something_real") {
		t.Errorf("the filter ate a key it should not have:\n  %s", line)
	}
	if !strings.Contains(line, `"msg": "boom a"`) {
		t.Errorf("the message is gone:\n  %s", line)
	}

	// verboseDump is the OTHER path that renders a result's keys, and
	// real applies the same pop to both.
	dump := c.verboseDump(r)
	for _, hidden := range []string{"exception", "warnings", "deprecations"} {
		if strings.Contains(dump, hidden) {
			t.Errorf("the verbose dump shows %q:\n  %s", hidden, dump)
		}
	}
}

// And the engine's own directives get the key, since they never pass
// through modules.Registry.Run. Measured: assert, include_vars,
// add_host and group_by all carry it in real when they fail.
func TestEngineDirectiveFailuresCarryException(t *testing.T) {
	res := modules.AddException(modules.Fail("no"))
	if got := res.Extra["exception"]; got != "(traceback unavailable)" {
		t.Errorf("exception = %v", got)
	}
	ok := modules.AddException(modules.Ok("fine"))
	if _, present := ok.Extra["exception"]; present {
		t.Error("a successful directive result carries an exception key")
	}
}
