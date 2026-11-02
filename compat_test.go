package playbook

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-ansible/modules"
)

const modernPlaybook = `
- hosts: all
  gather_facts: no
  become: true
  become_user: root
  tasks:
    - name: a task
      debug:
        msg: ok
      when: true
`

const oldPlaybook = `
- hosts: all
  gather_facts: no
  sudo: yes
  sudo_user: root
  tasks:
    - name: a task
      debug:
        msg: ok
      always_run: yes
`

func findingFor(t *testing.T, fs []CompatFinding, name string) CompatFinding {
	t.Helper()
	for _, f := range fs {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no finding for %q; got %v", name, fs)
	return CompatFinding{}
}

func TestCompatScanReportsRetiredKeywordsAtBothLevels(t *testing.T) {
	fs, err := CompatScan([]byte(oldPlaybook))
	if err != nil {
		t.Fatalf("CompatScan: %v", err)
	}

	sudo := findingFor(t, fs, "sudo")
	if sudo.Kind != "play keyword" {
		t.Errorf("sudo: kind = %q, want \"play keyword\"", sudo.Kind)
	}
	if sudo.Line != 4 {
		t.Errorf("sudo: line = %d, want 4", sudo.Line)
	}
	if !strings.Contains(sudo.Advice, "become") {
		t.Errorf("sudo: advice %q does not name become", sudo.Advice)
	}

	ar := findingFor(t, fs, "always_run")
	if ar.Kind != "task keyword" {
		t.Errorf("always_run: kind = %q, want \"task keyword\"", ar.Kind)
	}
	if !strings.Contains(ar.Advice, "check_mode") {
		t.Errorf("always_run: advice %q does not name check_mode", ar.Advice)
	}

	findingFor(t, fs, "sudo_user")
}

// TestCompatScanWorksOnSourceThatDoesNotParse is the promise the whole
// feature rests on. The playbook a user is stuck with is one that FAILS
// to parse — that is why they keep an old virtualenv — so a report that
// needed a parsed playbook would be silent in the only case it is for.
//
// The first half is the control: without it, a scan that happened to run
// on a source this package parses fine would look like it had passed.
func TestCompatScanWorksOnSourceThatDoesNotParse(t *testing.T) {
	if _, err := Parse([]byte(oldPlaybook)); err == nil {
		t.Fatal("control failed: this package now PARSES the old playbook, " +
			"so this test no longer measures a scan of unparseable source")
	}
	fs, err := CompatScan([]byte(oldPlaybook))
	if err != nil {
		t.Fatalf("CompatScan on unparseable source: %v", err)
	}
	if len(fs) != 3 {
		t.Fatalf("got %d findings, want 3 (sudo, sudo_user, always_run): %v", len(fs), fs)
	}
}

func TestCompatScanIsSilentOnAModernPlaybook(t *testing.T) {
	fs, err := CompatScan([]byte(modernPlaybook))
	if err != nil {
		t.Fatalf("CompatScan: %v", err)
	}
	if len(fs) != 0 {
		t.Errorf("a playbook using only current keywords produced %d findings: %v", len(fs), fs)
	}
}

func TestCompatScanLocatesAnUnresolvedModule(t *testing.T) {
	const src = `
- hosts: all
  gather_facts: no
  tasks:
    - name: a task
      no_such_module_anywhere:
        a: 1
`
	fs, err := CompatScan([]byte(src))
	if err != nil {
		t.Fatalf("CompatScan: %v", err)
	}
	f := findingFor(t, fs, "no_such_module_anywhere")
	if f.Kind != "module" {
		t.Errorf("kind = %q, want \"module\"", f.Kind)
	}
	if f.Line != 6 {
		t.Errorf("line = %d, want 6", f.Line)
	}
}

// TestFactsRenameAdviceNamesARegisteredModule holds the one piece of
// advice that names a replacement module to its promise: the module it
// names must actually be registered here. Advice pointing at a module
// this port does not have would send a user from one dead end to
// another.
func TestFactsRenameAdviceNamesARegisteredModule(t *testing.T) {
	reg := modules.Default()
	checked := 0
	for _, name := range reg.Names() {
		base, ok := strings.CutSuffix(name, "_info")
		if !ok {
			continue
		}
		checked++
		advice := moduleAdvice(base + "_facts")
		if !strings.Contains(advice, "`"+name+"`") {
			t.Errorf("moduleAdvice(%q) = %q; want it to name %q", base+"_facts", advice, name)
		}
	}
	// The control: a registry that returned nothing would pass the loop
	// above without checking anything at all.
	if checked < 20 {
		t.Fatalf("control failed: only %d *_info modules found in the registry; "+
			"the rename rule was not exercised", checked)
	}
}

func TestModuleAdviceDoesNotPromiseAFlagItCannotKeep(t *testing.T) {
	advice := moduleAdvice("a_name_that_is_not_a_module")
	if strings.Contains(advice, "_info") {
		t.Errorf("advice for an unknown name leaked the rename rule: %q", advice)
	}
	if !strings.Contains(advice, "coverage") {
		t.Errorf("advice %q should say plainly that this is missing coverage", advice)
	}
}

// nestedBlocks builds a playbook whose tasks are `block:` nested depth
// levels deep, in FLOW style. Block style would indent, making the bomb
// quadratic in bytes — measured, 20 000 levels is 1.6 GB of mostly
// spaces — so the file, not the parser, would become the limit. Flow
// style costs ten bytes a level, which is what an attacker would send.
func nestedBlocks(depth int) []byte {
	var b strings.Builder
	b.Grow(12*depth + 64)
	b.WriteString("- {hosts: all, gather_facts: no, tasks: ")
	for i := 0; i < depth; i++ {
		b.WriteString("[{block: ")
	}
	b.WriteString("[{debug: {msg: ok}}]")
	for i := 0; i < depth; i++ {
		b.WriteString("}]")
	}
	b.WriteString("}\n")
	return []byte(b.String())
}

// TestNestingBombIsRefused pins a bound this package does not own.
//
// CompatScan walks the task tree recursively, and so does parse; neither
// carries a depth limit of its own. They are safe only because
// gopkg.in/yaml.v3 refuses a document nested deeper than 10 000 before
// either walk ever runs — measured: 200 000 levels in 2.2 MB of source
// is refused with "exceeded max depth of 10000" after 6.4 MB of
// allocation, where the reference, ansible-core 2.21.4, refuses the same
// file with "YAML parsing failed: Stack overflow (used 16352 kB)" and
// exit 4.
//
// Borrowed strictness is the thing that breaks quietly on an upgrade: a
// YAML library that raised or dropped that cap would hand this package
// an unbounded recursion and no test would notice. This one would.
//
// The allocation ceiling is part of the assertion on purpose. Without
// it, a library that "refused" only after building the whole tree would
// still pass — a refusal that costs a gigabyte is not a refusal.
func TestNestingBombIsRefused(t *testing.T) {
	src := nestedBlocks(200000)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := CompatScan(src)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("a playbook nested 200 000 blocks deep was ACCEPTED; " +
			"the depth bound this package relies on is gone, and both " +
			"CompatScan and parse now recurse on attacker-chosen depth")
	}
	if !strings.Contains(err.Error(), "max depth") {
		t.Errorf("refused, but not for depth: %v\n"+
			"If the YAML library's depth cap was replaced by something else, "+
			"say so here rather than loosening the check.", err)
	}

	// Allocation counts are exact; wall times are not, so none is used.
	const ceiling = 64 << 20
	if used := after.TotalAlloc - before.TotalAlloc; used > ceiling {
		t.Errorf("refusing the bomb allocated %d bytes, over the %d ceiling", used, ceiling)
	}
}

// TestJustUnderTheBombDepthStillWalks is the other half of the bound: a
// document the YAML library ACCEPTS must be one these walks survive. A
// test that only proved the deep case is refused would say nothing about
// the deepest case that still gets through.
//
// 4 000 blocks, not 9 000: the cap counts YAML structural depth, and one
// nested block costs TWO levels of it — a sequence and the mapping
// inside it — so the 10 000 cap is reached at about 5 000 blocks.
// (Measured: 9 000 blocks is already refused.) The number here is a
// block count, and halving is the part that is easy to get wrong.
func TestJustUnderTheBombDepthStillWalks(t *testing.T) {
	fs, err := CompatScan(nestedBlocks(4000))
	if err != nil {
		t.Fatalf("4 000 blocks is under the YAML library's cap and must decode: %v", err)
	}
	if len(fs) != 0 {
		t.Errorf("nothing in this fixture is retired, got %d findings: %v", len(fs), fs)
	}
}

// TestRetiredKeywordsAreRefusedByReference is where the table's entries
// come from. Each keyword is written into a real playbook, at play level
// and at task level, and run through the installed reference: the
// reference must refuse it (exit 4, a parse error), or the table is
// claiming a removal that did not happen.
//
// It carries its own control. `become` at play level and `when` at task
// level must be ACCEPTED, so a reference that is broken, misconfigured
// or pointed at the wrong binary cannot report every keyword as retired
// and turn a failure into a table of imaginary removals.
func TestRetiredKeywordsAreRefusedByReference(t *testing.T) {
	bin := ansiblePlaybookBin(t)
	dir := t.TempDir()

	const (
		refused  = 4 // real's exit status for a parse error
		playKW   = "play"
		taskKW   = "task"
		controlV = "no"
	)

	run := func(level, name, value string) (int, string) {
		t.Helper()
		var b strings.Builder
		b.WriteString("- hosts: localhost\n  gather_facts: no\n")
		if level == playKW {
			fmt.Fprintf(&b, "  %s: %s\n", name, value)
		}
		b.WriteString("  tasks:\n    - debug:\n        msg: ok\n")
		if level == taskKW {
			fmt.Fprintf(&b, "      %s: %s\n", name, value)
		}
		path := filepath.Join(dir, name+"-"+level+".yml")
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("writing fixture: %v", err)
		}
		cmd := exec.Command(bin, "-i", "localhost,", "-c", "local", path)
		cmd.Env = append(os.Environ(), "ANSIBLE_LOCALHOST_WARNING=False")
		out, _ := cmd.CombinedOutput()
		return cmd.ProcessState.ExitCode(), string(out)
	}

	// The control first: a failure here invalidates everything below, so
	// it must not be reported as one more line in a long table.
	if code, out := run(playKW, "become", controlV); code == refused {
		t.Fatalf("control failed: the reference refused `become` at play level "+
			"(exit %d). It cannot distinguish a retired keyword from its own "+
			"breakage; no verdict below is evidence.\n%s", code, out)
	}
	if code, out := run(taskKW, "when", "true"); code == refused {
		t.Fatalf("control failed: the reference refused `when` on a task "+
			"(exit %d); no verdict below is evidence.\n%s", code, out)
	}

	// A plausible value per keyword: one refused for its NAME must still
	// be given a value of a sane shape, or a type error would pass for a
	// removal.
	values := map[string]string{
		"sudo": "no", "sudo_user": "root", "su": "no", "su_user": "root",
		"always_run": "yes", "first_available_file": "[]",
		"accelerate": "no", "accelerate_port": "5099",
		"squash_actions": "[]", "include": "nothing.yml",
	}
	for name := range retiredKeywords {
		value, ok := values[name]
		if !ok {
			t.Errorf("%s: no probe value — a keyword cannot enter the table "+
				"without being measured", name)
			continue
		}
		for _, level := range []string{playKW, taskKW} {
			if code, out := run(level, name, value); code != refused {
				t.Errorf("%s at %s level: the reference ACCEPTED it (exit %d); "+
					"it does not belong in retiredKeywords.\n%s", name, level, code, out)
			}
		}
	}
}
