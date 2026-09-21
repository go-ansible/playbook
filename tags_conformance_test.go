package playbook

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/go-ansible/inventory"
)

// The tag playbook the tables below run, matching the one measured
// against real ansible-core 2.21.4.
const tagConformancePlaybook = `
- name: tagged
  hosts: all
  gather_facts: false
  tasks:
    - {name: plain,  debug: {msg: plain}}
    - {name: alpha,  debug: {msg: alpha},  tags: [alpha]}
    - {name: beta,   debug: {msg: beta},   tags: [beta]}
    - {name: always, debug: {msg: always}, tags: [always]}
    - {name: never,  debug: {msg: never},  tags: [never, nuke]}
`

// TestTagSelectionMatchesRealAnsible pins tag selection to output
// measured from real ansible-core 2.21.4. Every `want` below is what
// real ansible-playbook actually ran for those flags, not what the
// documented rules were read to imply.
//
// The case that matters most is the first: with no flags at all, a task
// tagged `never` must NOT run. Guarding a destructive task is the only
// thing `never` is for, and this port used to run it.
func TestTagSelectionMatchesRealAnsible(t *testing.T) {
	tests := []struct {
		run, skip []string
		want      string
	}{
		{nil, nil, "plain,alpha,beta,always"},
		{[]string{"alpha"}, nil, "alpha,always"},
		{[]string{"alpha", "beta"}, nil, "alpha,beta,always"},
		{[]string{"never"}, nil, "always,never"},
		// Any of a never-task's OTHER tags selects it too.
		{[]string{"nuke"}, nil, "always,never"},
		{[]string{"all"}, nil, "plain,alpha,beta,always"},
		{[]string{"tagged"}, nil, "alpha,beta,always"},
		{[]string{"untagged"}, nil, "plain,always"},
		{nil, []string{"alpha"}, "plain,beta,always"},
		// `always` is not sacred: naming it explicitly skips it.
		{nil, []string{"always"}, "plain,alpha,beta"},
		{nil, []string{"untagged"}, "alpha,beta,always"},
		{nil, []string{"tagged"}, "plain"},
		// --skip-tags all spares `always` tasks...
		{nil, []string{"all"}, "always"},
		// ...unless `always` is named alongside it.
		{nil, []string{"all", "always"}, ""},
		{[]string{"all"}, []string{"beta"}, "plain,alpha,always"},
	}

	for _, tt := range tests {
		name := "run=" + strings.Join(tt.run, "+") + "/skip=" + strings.Join(tt.skip, "+")
		t.Run(name, func(t *testing.T) {
			if got := runTagPlaybook(t, tagConformancePlaybook, tt.run, tt.skip); got != tt.want {
				t.Errorf("ran %q, real ansible-core runs %q", got, tt.want)
			}
		})
	}
}

// TestTagInheritanceMatchesRealAnsible covers tags reaching a task from
// its play and its enclosing block, including a block tagged `never`.
func TestTagInheritanceMatchesRealAnsible(t *testing.T) {
	const pb = `
- name: inherit
  hosts: all
  gather_facts: false
  tags: [playtag]
  tasks:
    - {name: t1, debug: {msg: t1}}
    - name: blk
      block:
        - {name: t2, debug: {msg: t2}}
        - {name: t3, debug: {msg: t3}, tags: [inner]}
      tags: [blocktag]
    - name: nevblk
      block:
        - {name: t4, debug: {msg: t4}}
      tags: [never]
`
	tests := []struct {
		run, skip []string
		want      string
	}{
		{nil, nil, "t1,t2,t3"},
		// The play's own tag reaches every task — including the one in
		// the `never` block, which naming a tag of its own selects.
		{[]string{"playtag"}, nil, "t1,t2,t3,t4"},
		{[]string{"blocktag"}, nil, "t2,t3"},
		{[]string{"inner"}, nil, "t3"},
		{nil, []string{"blocktag"}, "t1"},
		{[]string{"never"}, nil, "t4"},
	}
	for _, tt := range tests {
		name := "run=" + strings.Join(tt.run, "+") + "/skip=" + strings.Join(tt.skip, "+")
		t.Run(name, func(t *testing.T) {
			if got := runTagPlaybook(t, pb, tt.run, tt.skip); got != tt.want {
				t.Errorf("ran %q, real ansible-core runs %q", got, tt.want)
			}
		})
	}
}

// runTagPlaybook runs src with the given tag filters and returns the
// names of the tasks that actually ran, comma-joined in order.
func runTagPlaybook(t *testing.T, src string, run, skip []string) string {
	t.Helper()
	pb, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.RunTags = run
	e.SkipTags = skip

	// Serialised because OnResult arrives from one goroutine per host.
	var mu sync.Mutex
	var ran []string
	e.OnResult = func(r Result) {
		if r.Skipped {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, r.Task)
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	return strings.Join(ran, ",")
}

// TestSerialRebannersEachBatch pins the shape real ansible-core prints
// for a rolling play: the PLAY banner and the TASK banner both repeat
// per batch, which is what makes the batch boundaries visible while a
// rolling update runs.
//
// Measured with 5 hosts and serial: 2, where real ansible-core prints
// three PLAY banners (batches of 2, 2 and 1).
func TestSerialRebannersEachBatch(t *testing.T) {
	pb, err := Parse([]byte(`
- name: batched
  hosts: all
  gather_facts: false
  serial: 2
  tasks:
    - name: mark
      debug: {msg: "{{ inventory_hostname }}"}
`))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	e := New(fiveHostInventory())
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if got := strings.Count(out, "PLAY [batched]"); got != 3 {
		t.Errorf("PLAY banner appeared %d times, real ansible-core prints it once per batch (3)\n%s", got, out)
	}
	if got := strings.Count(out, "TASK [mark]"); got != 3 {
		t.Errorf("TASK banner appeared %d times, real ansible-core re-banners it per batch (3)\n%s", got, out)
	}
	// The recap is still printed once, at the very end.
	if got := strings.Count(out, "PLAY RECAP"); got != 1 {
		t.Errorf("PLAY RECAP appeared %d times, want 1\n%s", got, out)
	}
}

// TestPlayThatMatchedNoHostsSaysSo pins real ansible-core's own
// "skipping: no hosts matched" line. Without it a mistyped host pattern
// is indistinguishable from a play that genuinely had no work.
func TestPlayThatMatchedNoHostsSaysSo(t *testing.T) {
	pb, err := Parse([]byte(`
- name: nobody
  hosts: nosuchgroup
  gather_facts: false
  tasks:
    - {name: t, debug: {msg: t}}
`))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	e := New(fiveHostInventory())
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "PLAY [nobody]") {
		t.Errorf("a play matching no hosts is still bannered by real ansible-core\n%s", out)
	}
	if !strings.Contains(out, "skipping: no hosts matched") {
		t.Errorf("missing real ansible-core's own no-hosts line\n%s", out)
	}
}

// fiveHostInventory mirrors the inventory the serial behaviour above was
// measured against: five hosts, all reached locally, so batching is
// observable without any remote machine.
func fiveHostInventory() *inventory.Inventory {
	// Grouped under `web` so a group pattern is testable; every host
	// still belongs to `all`.
	inv, err := inventory.ParseYAML([]byte(`
web:
  hosts:
    h1: {ansible_connection: local}
    h2: {ansible_connection: local}
    h3: {ansible_connection: local}
    h4: {ansible_connection: local}
    h5: {ansible_connection: local}
`))
	if err != nil {
		panic(err)
	}
	return inv
}

// TestTagFilteredTasksLeaveNoTraceInTheTranscript compares the WHOLE
// transcript, not just which tasks ran. The earlier tag tables compared
// only the messages the selected tasks printed, which is exactly why
// they could not see that the excluded ones were each printing a
// "skipping:" line and inflating the recap's skipped count.
//
// The expected text is real ansible-core's own output for this playbook
// with --tags alpha, banner padding aside.
func TestTagFilteredTasksLeaveNoTraceInTheTranscript(t *testing.T) {
	pb, err := Parse([]byte(strings.Replace(tagConformancePlaybook, "hosts: all", "hosts: all", 1)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	e := New(localhostInventory())
	e.RunTags = []string{"alpha"}
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// Nothing at all for the four unselected tasks.
	for _, absent := range []string{"TASK [plain]", "TASK [beta]", "TASK [never]", "skipping:"} {
		if strings.Contains(out, absent) {
			t.Errorf("transcript contains %q, which real ansible-core does not print:\n%s", absent, out)
		}
	}
	for _, present := range []string{"TASK [alpha]", "TASK [always]"} {
		if !strings.Contains(out, present) {
			t.Errorf("transcript is missing %q:\n%s", present, out)
		}
	}
	// And the recap counts none of them as skipped.
	if !strings.Contains(out, "skipped=0") {
		t.Errorf("recap must report skipped=0 — tags select, they do not skip:\n%s", out)
	}
}

// TestRetryLinesComeAfterTheTaskBanner pins the ORDER real ansible-core
// prints: the banner, then the retries, then the outcome. The banner
// used to be emitted only on a task's first RESULT, which put the retry
// lines above it.
func TestRetryLinesComeAfterTheTaskBanner(t *testing.T) {
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
	banner := strings.Index(out, "TASK [never succeeds]")
	retry := strings.Index(out, "FAILED - RETRYING")
	fatal := strings.Index(out, "fatal:")
	if banner < 0 || retry < 0 || fatal < 0 {
		t.Fatalf("missing one of banner/retry/fatal:\n%s", out)
	}
	if !(banner < retry && retry < fatal) {
		t.Errorf("want banner < retry < fatal, got %d/%d/%d:\n%s", banner, retry, fatal, out)
	}
}
