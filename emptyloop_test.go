package playbook

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

// TestEmptyLoopSkipsTheTask pins what ansible-core 2.21.4 does with a
// loop that has nothing to iterate: it SKIPS the task, banner and all,
// and the recap counts it. Measured on four spellings, each of which
// produced "skipping: [localhost]" and skipped=1 in real, and NOTHING
// at all in this port before this change — no banner, no line,
// skipped=0.
func TestEmptyLoopSkipsTheTask(t *testing.T) {
	for _, tc := range []struct{ name, task string }{
		{"with_items over an empty literal", "      with_items: []"},
		{"loop over an empty variable", `      loop: "{{ empty_list }}"`},
		{"with_fileglob matching nothing", `      with_fileglob: "nothing_that_exists_*.zzz"`},
		{"with_first_found with skip", "      with_first_found:\n        - files: [nope1, nope2]\n          skip: true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pb, err := Parse([]byte(`
- name: empty loop
  hosts: all
  gather_facts: false
  vars:
    empty_list: []
  tasks:
    - name: the empty one
      debug: msg="never {{ item }}"
` + tc.task + `
    - name: after
      debug: msg=after
`))
			if err != nil {
				t.Fatal(err)
			}
			rr, err := New(localhostInventory()).RunPlaybook(context.Background(), pb)
			if err != nil {
				t.Fatal(err)
			}
			if rr.Failed() {
				t.Fatalf("run failed: %+v", rr.Plays)
			}

			var skipped []Result
			sawAfter := false
			for _, p := range rr.Plays {
				for _, r := range p.Results {
					if r.Skipped {
						skipped = append(skipped, r)
					}
					if r.Task == "after" {
						sawAfter = true
					}
				}
			}
			if !sawAfter {
				t.Error("the task after the empty loop did not run")
			}
			if len(skipped) != 1 {
				t.Fatalf("got %d skipped results, real skips the task exactly once", len(skipped))
			}
			r := skipped[0]
			if r.Task != "the empty one" {
				t.Errorf("the skip is attributed to %q", r.Task)
			}
			// Real prints a bare "skipping: [host]" with no
			// "(item=...)", because no item exists to name.
			if r.Looped {
				t.Error("the skip must not be marked as a loop iteration: real prints no (item=...)")
			}
			// And the recap counts it, which is the visible half.
			if got := rr.Summary()["localhost"].Skipped; got != 1 {
				t.Errorf("recap skipped = %d, real reports 1", got)
			}
		})
	}
}

// TestEmptyLoopRegistersRealsShape pins the registered value, measured
// against real: six keys, the reason under BOTH names, and an empty
// results list.
func TestEmptyLoopRegistersRealsShape(t *testing.T) {
	pb, err := Parse([]byte(`
- name: empty loop
  hosts: all
  gather_facts: false
  tasks:
    - name: the empty one
      debug: msg="never {{ item }}"
      with_items: []
      register: r
    - name: report
      debug: msg="{{ r.skipped }}/{{ r.skip_reason }}/{{ r.skipped_reason }}/{{ r.results }}/{{ r.changed }}/{{ r.failed }}"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	var reported string
	e.OnResult = func(res Result) {
		if res.Task == "report" {
			reported = res.Msg
		}
	}
	rr, err := e.RunPlaybook(context.Background(), pb)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Failed() {
		t.Fatalf("run failed: %+v", rr.Plays)
	}
	// Real: True/No items in the list/No items in the list/[]/False/False
	if want := "True/No items in the list/No items in the list/[]/False/False"; reported != want {
		t.Errorf("registered value rendered as\n got %q\nwant %q (real's own)", reported, want)
	}

	// And the six keys, as real's dict2items|sort reports them.
	var keys []string
	for _, p := range rr.Plays {
		for _, r := range p.Results {
			if r.Task == "the empty one" && r.Skipped {
				for k := range r.Extra {
					keys = append(keys, k)
				}
			}
		}
	}
	sort.Strings(keys)
	if want := []string{"results", "skip_reason", "skipped_reason"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("the skip result carries %v, want %v", keys, want)
	}
}
