package playbook

import (
	"encoding/json"
	"fmt"
	"github.com/go-ansible/modules"
	"io"
	"sort"
	"strings"
	"sync"
)

// verboseAlwaysKey marks a result this callback should print in full
// even without -v — real Ansible's own mechanism, and the reason a debug
// task shows anything at all. The name comes from the modules package
// rather than being spelled twice. Keys beginning "_ansible_" are
// internal and never appear in the dump itself.
const verboseAlwaysKey = modules.VerboseAlwaysKey

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

	// An unnamed play is named after its hosts pattern, which is what
	// real Ansible's Play.get_name() falls back to — measured: a play
	// with `hosts: all` and no name banners as "PLAY [all]", not "PLAY".
	name := strings.TrimSpace(play.Name)
	if name == "" {
		name = strings.TrimSpace(play.Hosts)
	}
	msg := "PLAY"
	if name != "" {
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

	// An unnamed task banners under its module, as real Ansible does —
	// measured: a bare `- debug: {msg: x}` banners "TASK [debug]". This
	// port printed "TASK []", or nothing at all when several unnamed
	// tasks ran in a row.
	banner := r.Task
	if banner == "" {
		banner = r.Module
	}
	if banner != c.lastTask {
		fmt.Fprintf(c.w, "\n%s\n", c.colorize(colorCyan, "TASK ["+banner+"]"))
		c.lastTask = banner
	}
	switch {
	case r.Failed:
		line := fmt.Sprintf("failed: [%s]", r.Host)
		if r.Msg != "" {
			line += " => " + r.Msg
		}
		fmt.Fprintln(c.w, c.colorize(colorRed, line))
		if r.Ignored {
			// Real Ansible says so, on its own line, so a red line that
			// did not stop the run is not mistaken for one that did.
			fmt.Fprintln(c.w, c.colorize(colorCyan, "...ignoring"))
		}
	case r.Skipped:
		fmt.Fprintln(c.w, c.colorize(colorCyan, fmt.Sprintf("skipping: [%s]", r.Host)))
	case r.Changed:
		fmt.Fprintln(c.w, c.colorize(colorYellow, fmt.Sprintf("changed: [%s]", r.Host))+c.verboseDump(r))
	default:
		fmt.Fprintln(c.w, c.colorize(colorGreen, fmt.Sprintf("ok: [%s]", r.Host))+c.verboseDump(r))
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

// verboseDump renders the " => {...}" a result carrying
// _ansible_verbose_always gets — what makes a debug task actually show
// what it found. Real Ansible pretty-prints it at four spaces and sorts
// the keys, and strips its own internal _ansible_* keys from the dump.
// Returns the empty string for every other result, which is why an
// ordinary command still prints one bare line.
func (c *DefaultCallback) verboseDump(r Result) string {
	if v, ok := r.Extra[verboseAlwaysKey].(bool); !ok || !v {
		return ""
	}
	fields := map[string]any{}
	if r.Msg != "" {
		fields["msg"] = r.Msg
	}
	for k, v := range r.Extra {
		if strings.HasPrefix(k, "_ansible_") {
			continue
		}
		fields[k] = v
	}
	if len(fields) == 0 {
		return ""
	}
	data, err := json.MarshalIndent(fields, "", "    ")
	if err != nil {
		return ""
	}
	return " => " + string(data)
}
