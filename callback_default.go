package playbook

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/go-ansible/modules"
	"github.com/go-ansible/template"
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
	// COLOR_DEBUG's default, "dark gray", which real Ansible uses
	// for the retry line. Confirmed both in ansible/constants.py and
	// by capturing the real escape from a coloured run.
	colorDarkGray = "\033[1;30m"
	// COLOR_UNREACHABLE ("bright red") and COLOR_WARN ("bright
	// purple"), which the PLAY RECAP uses for those two columns and
	// nothing else does. Captured from a real coloured run rather
	// than derived from the colour names.
	colorBrightRed    = "\033[1;31m"
	colorBrightPurple = "\033[1;35m"
	colorReset        = "\033[0m"
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

// countField is real Ansible's colorize() (ansible/utils/color.py): a
// "lead=N" column whose count is padded to four characters, coloured
// only when the count is NOT zero. That last condition is the whole
// reason a real recap shows "unreachable=0" plain next to a coloured
// "failed=1".
func (c *DefaultCallback) countField(lead string, n int, code string) string {
	field := fmt.Sprintf("%s=%-4d", lead, n)
	if n == 0 {
		return field
	}
	return c.colorize(code, field)
}

func (c *DefaultCallback) OnPlayStart(play Play, hosts []string) {
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

	// Real Ansible says so rather than printing nothing, so a mistyped
	// host pattern is distinguishable from a play that genuinely had
	// no work. (Real also emits a "[WARNING]: Could not match supplied
	// host pattern" line on STDERR; this callback holds only the one
	// output stream, so that line is a remaining gap.)
	if len(hosts) == 0 {
		fmt.Fprintln(c.w, "skipping: no hosts matched")
	}

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
	c.taskBanner(r)
	if r.BannerOnly {
		return
	}
	// Real Ansible's default callback emits the diff before the line
	// that says what happened, so the reader sees the change and then
	// its verdict. Measured from a real --diff --check run, which also
	// showed the blank line renderDiff leaves after each one.
	for _, d := range r.Diffs {
		fmt.Fprint(c.w, renderDiff(d, c.colorize))
	}

	switch {
	case r.Included != "":
		// Real Ansible's own announcement for a dynamic include, in
		// the default (uncoloured) colour — measured, not assumed.
		fmt.Fprintf(c.w, "included: %s for %s\n", r.Included, r.Host)
	case r.Failed:
		// Real Ansible writes "fatal: [h]: FAILED! => {json}" with the
		// whole result inline, which is how a reader sees WHY a command
		// failed — its rc, its stderr. This port printed only the msg,
		// so a failing shell task showed a return code and nothing else.
		// An unreachable host gets its own UNREACHABLE! prefix there,
		// measured the same way.
		// An unreachable host's line is COLOR_UNREACHABLE — bright
		// red — where an ordinary failure is COLOR_ERROR, plain red.
		// Captured from a real coloured run, not inferred.
		kind, code := "FAILED!", colorRed
		if r.Unreachable {
			kind, code = "UNREACHABLE!", colorBrightRed
		}
		line := fmt.Sprintf("fatal: [%s]: %s => %s", hostLabel(r), kind, c.resultJSON(r))
		fmt.Fprintln(c.w, c.colorize(code, line))
		if r.Ignored {
			// Real Ansible says so, on its own line, so a red line that
			// did not stop the run is not mistaken for one that did.
			fmt.Fprintln(c.w, c.colorize(colorCyan, "...ignoring"))
		}
	case r.Skipped:
		fmt.Fprintln(c.w, c.colorize(colorCyan, fmt.Sprintf("skipping: [%s]%s", r.Host, skippedItemLabel(r))))
	case r.Changed:
		fmt.Fprintln(c.w, c.colorize(colorYellow, fmt.Sprintf("changed: [%s]%s", hostLabel(r), itemLabel(r)))+c.verboseDump(r))
	default:
		fmt.Fprintln(c.w, c.colorize(colorGreen, fmt.Sprintf("ok: [%s]%s", hostLabel(r), itemLabel(r)))+c.verboseDump(r))
	}
}

// taskBanner prints the TASK banner for r unless it is already the
// banner showing. The caller must hold c.mu.
//
// Both OnTaskResult and OnTaskRetry go through it, because a retried
// task reports its retries BEFORE it ever produces a result: real
// ansible-core prints the banner, then the FAILED - RETRYING lines,
// then the outcome, and banner-on-first-result-only put the retries
// above the banner instead.
func (c *DefaultCallback) taskBanner(r Result) {
	banner := DisplayName(r.Role, r.Task, r.Module)
	// Real Ansible banners a handler run differently, which is how a
	// reader tells it from an ordinary task of the same name.
	kind := "TASK"
	if r.Handler {
		kind = "RUNNING HANDLER"
	}
	// The kind is part of the identity: a handler that shares a task's
	// name still gets its own banner.
	if key := kind + " [" + banner + "]"; key != c.lastTask {
		fmt.Fprintf(c.w, "\n%s\n", c.colorize(colorCyan, key))
		c.lastTask = key
	}
}

// OnTaskRetry prints real Ansible's own retry line, verbatim from
// default.py's v2_runner_retry — including its unpluralised "1 retries
// left" and the trailing full stop. An unnamed task is named after its
// module, the same fallback the task banner uses.
func (c *DefaultCallback) OnTaskRetry(r Result, left int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.taskBanner(r)

	line := fmt.Sprintf("FAILED - RETRYING: [%s]: %s (%d retries left).",
		hostLabel(r), DisplayName(r.Role, r.Task, r.Module), left)
	fmt.Fprintln(c.w, c.colorize(colorDarkGray, line))
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
		// Real Ansible's own column set and order (default.py's
		// v2_playbook_on_stats). Ok already includes changed and
		// ignored results, as it does there.
		//
		// The host name is the only part hostcolor() colours, and the
		// column it sits in differs with colour: "%-26s" of the bare
		// name, but "%-37s" of the COLOURED one — and since the
		// escapes are exactly 11 characters, both come out 26 columns
		// wide on screen. Padding the whole line to 24 and wrapping
		// all of it in one colour, as this did before, was wrong on
		// both counts.
		code := colorGreen
		if s.Failed > 0 || s.Unreachable > 0 {
			code = colorRed
		} else if s.Changed > 0 {
			code = colorYellow
		}
		host := fmt.Sprintf("%-26s", h)
		if c.color {
			host = fmt.Sprintf("%-37s", c.colorize(code, h))
		}
		fmt.Fprintf(c.w, "%s : %s %s %s %s %s %s %s\n", host,
			c.countField("ok", s.Ok, colorGreen),
			c.countField("changed", s.Changed, colorYellow),
			c.countField("unreachable", s.Unreachable, colorBrightRed),
			c.countField("failed", s.Failed, colorRed),
			c.countField("skipped", s.Skipped, colorCyan),
			c.countField("rescued", s.Rescued, colorGreen),
			c.countField("ignored", s.Ignored, colorBrightPurple))
	}

	// Real Ansible ends its output with a blank line after the recap —
	// measured with od on both a play that had hosts and one that
	// matched none, so it is unconditional rather than a side effect of
	// the rows above.
	fmt.Fprintln(c.w)
}

// resultJSON renders a result the way real Ansible inlines one on a
// fatal line: compact, keys sorted, Python's ", "/": " separators, and
// its own _ansible_* keys stripped. changed and msg are always present
// there even when empty, and an unreachable result carries
// unreachable: true.
func (c *DefaultCallback) resultJSON(r Result) string {
	// A failing no_log task is where a credential would otherwise be
	// dumped in full. Real Ansible replaces everything but `changed`
	// with this one key, wording included.
	if r.NoLog {
		out, err := template.ToJSON(map[string]any{
			"censored": "the output has been hidden due to the fact that 'no_log: true' was specified for this result",
			"changed":  r.Changed,
		}, 0)
		if err != nil {
			return "censored"
		}
		return out
	}
	fields := map[string]any{
		"changed": r.Changed,
		"msg":     r.Msg,
	}
	for k, v := range r.Extra {
		if strings.HasPrefix(k, "_ansible_") {
			continue
		}
		fields[k] = v
	}
	if r.Unreachable {
		fields["unreachable"] = true
	}
	// A result carrying _ansible_verbose_always is INDENTED here too,
	// not just on an ok line — real Ansible's _dump_results makes that
	// one decision for both. Measured: a failing non-quiet assert dumps
	// over five lines, a quiet one on a single line.
	indent := 0
	if v, ok := r.Extra[verboseAlwaysKey].(bool); ok && v {
		indent = 4
	}
	out, err := template.ToJSON(fields, indent)
	if err != nil {
		// Never worth losing the failure itself over a rendering
		// problem: fall back to the message.
		return r.Msg
	}
	return out
}

// verboseDump renders the " => {...}" a result carrying
// _ansible_verbose_always gets — what makes a debug task actually show
// what it found. Real Ansible pretty-prints it at four spaces and sorts
// the keys, and strips its own internal _ansible_* keys from the dump.
// Returns the empty string for every other result, which is why an
// ordinary command still prints one bare line.
func (c *DefaultCallback) verboseDump(r Result) string {
	// no_log: the point of the task is that its result is not printed.
	if r.NoLog {
		return ""
	}
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
	data, err := template.ToJSON(fields, 4)
	if err != nil {
		return ""
	}
	return " => " + data
}

// hostLabel is how real Ansible names the target of a result: the host
// alone, or "host -> delegate" when the task ran somewhere else.
// Deliberately NOT used for "skipping:", which real Ansible leaves
// unannotated — a skipped task never connected anywhere, so there is no
// delegation to report. Measured: a delegated task skipped by `when:`
// prints "skipping: [h1]", while the same task run prints
// "ok: [h1 -> h5]".
func hostLabel(r Result) string {
	if r.Delegate == "" || r.Delegate == r.Host {
		return r.Host
	}
	return r.Host + " -> " + r.Delegate
}

// itemLabel is the " => (item=x)" real Ansible appends to a looped
// task's result line, and the only thing that says which iteration a
// line belongs to. Empty for a task that did not loop.
//
// A no_log task's item is censored rather than printed, since the item
// is frequently the secret itself — real Ansible's own wording.
func itemLabel(r Result) string {
	if !r.Looped {
		return ""
	}
	if r.NoLog {
		return " => (item=(censored due to no_log))"
	}
	return fmt.Sprintf(" => (item=%v)", r.Item)
}

// skippedItemLabel is itemLabel plus the TRAILING SPACE real Ansible
// leaves on a skipped iteration's line — "skipping: [h1] => (item=2) ",
// where the same label on an ok or changed line has none. Measured with
// od rather than inferred: it is a quirk of real Ansible's own format
// string for that one case, and reproducing it is what makes a
// transcript comparison come out byte-identical.
func skippedItemLabel(r Result) string {
	if label := itemLabel(r); label != "" {
		return label + " "
	}
	return ""
}
