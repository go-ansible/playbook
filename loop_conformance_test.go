package playbook

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestLoopWhenIsEvaluatedPerItem pins the most consequential of these:
// a `when:` on a looping task is evaluated ONCE PER ITEM, with `item`
// bound. This port evaluated it once before the loop, where `item` does
// not exist yet — so it was wrong in BOTH directions, and a playbook
// that said to skip an item acted on it anyway.
func TestLoopWhenIsEvaluatedPerItem(t *testing.T) {
	tests := []struct {
		when string
		want string
	}{
		// undefined != 2 was true, so every iteration ran.
		{`item != 2`, "ran-1,ran-3"},
		// undefined == 1 was false, so the whole task was skipped.
		{`item == 1`, "ran-1"},
		{`item > 1`, "ran-2,ran-3"},
		// A condition not mentioning item still applies to every one.
		{`true`, "ran-1,ran-2,ran-3"},
		{`false`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.when, func(t *testing.T) {
			var mu sync.Mutex
			var ran []string
			e := New(localhostInventory())
			e.OnResult = func(r Result) {
				if r.Skipped {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if strings.HasPrefix(r.Msg, "ran-") {
					ran = append(ran, r.Msg)
				}
			}
			pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  tasks:
    - name: filter
      debug: {msg: "ran-{{ item }}"}
      loop: [1, 2, 3]
      when: "` + tt.when + `"
`))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(ran, ","); got != tt.want {
				t.Errorf("when %q ran %q, real ansible-core runs %q", tt.when, got, tt.want)
			}
		})
	}
}

// TestLoopCountsOnceInTheRecap pins real Ansible's per-TASK counting: a
// loop over three items that all changed reports changed=1, not 3. This
// port counted every iteration, so a 100-item loop inflated the recap
// by a hundred.
func TestLoopCountsOnceInTheRecap(t *testing.T) {
	tests := []struct {
		name string
		task string
		want HostSummary
	}{{
		name: "three ok iterations",
		task: `{name: t, debug: {msg: x}, loop: [1,2,3]}`,
		want: HostSummary{Ok: 1},
	}, {
		name: "three changed iterations",
		task: `{name: t, command: "echo {{item}}", loop: [1,2,3]}`,
		want: HostSummary{Ok: 1, Changed: 1},
	}, {
		name: "every iteration skipped",
		task: `{name: t, debug: {msg: x}, loop: [1,2,3], when: false}`,
		want: HostSummary{Skipped: 1},
	}, {
		// One that ran makes the TASK ok; the skipped ones do not count
		// at all.
		name: "one ran, two skipped",
		task: `{name: t, debug: {msg: x}, loop: [1,2,3], when: "item == 1"}`,
		want: HostSummary{Ok: 1},
	}, {
		name: "single item",
		task: `{name: t, debug: {msg: x}, loop: [1]}`,
		want: HostSummary{Ok: 1},
	}, {
		name: "no loop at all",
		task: `{name: t, debug: {msg: x}}`,
		want: HostSummary{Ok: 1},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pb, err := Parse([]byte(fmt.Sprintf("- {name: p, hosts: all, gather_facts: false, tasks: [%s]}\n", tt.task)))
			if err != nil {
				t.Fatal(err)
			}
			e := New(localhostInventory())
			rr, err := e.RunPlaybook(context.Background(), pb)
			if err != nil {
				t.Fatal(err)
			}
			got := rr.Summary()["localhost"]
			if got == nil {
				t.Fatal("no summary for localhost")
			}
			if *got != tt.want {
				t.Errorf("summary = %+v, real ansible-core gives %+v", *got, tt.want)
			}
		})
	}
}

// foldLoops must leave non-looped results alone and in order.
func TestFoldLoopsKeepsOtherResults(t *testing.T) {
	in := []Result{
		{Host: "h1", Task: "a"},
		{Host: "h1", Task: "loop", Looped: true, Skipped: true},
		{Host: "h1", Task: "loop", Looped: true, Changed: true},
		{Host: "h1", Task: "b"},
		// A different host's iterations fold separately.
		{Host: "h2", Task: "loop", Looped: true, Skipped: true},
	}
	got := foldLoops(in)
	if len(got) != 4 {
		t.Fatalf("got %d results, want 4: %+v", len(got), got)
	}
	if got[0].Task != "a" || got[2].Task != "b" {
		t.Errorf("order or content changed: %+v", got)
	}
	// One iteration changed, so the folded task is changed and NOT
	// skipped.
	if !got[1].Changed || got[1].Skipped {
		t.Errorf("folded loop = %+v, want changed and not skipped", got[1])
	}
	if !got[3].Skipped {
		t.Errorf("h2's loop skipped entirely, got %+v", got[2])
	}
}
