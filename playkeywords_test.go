package playbook

import (
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// A mistyped play keyword must be refused, not dropped. Measured against
// real ansible-core 2.21.4: `serail: 1` exits 4 there with
//
//	ERROR! 'serail' is not a valid attribute for a Play
//
// while this port used to run the play against every host at once and
// exit 0, printing nothing at all. A typo turning a rolling update into
// a simultaneous one, silently, is not something a drop-in replacement
// can do.
func TestAnUnknownPlayKeywordIsRefused(t *testing.T) {
	for _, c := range []struct{ name, key string }{
		{"a mistyped serial", "serail"},
		{"a mistyped any_errors_fatal", "any_errors_fatl"},
		{"a task keyword used on a play", "loop"},
		{"something invented", "no_such_keyword_at_all"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte("- hosts: all\n  " + c.key + ": 1\n  tasks: []\n"))
			if err == nil {
				t.Fatalf("%q was accepted on a play", c.key)
			}
			want := "\"" + c.key + "\" is not a valid attribute for a Play"
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err.Error(), want)
			}
		})
	}
}

// The CONTROL: every keyword real accepts on a Play must still parse.
// Without this the gate could be "refuse everything unusual", which
// would reject playbooks real runs -- a worse failure than the one it
// fixes.
func TestEveryRealPlayKeywordIsAccepted(t *testing.T) {
	for kw := range playKeywords {
		t.Run(kw, func(t *testing.T) {
			// A value that is plausible for any of them; the gate runs
			// before any of it is interpreted, which is what is tested.
			src := "- hosts: all\n  tasks: []\n  " + kw + ": {}\n"
			if _, err := Parse([]byte(src)); err != nil && strings.Contains(err.Error(), "not a valid attribute") {
				t.Errorf("%q is play-valid in real but refused here: %v", kw, err)
			}
		})
	}
}

// The list is REAL's, so it is checked against real rather than trusted.
// `ansible-doc -t keyword <name> --json` carries applies_to, so the set
// can be re-derived; a keyword added or removed upstream shows up here
// instead of as a playbook that mysteriously will not parse.
func TestPlayKeywordsMatchTheReference(t *testing.T) {
	bin, err := exec.LookPath("ansible-doc")
	if err != nil {
		// A skipped judge reads exactly like a passing one, and this one
		// IS the judge for the list above.
		if os.Getenv("ANSIBLE_REQUIRE") != "" {
			t.Fatalf("ANSIBLE_REQUIRE is set but ansible-doc is not installed: %v", err)
		}
		t.Skip("ansible-doc not found in PATH; skipping the check against the reference")
	}

	out, err := exec.Command(bin, "-t", "keyword", "-l", "--json").Output()
	if err != nil {
		t.Fatalf("ansible-doc -t keyword -l: %v", err)
	}
	var all map[string]any
	if err := json.Unmarshal(out, &all); err != nil {
		t.Fatalf("parsing the keyword list: %v", err)
	}
	if len(all) < 40 {
		t.Fatalf("the reference listed only %d keywords, which cannot be right: "+
			"this check is reading something other than the keyword list", len(all))
	}

	real := map[string]bool{}
	for name := range all {
		one, err := exec.Command(bin, "-t", "keyword", name, "--json").Output()
		if err != nil {
			t.Fatalf("ansible-doc -t keyword %s: %v", name, err)
		}
		var doc map[string]struct {
			AppliesTo []string `json:"applies_to"`
		}
		if err := json.Unmarshal(one, &doc); err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, a := range doc[name].AppliesTo {
			if a == "Play" {
				real[name] = true
			}
		}
	}

	var missing, extra []string
	for k := range real {
		if !playKeywords[k] {
			missing = append(missing, k)
		}
	}
	for k := range playKeywords {
		if !real[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("the reference accepts these on a Play and playKeywords does not: %v — "+
			"playbooks real runs would be refused here", missing)
	}
	if len(extra) > 0 {
		t.Errorf("playKeywords has these and the reference does not: %v — "+
			"typos real catches would be accepted here", extra)
	}
}
