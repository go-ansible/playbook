package playbook

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

func (c *recordingCallback) OnPlayStart(play Play, hosts []string) {
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

	cb.OnPlayStart(Play{Name: "deploy"}, []string{"h1"})
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
		"fatal: [web2]: FAILED! => {\"changed\": false, \"msg\": \"boom\"}\n" +
		"\nPLAY RECAP\n" +
		"web1                       : ok=1    changed=0    unreachable=0    failed=0    skipped=1    rescued=0    ignored=0   \n" +
		"web2                       : ok=1    changed=1    unreachable=0    failed=1    skipped=0    rescued=0    ignored=0   \n" +
		"web3                       : ok=1    changed=1    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0   \n" +
		// Real ansible-core ends its output with a blank line after the
		// recap — measured with od, not assumed. This test asserted the
		// opposite until then.
		"\n"
	if got := buf.String(); got != want {
		t.Errorf("output =\n%q\nwant\n%q", got, want)
	}
}

func TestDefaultCallbackUnnamedPlay(t *testing.T) {
	var buf bytes.Buffer
	// Real Ansible prints a bare "PLAY" banner for a play with no name.
	NewDefaultCallback(&buf, false).OnPlayStart(Play{Name: "  "}, []string{"h1"})
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
	cb.OnPlayStart(Play{Name: "first"}, []string{"h1"})
	cb.OnTaskResult(Result{Host: "h", Task: "shared"})
	cb.OnPlayStart(Play{Name: "second"}, []string{"h1"})
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
	// "Gathering Facts" is real Ansible's own banner for the implicit
	// fact-gathering step; this port used to call it "(gather_facts)".
	if got := cb.tasks(); len(got) != 2 || got[0] != "Gathering Facts" || got[1] != "speak" {
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
	// A connect failure is no longer a pseudo-task of its own: real
	// Ansible connects per task, so it surfaces under the first task
	// that needs a connection — here the implicit "Gathering Facts".
	got := cb.tasks()
	if len(got) != 1 || got[0] != "Gathering Facts" {
		t.Fatalf("results = %#v, want just the connect failure", got)
	}
	if !cb.results[0].Failed || !cb.results[0].Unreachable ||
		!strings.Contains(cb.results[0].Msg, "no route to host") {
		t.Errorf("connect result = %#v, want an unreachable failure carrying the dial error", cb.results[0])
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

// TestIncludeRoleScopesVarsImportRoleDoesNot pins the difference measured
// against real ansible-core 2.21.4 by running each form in isolation:
// include_role is dynamic and its variables leave scope with it, while
// import_role is static and real Ansible injects its variables for the
// whole play. This port unwound both.
func TestIncludeRoleScopesVarsImportRoleDoesNot(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/r3/vars/main.yml", "r3_var: v3\n")
	writePlaybookFile(t, dir, "roles/r3/tasks/main.yml", "- name: inside\n  debug: {msg: \"{{ r3_var }}\"}\n")

	run := func(t *testing.T, directive string) string {
		t.Helper()
		pbPath := writePlaybookFile(t, dir, directive+".yml", `
- hosts: all
  gather_facts: false
  tasks:
    - `+directive+`: {name: r3}
    - name: after
      debug:
        msg: "{{ r3_var | default('GONE') }}"
`)
		pb, err := ParseFile(pbPath)
		if err != nil {
			t.Fatal(err)
		}
		var after string
		e := New(localhostInventory())
		e.OnResult = func(r Result) {
			if r.Task == "after" {
				after = r.Msg
			}
		}
		if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
			t.Fatal(err)
		}
		return after
	}

	if got := run(t, "include_role"); got != "GONE" {
		t.Errorf("after include_role: %q, want GONE (dynamic, vars leave scope)", got)
	}
	if got := run(t, "import_role"); got != "v3" {
		t.Errorf("after import_role: %q, want v3 (static, vars persist)", got)
	}
}

// TestSetFactBeatsPlayVars covers the precedence rung set_fact actually
// sits on. Its output used to land in the Facts layer, below play vars,
// so a play var of the same name won — the opposite of real Ansible.
func TestSetFactBeatsPlayVars(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  vars:
    v: from_play_vars
  tasks:
    - name: before
      debug: {msg: "{{ v }}"}
    - set_fact:
        v: from_set_fact
    - name: after
      debug: {msg: "{{ v }}"}
`))
	if err != nil {
		t.Fatal(err)
	}
	msgs := map[string]string{}
	e := New(localhostInventory())
	e.OnResult = func(r Result) { msgs[r.Task] = r.Msg }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if msgs["before"] != "from_play_vars" {
		t.Errorf("before set_fact = %q, want the play var", msgs["before"])
	}
	if msgs["after"] != "from_set_fact" {
		t.Errorf("after set_fact = %q, want the set_fact to win over the play var", msgs["after"])
	}
}

// TestRoleMetaDependenciesRunFirst covers role dependencies, which this
// port read nowhere: a role's meta/main.yml dependencies: run before its
// own tasks, each with its own defaults/vars.
func TestRoleMetaDependenciesRunFirst(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/dep/vars/main.yml", "dep_var: from_dep\n")
	writePlaybookFile(t, dir, "roles/dep/tasks/main.yml", "- name: dep task\n  debug: {msg: \"{{ dep_var }}\"}\n")
	writePlaybookFile(t, dir, "roles/main/meta/main.yml", "dependencies:\n  - dep\n")
	writePlaybookFile(t, dir, "roles/main/tasks/main.yml", "- name: main task\n  debug: {msg: ran}\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", "- hosts: all\n  gather_facts: false\n  roles: [main]\n")

	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	e := New(localhostInventory())
	e.OnResult = func(r Result) { order = append(order, r.Task+"="+r.Msg) }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	want := []string{"dep task=from_dep", "main task=ran"}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Errorf("order = %#v, want %#v (dependency first, with its own vars)", order, want)
	}
}

// TestRoleDependencyCycleIsAnError guards that a cycle is reported
// rather than recursed into until the stack dies.
func TestRoleDependencyCycleIsAnError(t *testing.T) {
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/a/meta/main.yml", "dependencies: [b]\n")
	writePlaybookFile(t, dir, "roles/a/tasks/main.yml", "- debug: {msg: a}\n")
	writePlaybookFile(t, dir, "roles/b/meta/main.yml", "dependencies: [a]\n")
	writePlaybookFile(t, dir, "roles/b/tasks/main.yml", "- debug: {msg: b}\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", "- hosts: all\n  gather_facts: false\n  roles: [a]\n")

	_, err := ParseFile(pbPath)
	if err == nil {
		t.Fatal("a dependency cycle parsed without error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %v, want it to name the cycle", err)
	}
}

// TestRoleSrcResolvesAgainstRoleDirs covers the search path that makes
// `src: hello.txt` work inside a role at all. Without it every role
// shipping a file failed with "no such file or directory".
func TestRoleSrcResolvesAgainstRoleDirs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	writePlaybookFile(t, dir, "roles/r/files/hello.txt", "from-role-files\n")
	writePlaybookFile(t, dir, "roles/r/templates/t.j2", "rendered:{{ who }}\n")
	writePlaybookFile(t, dir, "roles/r/vars/main.yml", "who: ada\n")
	writePlaybookFile(t, dir, "roles/r/tasks/main.yml", `
- name: copy from role files
  copy: {src: hello.txt, dest: `+out+`/copied.txt}
- name: template from role templates
  template: {src: t.j2, dest: `+out+`/rendered.txt}
`)
	pbPath := writePlaybookFile(t, dir, "site.yml", "- hosts: all\n  gather_facts: false\n  roles: [r]\n")

	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	rr, err := e2eRun(t, pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	for name, want := range map[string]string{
		"copied.txt":   "from-role-files\n",
		"rendered.txt": "rendered:ada\n",
	} {
		got, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func e2eRun(t *testing.T, pb Playbook) (*RunResult, error) {
	t.Helper()
	return New(localhostInventory()).RunPlaybook(context.Background(), pb)
}

// TestDefaultCallbackRecapColors pins the coloured PLAY RECAP line
// against bytes CAPTURED from real ansible-core 2.21.4 (run with
// ANSIBLE_FORCE_COLOR=1 and read through cat -v), not against a reading
// of ansible/utils/color.py. Two things here are easy to get wrong and
// were wrong before this test existed: only the host name is coloured
// (not the whole line), and a column whose count is zero stays
// UNCOLOURED even though every other column around it is painted.
func TestDefaultCallbackRecapColors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		results []Result
		rescued map[string]int
		want    string
	}{{
		// Real witness, for a play that ended ok=4 changed=1
		// unreachable=0 failed=1 skipped=1 rescued=1 ignored=1:
		//
		//	\033[0;31mh1\033[0m                         : \033[0;32mok=4   \033[0m ...
		name: "failed host, every column but unreachable non-zero",
		results: []Result{
			{Host: "h1"},
			{Host: "h1", Changed: true},
			{Host: "h1", Skipped: true},
			{Host: "h1", Failed: true, Ignored: true},
			{Host: "h1", Failed: true}, // the one the rescue below recovered
			{Host: "h1"},               // the rescue task itself, which counts as ok
			{Host: "h1", Failed: true},
		},
		rescued: map[string]int{"h1": 1},
		want: "\033[0;31mh1\033[0m                         : " +
			"\033[0;32mok=4   \033[0m \033[0;33mchanged=1   \033[0m unreachable=0    " +
			"\033[0;31mfailed=1   \033[0m \033[0;36mskipped=1   \033[0m " +
			"\033[0;32mrescued=1   \033[0m \033[1;35mignored=1   \033[0m\n",
	}, {
		// Real witness, for an unreachable host: the host name goes
		// red (not bright red), while the unreachable COLUMN is
		// bright red — two different colours in one line.
		name:    "unreachable host",
		results: []Result{{Host: "hx", Unreachable: true}},
		want: "\033[0;31mhx\033[0m                         : ok=0    changed=0    " +
			"\033[1;31munreachable=1   \033[0m failed=0    skipped=0    rescued=0    ignored=0   \n",
	}, {
		// A clean run: the host is green, and only the one non-zero
		// column is painted.
		name:    "all ok",
		results: []Result{{Host: "h1"}},
		want: "\033[0;32mh1\033[0m                         : \033[0;32mok=1   \033[0m " +
			"changed=0    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0   \n",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			NewDefaultCallback(&buf, true).OnStats(&RunResult{
				Plays: []PlayResult{{Results: tc.results, Rescued: tc.rescued}},
			})
			got := buf.String()
			_, line, ok := strings.Cut(got, "PLAY RECAP\033[0m\n")
			if !ok {
				t.Fatalf("no recap banner in %q", got)
			}
			// Real ansible-core ends its output with one blank line
			// after the recap rows, so the captured line is followed
			// by an empty one.
			if line != tc.want+"\n" {
				t.Errorf("recap =\n%q\nwant\n%q", line, tc.want+"\n")
			}
		})
	}
}

// TestIgnoreUnreachableKeepsTheHost pins the semantics MEASURED against
// real ansible-core 2.21.4: an unreachable host whose task said to
// ignore it stays in the play, the NEXT task tries to connect again,
// and the recap files it under ok+ignored rather than unreachable.
//
// The decision is per TASK, which is what makes an ignored-unreachable
// ping followed by an ordinary command report UNREACHABLE twice and
// drop the host only on the second — and a `debug` between them run
// normally, since real never touches the connection for one.
func TestIgnoreUnreachableKeepsTheHost(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - {name: dead, command: "true", ignore_unreachable: true}
    - {name: no connection needed, debug: {msg: hi}}
    - {name: needs one, command: "true"}
    - {name: never reached, debug: {msg: nope}}
`))
	if err != nil {
		t.Fatal(err)
	}
	cb := &recordingCallback{}
	e := New(localhostInventory())
	e.Callbacks = []Callback{cb}
	e.Connect = func(context.Context, string, map[string]any) (remoteexec.Connection, error) {
		return nil, errors.New("no route to host")
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"dead", "no connection needed", "needs one"}
	if got := cb.tasks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("tasks = %#v, want %#v — the ignored one carries on, the second unreachable drops the host", got, want)
	}
	if r := cb.results[0]; !r.Unreachable || !r.Ignored {
		t.Errorf("first result = %#v, want unreachable AND ignored", r)
	}
	if r := cb.results[2]; !r.Unreachable || r.Ignored {
		t.Errorf("third result = %#v, want unreachable and NOT ignored", r)
	}
	s := rr.Summary()["localhost"]
	if s == nil || s.Unreachable != 1 || s.Ignored != 1 || s.Ok != 2 {
		t.Errorf("summary = %+v, want unreachable=1 ignored=1 ok=2", s)
	}
	// Unreachable is what real's exit code keys on, separately from
	// Failed, and it wins over it.
	if !rr.Unreachable() {
		t.Error("Unreachable() = false, want true")
	}
}

// TestIgnoreUnreachableOnThePlay: the play-level keyword covers every
// task including the implicit "Gathering Facts", and a play that
// ignores every unreachable host reports unreachable=0 and is not a
// failure at all. Measured on 2.21.4 (exit 0, ok=4 ignored=2).
func TestIgnoreUnreachableOnThePlay(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  ignore_unreachable: true
  tasks:
    - {name: t, debug: {msg: hi}}
`))
	if err != nil {
		t.Fatal(err)
	}
	cb := &recordingCallback{}
	e := New(localhostInventory())
	e.Callbacks = []Callback{cb}
	e.Connect = func(context.Context, string, map[string]any) (remoteexec.Connection, error) {
		return nil, errors.New("no route to host")
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Gathering Facts", "t"}
	if got := cb.tasks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("tasks = %#v, want %#v", got, want)
	}
	if !cb.results[0].Ignored {
		t.Error("the implicit gather step should inherit the play's ignore_unreachable")
	}
	if rr.Failed() || rr.Unreachable() {
		t.Error("a play that ignores every unreachable host is not a failed run")
	}
}

// TestMetaBannersWithoutAResult: real Ansible's meta: fires the
// task-start callback and NO runner callback, so it prints a TASK
// header with nothing under it and counts toward nothing in the recap.
// This port reported an ok line for it and failed outright on
// meta: noop.
func TestMetaBannersWithoutAResult(t *testing.T) {
	pb, err := Parse([]byte(`
- hosts: all
  gather_facts: false
  tasks:
    - {name: nothing, meta: noop}
    - {name: real work, debug: {msg: hi}}
`))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	e := New(localhostInventory())
	e.Callbacks = []Callback{NewDefaultCallback(&buf, false)}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if want := "\nTASK [nothing]\n\nTASK [real work]\n"; !strings.Contains(buf.String(), want) {
		t.Errorf("output =\n%q\nwant it to contain %q", buf.String(), want)
	}
	if s := rr.Summary()["localhost"]; s == nil || s.Ok != 1 {
		t.Errorf("summary = %+v, want ok=1 — meta counts toward nothing", s)
	}
}

// TestBlockAndIncludeVars pins vars: on a block and on an include,
// MEASURED against real ansible-core 2.21.4. The BlockVars layer had
// existed — named, ordered between the play's and the task's — with
// nothing ever writing to it, so `block: {vars: ...}` was accepted and
// dropped, and so was the vars: on an include_tasks/import_tasks,
// which parse into a synthetic block here. A playbook parameterising
// an included file got the variable undefined.
func TestBlockAndIncludeVars(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "inc.yml"), []byte(
		"- {name: included, debug: {msg: \"got {{ passed | default('nothing') }}\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "play.yml"), []byte(`
- hosts: all
  gather_facts: false
  vars: {v: play}
  tasks:
    - name: the block
      vars: {v: block, only_here: 1}
      block:
        - {name: inside, debug: {msg: "{{ v }}/{{ only_here }}"}}
        - {name: task wins, vars: {v: task}, debug: {msg: "{{ v }}"}}
    - {name: after, debug: {msg: "{{ v }}/{{ only_here | default('gone') }}"}}
    - {name: imported, import_tasks: inc.yml, vars: {passed: from-import}}
    - {name: included, include_tasks: inc.yml, vars: {passed: from-include}}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	pb, err := ParseFile(filepath.Join(dir, "play.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cb := &recordingCallback{}
	e := New(localhostInventory())
	e.Callbacks = []Callback{cb}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}

	msgs := map[string][]string{}
	var includedLine string
	for _, r := range cb.results {
		if r.Included != "" {
			includedLine = r.Included
			continue
		}
		msgs[r.Task] = append(msgs[r.Task], r.Msg)
	}
	for _, tc := range []struct{ task, want string }{
		{"inside", "block/1"},
		{"task wins", "task"},
		// Block vars go out of scope with the block, as they do there.
		{"after", "play/gone"},
	} {
		if got := msgs[tc.task]; len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: msg = %v, want [%q]", tc.task, got, tc.want)
		}
	}
	// Both include forms carry their vars in — import was dropping them too.
	want := []string{"got from-import", "got from-include"}
	if got := msgs["included"]; !reflect.DeepEqual(got, want) {
		t.Errorf("included msgs = %v, want %v", got, want)
	}
	// And only the DYNAMIC one announces itself, with an absolute path.
	if !filepath.IsAbs(includedLine) || filepath.Base(includedLine) != "inc.yml" {
		t.Errorf("included line = %q, want the absolute path of inc.yml", includedLine)
	}
}

// TestIncludeTasksFileForm: real accepts include_tasks: {file: path}
// as well as the bare string. Only the string parsed here, so the
// documented mapping form was a parse error.
func TestIncludeTasksFileForm(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "inc.yml"), []byte(
		"- {name: included, debug: {msg: hi}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "play.yml"), []byte(`
- hosts: all
  gather_facts: false
  tasks:
    - name: mapping form
      include_tasks: {file: inc.yml}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	pb, err := ParseFile(filepath.Join(dir, "play.yml"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n := len(pb[0].Tasks[0].Block); n != 1 {
		t.Fatalf("block has %d tasks, want the one from the included file", n)
	}
}
