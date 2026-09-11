package playbook

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

const (
	colorGreen  = "\033[0;32m"
	colorYellow = "\033[0;33m"
	colorRed    = "\033[0;31m"
	colorCyan   = "\033[0;36m"
	colorReset  = "\033[0m"
)

// DefaultCallback prints a run the way ansible-playbook prints it to a
// terminal — PLAY and TASK banners, a colored per-host line for every
// result, and a PLAY RECAP — and is this port's equivalent of real
// Ansible's own ansible.builtin.default stdout callback.
//
// Its banners are not padded out with asterisks the way real Ansible's
// Display.banner pads them to the terminal width; that is a cosmetic
// difference this port has always had, named here rather than left to
// be discovered.
type DefaultCallback struct {
	BaseCallback

	w     io.Writer
	color bool

	// mu serializes the whole of each hook. Results arrive concurrently
	// from one goroutine per host, so without it both lastTask and the
	// writer itself would be raced and lines from different hosts could
	// interleave mid-write.
	mu       sync.Mutex
	lastTask string
}

// NewDefaultCallback returns a DefaultCallback writing to w, with ANSI
// color escapes when color is true.
func NewDefaultCallback(w io.Writer, color bool) *DefaultCallback {
	return &DefaultCallback{w: w, color: color}
}

func (c *DefaultCallback) colorize(code, s string) string {
	if !c.color {
		return s
	}
	return code + s + colorReset
}

func (c *DefaultCallback) OnPlayStart(play Play) {
	c.mu.Lock()
	defer c.mu.Unlock()

	msg := "PLAY"
	if name := strings.TrimSpace(play.Name); name != "" {
		msg = "PLAY [" + name + "]"
	}
	fmt.Fprintf(c.w, "\n%s\n", c.colorize(colorCyan, msg))

	// A new play re-banners its first task even when the previous play
	// ended on a task of the same name.
	c.lastTask = ""
}

func (c *DefaultCallback) OnTaskResult(r Result) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if r.Task != c.lastTask {
		fmt.Fprintf(c.w, "\n%s\n", c.colorize(colorCyan, "TASK ["+r.Task+"]"))
		c.lastTask = r.Task
	}
	switch {
	case r.Failed:
		line := fmt.Sprintf("failed: [%s]", r.Host)
		if r.Msg != "" {
			line += " => " + r.Msg
		}
		fmt.Fprintln(c.w, c.colorize(colorRed, line))
	case r.Skipped:
		fmt.Fprintln(c.w, c.colorize(colorCyan, fmt.Sprintf("skipping: [%s]", r.Host)))
	case r.Changed:
		fmt.Fprintln(c.w, c.colorize(colorYellow, fmt.Sprintf("changed: [%s]", r.Host)))
	default:
		fmt.Fprintln(c.w, c.colorize(colorGreen, fmt.Sprintf("ok: [%s]", r.Host)))
	}
}

func (c *DefaultCallback) OnStats(rr *RunResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	fmt.Fprintln(c.w)
	fmt.Fprintln(c.w, c.colorize(colorCyan, "PLAY RECAP"))

	summary := rr.Summary()
	hosts := make([]string, 0, len(summary))
	for h := range summary {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		s := summary[h]
		// Real Ansible's own column set and order. Ok already includes
		// changed and ignored results, as it does there.
		line := fmt.Sprintf("%-24s : ok=%-4d changed=%-4d unreachable=%-4d failed=%-4d skipped=%-4d rescued=%-4d ignored=%-4d",
			h, s.Ok, s.Changed, s.Unreachable, s.Failed, s.Skipped, s.Rescued, s.Ignored)
		code := colorGreen
		if s.Failed > 0 || s.Unreachable > 0 {
			code = colorRed
		} else if s.Changed > 0 {
			code = colorYellow
		}
		fmt.Fprintln(c.w, c.colorize(code, line))
	}
}
