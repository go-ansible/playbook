package playbook

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// Warner reports a non-fatal diagnostic — something the user should
// see, that does not stop the run. It is this port's equivalent of
// real Ansible's display.warning.
//
// It is separate from Callback on purpose. A callback reports the
// PROGRESS of a run and writes to the run's own output stream; a
// warning is about the run's INPUTS (a pattern that matched nothing,
// an inventory that could not be read) and real writes it to stderr,
// so that a caller piping a playbook's output somewhere still sees it.
// Real makes the same split, between its callback plugins and the one
// Display singleton they all share.
type Warner func(msg string)

// NewWarner returns a Warner that writes each message to w in real
// Ansible's own shape — the literal "[WARNING]: ", then the message,
// on one line — and writes each DISTINCT message at most once.
//
// The deduplication is real's, not a convenience: display.warning
// keeps the set of warnings it has already issued, and a run that
// would warn the same thing repeatedly says it once. Measured against
// ansible-core 2.21.4 with a three-play playbook whose plays matched
// an empty inventory: "provided hosts list is empty" appeared once
// rather than three times, and two plays sharing a mistyped pattern
// produced one line for it, not two. Without this, a playbook with
// twenty plays would say the same thing twenty times where real says
// it once.
//
// The returned Warner is safe for concurrent use, because a run warns
// from whichever goroutine noticed — and strategy: free has several.
func NewWarner(w io.Writer) Warner {
	var mu sync.Mutex
	seen := map[string]bool{}
	return func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		if seen[msg] {
			return
		}
		seen[msg] = true
		fmt.Fprintf(w, "[WARNING]: %s\n", msg)
	}
}

// warn reports through the Engine's Warner, tolerating a nil one so
// that an Engine built as a zero value — rather than by New — stays
// usable and simply says nothing.
func (e *Engine) warn(msg string) {
	if e.Warn != nil {
		e.Warn(msg)
	}
}

// defaultWarner is what New installs: real's destination, stderr.
func defaultWarner() Warner { return NewWarner(os.Stderr) }
