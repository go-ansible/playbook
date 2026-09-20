package playbook

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/go-ansible/modules"
)

// diffCase is one entry of testdata/diff_cases.json, whose `expected`
// field was produced by calling real ansible-core 2.21.4's own
// CallbackBase._get_diff — not by transcribing what a diff is assumed to
// look like. testdata/gen_diff_cases.py regenerates the file.
type diffCase struct {
	Before       string `json:"before"`
	After        string `json:"after"`
	BeforeHeader string `json:"before_header"`
	AfterHeader  string `json:"after_header"`
	Expected     string `json:"expected"`
}

// TestRenderDiffMatchesRealAnsible checks this port's diff output against
// a corpus generated from real Ansible's own callback. The corpus mixes
// the two shapes measured from a live `--diff --check` run, hand-picked
// edge cases (an emptied file, a missing trailing newline, identical
// sides) and 300 randomised pairs, which is what covers the hunk-grouping
// boundaries no hand-written case thinks to hit.
func TestRenderDiffMatchesRealAnsible(t *testing.T) {
	raw, err := os.ReadFile("testdata/diff_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []diffCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	// A corpus that failed to load would otherwise pass this test by
	// comparing nothing at all.
	if len(cases) < 300 {
		t.Fatalf("corpus too small to be the real one: %d cases", len(cases))
	}
	withDiff := 0
	for _, c := range cases {
		if c.Expected != "" {
			withDiff++
		}
	}
	if withDiff < 250 {
		t.Fatalf("only %d cases expect any diff output; the corpus is not exercising the renderer", withDiff)
	}
	// difflib's autojunk only engages at 200 elements or more, so a
	// corpus of small files would leave that branch untested while
	// still passing.
	large := 0
	for _, c := range cases {
		if strings.Count(c.Before, "\n") >= 200 {
			large++
		}
	}
	if large < 20 {
		t.Fatalf("only %d cases are large enough to exercise autojunk", large)
	}

	failures := 0
	for i, c := range cases {
		got := renderDiff(modules.Diff{
			Before:       c.Before,
			After:        c.After,
			BeforeHeader: c.BeforeHeader,
			AfterHeader:  c.AfterHeader,
		}, nil)
		if got != c.Expected {
			failures++
			if failures <= 3 {
				t.Errorf("case %d: before=%q after=%q\n got:\n%s\nwant:\n%s",
					i, c.Before, c.After, got, c.Expected)
			}
		}
	}
	if failures > 3 {
		t.Errorf("%d of %d cases diverge from real Ansible (first 3 shown)", failures, len(cases))
	}
}

// The skip paths and `prepared` have no corpus entry because they are
// not reachable from a before/after pair alone.
func TestRenderDiffSkipMessages(t *testing.T) {
	tests := []struct {
		name string
		d    modules.Diff
		want string
	}{{
		name: "destination binary",
		d:    modules.Diff{DstBinary: true},
		want: "diff skipped: destination file appears to be binary\n",
	}, {
		name: "source binary",
		d:    modules.Diff{SrcBinary: true},
		want: "diff skipped: source file appears to be binary\n",
	}, {
		name: "destination too large",
		d:    modules.Diff{DstLarger: modules.MaxDiffSize},
		want: "diff skipped: destination file size is greater than 104448\n",
	}, {
		name: "source too large",
		d:    modules.Diff{SrcLarger: modules.MaxDiffSize},
		want: "diff skipped: source file size is greater than 104448\n",
	}, {
		name: "prepared text is emitted verbatim",
		d:    modules.Diff{Prepared: "whatever the module wrote\n"},
		want: "whatever the module wrote\n",
	}, {
		// A declined side still leaves the other one to show, and real
		// Ansible prints the message AND the diff rather than returning
		// early.
		name: "a skip message does not suppress the diff",
		d:    modules.Diff{SrcBinary: true, After: "now text\n"},
		want: "diff skipped: source file appears to be binary\n" +
			"--- before\n+++ after\n@@ -0,0 +1 @@\n+now text\n\n",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderDiff(tt.d, nil); got != tt.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestRenderDiffColorsEachLineKind(t *testing.T) {
	got := renderDiff(modules.Diff{
		Before: "a\nb\n", After: "a\nB\n",
		BeforeHeader: "/f (content)", AfterHeader: "/f (content)",
	}, func(code, s string) string { return "<" + code + ">" + s })

	// Real Ansible colors adds green, removes red and hunk headers cyan;
	// a context line is left alone.
	for _, want := range []string{
		"<" + colorDiffRemove + ">--- before",
		"<" + colorDiffAdd + ">+++ after",
		"<" + colorDiffLines + ">@@",
		"<" + colorDiffAdd + ">+B",
		"<" + colorDiffRemove + ">-b",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<"+colorDiffAdd+"> a\n") {
		t.Error("a context line must not be colored")
	}
}
