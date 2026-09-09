package playbook

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	remoteexec "github.com/go-remoteexec/transport"
)

// recordingCallback embeds BaseCallback and overrides only what it
// cares about — the way a real callback plugin subclasses CallbackBase
// and defines only the v2_* hooks it needs.
type recordingCallback struct {
	BaseCallback

	mu      sync.Mutex
	plays   []string
	results []Result
	stats   int
}

func (c *recordingCallback) OnPlayStart(play Play) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plays = append(c.plays, play.Name)
}

func (c *recordingCallback) OnTaskResult(r Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results = append(c.results, r)
}

func (c *recordingCallback) OnStats(*RunResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats++
}

func (c *recordingCallback) tasks() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.results))
	for _, r := range c.results {
		out = append(out, r.Task)
	}
	return out
}

// statsOnlyCallback overrides nothing but OnStats, proving BaseCallback
// really does satisfy the rest of the interface on its own.
type statsOnlyCallback struct {
	BaseCallback
	seen int
}

func (c *statsOnlyCallback) OnStats(*RunResult) { c.seen++ }

func TestDefaultCallbackOutput(t *testing.T) {
	var buf bytes.Buffer
	cb := NewDefaultCallback(&buf, false)

	cb.OnPlayStart(Play{Name: "deploy"})
	cb.OnTaskResult(Result{Host: "web1", Task: "install"})
	cb.OnTaskResult(Result{Host: "web2", Task: "install", Changed: true})
	cb.OnTaskResult(Result{Host: "web1", Task: "configure", Skipped: true})
	cb.OnTaskResult(Result{Host: "web2", Task: "configure", Failed: true, Msg: "boom"})
	cb.OnStats(&RunResult{Plays: []PlayResult{{Results: []Result{
		{Host: "web1"},
		{Host: "web2", Changed: true},
		{Host: "web1", Skipped: true},
		{Host: "web2", Failed: true},
		{Host: "web3", Changed: true},
	}}}})

	want := "\nPLAY [deploy]\n" +
		"\nTASK [install]\n" +
		"ok: [web1]\n" +
		"changed: [web2]\n" +
		"\nTASK [configure]\n" +
		"skipping: [web1]\n" +
		"failed: [web2] => boom\n" +
		"\nPLAY RECAP\n" +
		"web1                     : ok=1    changed=0    failed=0    skipped=1   \n" +
		"web2                     : ok=1    changed=1    failed=1    skipped=0   \n" +
		"web3                     : ok=1    changed=1    failed=0    skipped=0   \n"
	if got := buf.String(); got != want {
		t.Errorf("output =\n%q\nwant\n%q", got, want)
	}
}

func TestDefaultCallbackUnnamedPlay(t *testing.T) {
	var buf bytes.Buffer
	// Real Ansible prints a bare "PLAY" banner for a play with no name.
	NewDefaultCallback(&buf, false).OnPlayStart(Play{Name: "  "})
	if got := buf.String(); got != "\nPLAY\n" {
		t.Errorf("unnamed play banner = %q, want %q", got, "\nPLAY\n")
	}
}

func TestDefaultCallbackColor(t *testing.T) {
	var buf bytes.Buffer
	cb := NewDefaultCallback(&buf, true)
	cb.OnTaskResult(Result{Host: "web1", Task: "install", Changed: true})

	want := "\n\033[0;36mTASK [install]\033[0m\n\033[0;33mchanged: [web1]\033[0m\n"
	if got := buf.String(); got != want {
		t.Errorf("colored output = %q, want %q", got, want)
	}
}

// TestDefaultCallbackRebannersPerPlay covers the one place this
// callback deliberately differs from the CLI printer it replaces: a
// second play whose first task shares the previous play's last task
// name still gets its own TASK banner, because a play boundary resets
// the tracking.
func TestDefaultCallbackRebannersPerPlay(t *testing.T) {
	var buf bytes.Buffer
	cb := NewDefaultCallback(&buf, false)
	cb.OnPlayStart(Play{Name: "first"})
	cb.OnTaskResult(Result{Host: "h", Task: "shared"})
	cb.OnPlayStart(Play{Name: "second"})
	cb.OnTaskResult(Result{Host: "h", Task: "shared"})

	if n := strings.Count(buf.String(), "TASK [shared]"); n != 2 {
		t.Errorf("TASK banners = %d, want 2 (one per play)", n)
	}
}

// TestDefaultCallbackConcurrentResults exists for the race detector:
// results really do arrive from one goroutine per host, so the callback
// has to serialize itself rather than rely on the engine to do it.
func TestDefaultCallbackConcurrentResults(t *testing.T) {
	var buf bytes.Buffer
	cb := NewDefaultCallback(&buf, false)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cb.OnTaskResult(Result{Host: string(rune('a' + i)), Task: "t"})
		}(i)
	}
	wg.Wait()

	// Every line is whole: one banner plus one line per result, nothing
	// spliced together mid-write.
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 10 { // leading blank, banner, 8 results
		t.Fatalf("lines = %d (%q), want 10", len(lines), buf.String())
	}
	for _, line := range lines[2:] {
		if !strings.HasPrefix(line, "ok: [") || !strings.HasSuffix(line, "]") {
			t.Errorf("interleaved line %q", line)
		}
	}
}

func TestEngineRaisesCallbackHooks(t *testing.T) {
	pb, err := Parse([]byte(`
- name: say something
  hosts: all
  gather_facts: false
  tasks:
    - name: speak
      debug:
        msg: hi
`))
	if err != nil {
		t.Fatal(err)
	}

	cb := &recordingCallback{}
	stats := &statsOnlyCallback{}
	e := New(localhostInventory())
	e.Callbacks = []Callback{cb, stats}

	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}

	if len(cb.plays) != 1 || cb.plays[0] != "say something" {
		t.Errorf("plays = %#v, want one named play", cb.plays)
	}
	if got := cb.tasks(); len(got) != 1 || got[0] != "speak" {
		t.Errorf("results = %#v, want one for the speak task", got)
	}
	if cb.stats != 1 {
		t.Errorf("OnStats calls = %d, want 1", cb.stats)
	}
	// Every installed callback is raised, not just the first.
	if stats.seen != 1 {
		t.Errorf("second callback's OnStats calls = %d, want 1", stats.seen)
	}
}

// TestCallbackSeesConnectAndFactsResults covers the three results that
// used to be recorded straight onto the play, bypassing the reporting
// chokepoint entirely — so a live reporter never saw a host fail to
// connect, and never saw fact gathering happen at all.
func TestCallbackSeesConnectAndFactsResults(t *testing.T) {
	pb, err := Parse([]byte(`
- name: gather
  hosts: all
  tasks:
    - name: speak
      debug:
        msg: hi
`))
	if err != nil {
		t.Fatal(err)
	}

	cb := &recordingCallback{}
	e := New(localhostInventory())
	e.Callbacks = []Callback{cb}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if got := cb.tasks(); len(got) != 2 || got[0] != "(gather_facts)" || got[1] != "speak" {
		t.Fatalf("results = %#v, want the gather_facts result followed by the task's", got)
	}

	// And the connect-failure result, from the same previously-bypassing
	// block.
	cb = &recordingCallback{}
	e = New(localhostInventory())
	e.Callbacks = []Callback{cb}
	e.Connect = func(context.Context, string, map[string]any) (remoteexec.Connection, error) {
		return nil, errors.New("no route to host")
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	got := cb.tasks()
	if len(got) != 1 || got[0] != "(connect)" {
		t.Fatalf("results = %#v, want just the connect failure", got)
	}
	if !cb.results[0].Failed || !strings.Contains(cb.results[0].Msg, "no route to host") {
		t.Errorf("connect result = %#v, want a failure carrying the dial error", cb.results[0])
	}
}

// TestCallbackStatsRaisedOnError pins that a run cut short by an error
// still gets its recap hook, covering whatever did run.
func TestCallbackStatsRaisedOnError(t *testing.T) {
	pb, err := Parse([]byte(`
- name: ask first
  hosts: all
  gather_facts: false
  vars_prompt:
    - name: answer
  tasks:
    - name: speak
      debug:
        msg: hi
`))
	if err != nil {
		t.Fatal(err)
	}

	cb := &recordingCallback{}
	e := New(localhostInventory())
	e.Callbacks = []Callback{cb}
	e.Prompt = func(string, bool) (string, error) { return "", errors.New("stdin closed") }
	if _, err := e.RunPlaybook(context.Background(), pb); err == nil {
		t.Fatal("expected the prompt failure to abort the run")
	}
	if cb.stats != 1 {
		t.Errorf("OnStats calls after an error = %d, want 1", cb.stats)
	}
}

// TestAssertEvaluatesJinjaConditions covers the form every real playbook
// uses and this engine used to reject: `that` holding expression source
// rather than pre-computed booleans. The modules package documented that
// the engine would evaluate these, and nothing did.
func TestAssertEvaluatesJinjaConditions(t *testing.T) {
	run := func(t *testing.T, body string) *RunResult {
		t.Helper()
		pb, err := Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		e := New(localhostInventory())
		rr, err := e.RunPlaybook(context.Background(), pb)
		if err != nil {
			t.Fatal(err)
		}
		return rr
	}

	passing := run(t, `
- hosts: all
  gather_facts: false
  vars:
    n: 5
    word: hello
  tasks:
    - assert:
        that:
          - n | int == 5
          - word == "hello"
          - n > 1
`)
	if passing.Failed() {
		t.Errorf("assert over true conditions failed: %+v", passing.Plays)
	}

	failing := run(t, `
- hosts: all
  gather_facts: false
  vars:
    n: 5
  tasks:
    - assert:
        that:
          - n | int == 6
        fail_msg: n was not six
`)
	if !failing.Failed() {
		t.Fatal("assert over a false condition did not fail")
	}
	var msg string
	for _, p := range failing.Plays {
		for _, r := range p.Results {
			if r.Failed {
				msg = r.Msg
			}
		}
	}
	if msg != "n was not six" {
		t.Errorf("fail_msg = %q, want %q", msg, "n was not six")
	}

	// A bare boolean still works, and a single condition need not be a list.
	if rr := run(t, "- hosts: all\n  gather_facts: false\n  tasks:\n    - assert: {that: true}\n"); rr.Failed() {
		t.Error("assert over a literal true failed")
	}
	if rr := run(t, "- hosts: all\n  gather_facts: false\n  vars: {n: 2}\n  tasks:\n    - assert: {that: \"n | int == 2\"}\n"); rr.Failed() {
		t.Error("assert over a single non-list condition failed")
	}
}

// TestLoopRegisterShape pins the registered value a LOOPED task produces
// against what real ansible-core 2.21.4 produces: exactly the keys
// changed/failed/msg/results, with none of the module's own fields at the
// top level, and one entry per iteration carrying the module fields plus
// item and ansible_loop_var. `results` did not exist here at all.
func TestLoopRegisterShape(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - command: echo "{{ item }}"
      loop: [a, b]
      register: r
    - name: aggregate
      debug:
        msg: "{{ r.results | map(attribute='stdout') | join(',') }}|{{ r.msg }}|{{ r.changed }}|{{ r.results | length }}"
    - name: entry
      debug:
        msg: "{{ r.results[1].item }}/{{ r.results[1].ansible_loop_var }}/{{ r.results[0].rc }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	msgs := map[string]string{}
	e := New(localhostInventory())
	e.OnResult = func(res Result) { msgs[res.Task] = res.Msg }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}

	// Real Ansible: stdout of each iteration, the fixed aggregate msg,
	// the aggregate changed, and one results entry per item.
	if got, want := msgs["aggregate"], "a,b|All items completed|True|2"; got != want {
		t.Errorf("aggregate = %q, want %q", got, want)
	}
	// Each entry carries the module's own fields plus the item and the
	// name of the loop variable it was bound to.
	if got, want := msgs["entry"], "b/item/0"; got != want {
		t.Errorf("entry = %q, want %q", got, want)
	}
}

// TestLoopIndexVar covers loop_control.index_var, which was parsed
// nowhere and rendered as the empty string.
func TestLoopIndexVar(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - debug:
        msg: "{{ idx }}:{{ thing }}"
      loop: [alpha, beta, gamma]
      loop_control:
        loop_var: thing
        index_var: idx
`))
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	e := New(localhostInventory())
	e.OnResult = func(res Result) { seen = append(seen, res.Msg) }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	want := []string{"0:alpha", "1:beta", "2:gamma"}
	if len(seen) != len(want) {
		t.Fatalf("results = %#v, want %#v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("result[%d] = %q, want %q (index_var is 0-based)", i, seen[i], want[i])
		}
	}
}

// TestNonLoopedRegisterStaysFlat guards that giving looped tasks a
// results list did not change the shape of an ordinary one.
func TestNonLoopedRegisterStaysFlat(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - command: echo solo
      register: solo
    - debug:
        msg: "{{ solo.stdout }}|{{ solo.results is defined }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	var last string
	e := New(localhostInventory())
	e.OnResult = func(res Result) { last = res.Msg }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if last != "solo|False" {
		t.Errorf("non-looped register = %q, want %q", last, "solo|False")
	}
}
