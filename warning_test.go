package playbook

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
)

// The shape is real's, measured: the literal "[WARNING]: ", the
// message, one line, nothing else.
func TestWarnerFormat(t *testing.T) {
	var buf bytes.Buffer
	w := NewWarner(&buf)
	w("Could not match supplied host pattern, ignoring: zzz")
	want := "[WARNING]: Could not match supplied host pattern, ignoring: zzz\n"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

// Real's display.warning keeps the set of warnings it has issued and
// says each one once. Measured on a three-play playbook against an
// empty inventory: one "provided hosts list is empty" line, not three.
func TestWarnerDeduplicates(t *testing.T) {
	var buf bytes.Buffer
	w := NewWarner(&buf)
	w("same")
	w("same")
	w("different")
	w("same")
	got := buf.String()
	want := "[WARNING]: same\n[WARNING]: different\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A run warns from whichever goroutine noticed, and strategy: free has
// several at once. Meaningful under -race, which CI runs.
func TestWarnerIsConcurrencySafe(t *testing.T) {
	var buf bytes.Buffer
	w := NewWarner(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w("racing")
		}()
	}
	wg.Wait()
	if n := strings.Count(buf.String(), "racing"); n != 1 {
		t.Fatalf("wrote the message %d times, want 1", n)
	}
}

// An Engine built as a zero value rather than by New has no Warner,
// and must stay usable rather than panicking on the first warning.
func TestEngineWithNilWarnerDoesNotPanic(t *testing.T) {
	e := &Engine{}
	e.warn("nobody is listening")
}

// End to end: a play whose hosts: matches nothing warns, once per
// distinct term, exactly as real does. Measured against ansible-core
// 2.21.4 -- three plays with patterns aaa, bbb, aaa produced two
// warnings, one for aaa and one for bbb.
func TestPlayWithUnmatchedPatternWarns(t *testing.T) {
	pb, err := Parse([]byte(`
- name: pA
  hosts: aaa
  gather_facts: false
  tasks: [{debug: {msg: x}}]
- name: pB
  hosts: bbb
  gather_facts: false
  tasks: [{debug: {msg: x}}]
- name: pC
  hosts: aaa
  gather_facts: false
  tasks: [{debug: {msg: x}}]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var buf bytes.Buffer
	e := New(localhostInventory())
	e.Warn = NewWarner(&buf)
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}

	want := "[WARNING]: Could not match supplied host pattern, ignoring: aaa\n" +
		"[WARNING]: Could not match supplied host pattern, ignoring: bbb\n"
	if buf.String() != want {
		t.Fatalf("warnings =\n%q\nwant\n%q", buf.String(), want)
	}
}

// A play that DOES match warns nothing -- the warning must not fire on
// the ordinary path.
func TestPlayWithMatchedPatternWarnsNothing(t *testing.T) {
	pb, err := Parse([]byte(`
- name: p
  hosts: all
  gather_facts: false
  tasks: [{debug: {msg: x}}]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var buf bytes.Buffer
	e := New(localhostInventory())
	e.Warn = NewWarner(&buf)
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("warned %q, want nothing", buf.String())
	}
}

// A --limit term that matches nothing warns with the SAME sentence,
// and only once however many plays the limit is applied to.
func TestUnmatchedLimitWarnsOnce(t *testing.T) {
	pb, err := Parse([]byte(`
- name: pA
  hosts: all
  gather_facts: false
  tasks: [{debug: {msg: x}}]
- name: pB
  hosts: all
  gather_facts: false
  tasks: [{debug: {msg: x}}]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var buf bytes.Buffer
	e := New(localhostInventory())
	e.Warn = NewWarner(&buf)
	e.Limit = "zzz"
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	want := "[WARNING]: Could not match supplied host pattern, ignoring: zzz\n"
	if buf.String() != want {
		t.Fatalf("warnings = %q, want %q", buf.String(), want)
	}
}
