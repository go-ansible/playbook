package playbook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A play's fact_path must reach the gatherer. The facts package has
// implemented it all along -- the directory's *.fact files come back as
// ansible_local -- but connectAndGatherFacts called the defaults-only
// facts.Gather and discarded the play's setting.
//
// Measured end to end against real ansible-core 2.21.4 with a
// facts.d/demo.fact holding `[general]` / `answer = 42`: real prints 42
// and so does this now, and both print the default when the keyword is
// absent.
//
// (Wiring it was not sufficient on its own: facts published the key as
// "ansible_local", which vars.InjectFacts turned into
// ansible_ansible_local. Fixed in facts v0.19.0.)
func TestFactPathReachesTheGatherer(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "demo.fact"), []byte("[general]\nanswer = 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, factPath string) string {
		t.Helper()
		src := "- hosts: all\n  gather_facts: true\n"
		if factPath != "" {
			src += "  fact_path: " + factPath + "\n"
		}
		src += "  tasks:\n    - debug: {msg: \"local=>{{ ansible_local.demo.general.answer | default('MISSING') }}<\"}\n"
		pb, err := Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		var msg string
		e := New(localhostInventory())
		e.OnResult = func(r Result) {
			if strings.Contains(r.Msg, "local=>") {
				msg = r.Msg
			}
		}
		if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
			t.Fatal(err)
		}
		return msg
	}

	t.Run("the directory the play named is read", func(t *testing.T) {
		if got := run(t, dir); got != "local=>42<" {
			t.Errorf("msg = %q, want %q", got, "local=>42<")
		}
	})

	// The CONTROL: without the keyword the default path is used, which
	// holds nothing here. Without it, "42" could come from reading the
	// directory unconditionally rather than because the play asked.
	t.Run("without the keyword, the default path is used", func(t *testing.T) {
		if got := run(t, ""); got != "local=>MISSING<" {
			t.Errorf("msg = %q, want %q — fact_path was read even though the play did not set it", got, "local=>MISSING<")
		}
	})
}
