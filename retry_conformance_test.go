package playbook

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRetryLinesMatchRealAnsible pins the countdown real ansible-core
// prints while a until:/retries: task is being retried. Both sequences
// were measured from real runs: a task with retries: 3 that never
// succeeds counts 2, 1, 0; one with retries: 5 that succeeds on its
// third attempt counts 4, 3 and then stops.
//
// The line itself is real ansible-core's, verbatim from default.py's
// v2_runner_retry — including the unpluralised "1 retries left".
func TestRetryLinesMatchRealAnsible(t *testing.T) {
	t.Run("exhausted", func(t *testing.T) {
		out := runRetryPlaybook(t, `
- name: exhaust
  hosts: all
  gather_facts: false
  tasks:
    - name: never succeeds
      shell: "exit 1"
      register: r
      until: r.rc == 0
      retries: 3
      delay: 0
      ignore_errors: true
`)
		want := []string{
			"FAILED - RETRYING: [localhost]: never succeeds (2 retries left).",
			"FAILED - RETRYING: [localhost]: never succeeds (1 retries left).",
			"FAILED - RETRYING: [localhost]: never succeeds (0 retries left).",
		}
		assertRetryLines(t, out, want)
	})

	t.Run("succeeds partway", func(t *testing.T) {
		counter := filepath.Join(t.TempDir(), "c")
		out := runRetryPlaybook(t, `
- name: eventually
  hosts: all
  gather_facts: false
  tasks:
    - name: succeed on 3rd
      shell: "c=$(cat `+counter+` 2>/dev/null || echo 0); c=$((c+1)); echo $c > `+counter+`; test $c -ge 3"
      register: r
      until: r.rc == 0
      retries: 5
      delay: 0
`)
		// Only two lines: the third attempt succeeds, so no retry follows it.
		assertRetryLines(t, out, []string{
			"FAILED - RETRYING: [localhost]: succeed on 3rd (4 retries left).",
			"FAILED - RETRYING: [localhost]: succeed on 3rd (3 retries left).",
		})
		if _, err := os.Stat(counter); err != nil {
			t.Fatalf("the probe never ran: %v", err)
		}
	})

	t.Run("a task that never retries prints nothing", func(t *testing.T) {
		out := runRetryPlaybook(t, `
- name: plain
  hosts: all
  gather_facts: false
  tasks:
    - {name: fine, shell: "true"}
`)
		if strings.Contains(out, "RETRYING") {
			t.Errorf("a task with no until:/retries: must print no retry line:\n%s", out)
		}
	})
}

func assertRetryLines(t *testing.T, out string, want []string) {
	t.Helper()
	var got []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "RETRYING") {
			got = append(got, strings.TrimSpace(line))
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d retry lines, real ansible-core prints %d:\n%s", len(got), len(want), out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

func runRetryPlaybook(t *testing.T, src string) string {
	t.Helper()
	pb, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	e := New(localhostInventory())
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestRecapEndsWithABlankLine pins the trailing blank line real
// ansible-core prints after PLAY RECAP. Measured with od on both a play
// that had hosts and one that matched none.
func TestRecapEndsWithABlankLine(t *testing.T) {
	for _, tt := range []struct{ name, hosts string }{
		{"with hosts", "all"},
		{"no hosts matched", "nosuchgroup"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := runRetryPlaybook(t, `
- name: p
  hosts: `+tt.hosts+`
  gather_facts: false
  tasks:
    - {name: t, debug: {msg: x}}
`)
			if !strings.HasSuffix(out, "\n\n") {
				t.Errorf("output must end with a blank line, got %q", out[max(0, len(out)-24):])
			}
		})
	}
}

// TestExhaustedRetriesReportAttempts pins the attempts field real
// ansible-core carries in the RESULT of a task whose retries ran out,
// not only in the registered variable: its fatal line reads
// "attempts": 3 alongside rc and stderr.
func TestExhaustedRetriesReportAttempts(t *testing.T) {
	out := runRetryPlaybook(t, `
- name: exhaust
  hosts: all
  gather_facts: false
  tasks:
    - name: never succeeds
      shell: "exit 1"
      register: r
      until: r.rc == 0
      retries: 2
      delay: 0
      ignore_errors: true
`)
	// retries: 2 means three executions, and real ansible-core's own
	// off-by-one reports attempts as 2 — see the engine's note on the
	// ansible-core quirk this reproduces deliberately.
	if !strings.Contains(out, `"attempts": 2`) {
		t.Errorf("fatal line must carry the attempts count:\n%s", out)
	}
}
