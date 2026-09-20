package playbook

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-ansible/modules"
)

// diffContext is how many unchanged lines surround each change, real
// Ansible's DIFF_CONTEXT (ansible/config/base.yml), whose default is 3.
const diffContext = 3

// noNewlineMarker is what real Ansible appends to a final line that has
// no newline, before handing the text to difflib — so the marker takes
// part in the comparison and shows up as an ordinary diff line. See
// CallbackBase._get_diff in ansible/plugins/callback/__init__.py.
// Real Ansible's COLOR_DIFF_* defaults (ansible/config/base.yml):
// add green, remove red, hunk headers cyan.
const (
	colorDiffAdd    = colorGreen
	colorDiffRemove = colorRed
	colorDiffLines  = colorCyan
)

const noNewlineMarker = "\n\\ No newline at end of file\n"

// renderDiff formats one module diff the way real Ansible's
// CallbackBase._get_diff does, including its ---/+++ header lines and
// the blank line it appends after a non-empty diff. colorize takes the
// escape code first, matching DefaultCallback.colorize, and wraps add,
// remove and hunk-header lines; pass nil to emit plain text.
//
// The skip cases come first and are mutually compatible with a
// before/after pair in real Ansible's own implementation — it appends
// each message it finds and then still diffs, rather than returning
// early — so this does the same.
func renderDiff(d modules.Diff, colorize func(code, s string) string) string {
	if colorize == nil {
		colorize = func(_, s string) string { return s }
	}
	var out strings.Builder
	if d.DstBinary {
		out.WriteString("diff skipped: destination file appears to be binary\n")
	}
	if d.SrcBinary {
		out.WriteString("diff skipped: source file appears to be binary\n")
	}
	if d.DstLarger > 0 {
		fmt.Fprintf(&out, "diff skipped: destination file size is greater than %d\n", d.DstLarger)
	}
	if d.SrcLarger > 0 {
		fmt.Fprintf(&out, "diff skipped: source file size is greater than %d\n", d.SrcLarger)
	}

	if body := unifiedDiff(d.Before, d.After); body != "" {
		fmt.Fprintf(&out, "%s\n%s\n",
			colorize(colorDiffRemove, "--- "+diffHeader("before", d.BeforeHeader)),
			colorize(colorDiffAdd, "+++ "+diffHeader("after", d.AfterHeader)))
		for _, line := range strings.SplitAfter(body, "\n") {
			if line == "" {
				continue
			}
			switch {
			case strings.HasPrefix(line, "+"):
				out.WriteString(colorize(colorDiffAdd, line))
			case strings.HasPrefix(line, "-"):
				out.WriteString(colorize(colorDiffRemove, line))
			case strings.HasPrefix(line, "@@"):
				out.WriteString(colorize(colorDiffLines, line))
			default:
				out.WriteString(line)
			}
		}
		// Real Ansible appends a bare newline once a diff has any
		// content, which is the blank line seen between the diff and
		// the task's own result line.
		out.WriteString("\n")
	}

	// A module that formatted its own diff text (real Ansible's
	// `prepared` key) is emitted verbatim.
	out.WriteString(d.Prepared)
	return out.String()
}

// diffHeader is real Ansible's "before" vs "before: <header>" choice: a
// module that named the side gets the name appended, one that did not
// gets the bare word.
func diffHeader(side, header string) string {
	if header == "" {
		return side
	}
	return side + ": " + header
}

// unifiedDiff renders before/after as the body of a unified diff — hunk
// headers and their lines, without the ---/+++ file headers, which
// renderDiff adds. It returns the empty string when the two sides are
// identical.
//
// Go has no unified diff in its standard library, so what follows is a
// port of Python's difflib, which is what real Ansible calls. A plain
// longest-common-subsequence walk is NOT enough to match it: difflib
// uses SequenceMatcher, whose recursive longest-matching-block search
// places an inserted duplicate of an existing line BEFORE that line
// where an LCS walk places it after. Both are valid diffs of the same
// length, but only one matches what real Ansible prints — a randomised
// corpus caught the difference on its twentieth case.
func unifiedDiff(before, after string) string {
	if before == after {
		return ""
	}
	a, b := diffLines(before), diffLines(after)

	var out strings.Builder
	for _, group := range groupedOpcodes(opcodes(a, b), diffContext) {
		first, last := group[0], group[len(group)-1]
		fmt.Fprintf(&out, "@@ -%s +%s @@\n",
			formatRange(first.i1, last.i2), formatRange(first.j1, last.j2))
		for _, c := range group {
			switch c.tag {
			case tagEqual:
				writeDiffLines(&out, " ", a[c.i1:c.i2])
			case tagDelete:
				writeDiffLines(&out, "-", a[c.i1:c.i2])
			case tagInsert:
				writeDiffLines(&out, "+", b[c.j1:c.j2])
			case tagReplace:
				// Every removal, THEN every addition — difflib does not
				// interleave them, even when the two runs are the same
				// length.
				writeDiffLines(&out, "-", a[c.i1:c.i2])
				writeDiffLines(&out, "+", b[c.j1:c.j2])
			}
		}
	}
	return out.String()
}

func writeDiffLines(out *strings.Builder, prefix string, lines []string) {
	for _, line := range lines {
		out.WriteString(prefix)
		out.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			out.WriteString("\n")
		}
	}
}

// diffLines is Python's str.splitlines(True) — each line KEEPS its
// terminator — followed by real Ansible's own no-newline marker on a
// final line that lacks one. Keeping the terminator is what makes "b"
// and "b\n" compare as different lines, which is how a file that gained
// a trailing newline shows up as a change at all.
func diffLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, s+noNewlineMarker)
			break
		}
		lines = append(lines, s[:i+1])
		s = s[i+1:]
	}
	return lines
}

// formatRange is difflib's _format_range_unified, which takes a start
// and a STOP rather than a length: a one-line range prints its start
// alone, and an empty range begins at the line just before it.
func formatRange(start, stop int) string {
	beginning := start + 1
	length := stop - start
	switch {
	case length == 1:
		return fmt.Sprint(beginning)
	case length == 0:
		return fmt.Sprintf("%d,0", beginning-1)
	default:
		return fmt.Sprintf("%d,%d", beginning, length)
	}
}

type opTag int

const (
	tagEqual opTag = iota
	tagReplace
	tagDelete
	tagInsert
)

// opcode is difflib's (tag, i1, i2, j1, j2): a[i1:i2] becomes b[j1:j2].
type opcode struct {
	tag            opTag
	i1, i2, j1, j2 int
}

// opcodes is SequenceMatcher.get_opcodes.
func opcodes(a, b []string) []opcode {
	var out []opcode
	i, j := 0, 0
	for _, m := range matchingBlocks(a, b) {
		switch {
		case i < m.a && j < m.b:
			out = append(out, opcode{tagReplace, i, m.a, j, m.b})
		case i < m.a:
			out = append(out, opcode{tagDelete, i, m.a, j, m.b})
		case j < m.b:
			out = append(out, opcode{tagInsert, i, m.a, j, m.b})
		}
		i, j = m.a+m.size, m.b+m.size
		if m.size > 0 {
			out = append(out, opcode{tagEqual, m.a, i, m.b, j})
		}
	}
	return out
}

// groupedOpcodes is SequenceMatcher.get_grouped_opcodes: it trims the
// leading and trailing runs of unchanged lines to n, and splits wherever
// an unchanged run is longer than 2n — which is what decides whether two
// nearby changes share one hunk or get their own.
func groupedOpcodes(codes []opcode, n int) [][]opcode {
	if len(codes) == 0 {
		codes = []opcode{{tagEqual, 0, 1, 0, 1}}
	}
	codes = append([]opcode(nil), codes...)
	if codes[0].tag == tagEqual {
		c := codes[0]
		codes[0] = opcode{c.tag, max(c.i1, c.i2-n), c.i2, max(c.j1, c.j2-n), c.j2}
	}
	if last := len(codes) - 1; codes[last].tag == tagEqual {
		c := codes[last]
		codes[last] = opcode{c.tag, c.i1, min(c.i2, c.i1+n), c.j1, min(c.j2, c.j1+n)}
	}

	var groups [][]opcode
	var group []opcode
	for _, c := range codes {
		if c.tag == tagEqual && c.i2-c.i1 > 2*n {
			group = append(group, opcode{c.tag, c.i1, min(c.i2, c.i1+n), c.j1, min(c.j2, c.j1+n)})
			groups = append(groups, group)
			group = nil
			c = opcode{c.tag, max(c.i1, c.i2-n), c.i2, max(c.j1, c.j2-n), c.j2}
		}
		group = append(group, c)
	}
	// A group that is nothing but unchanged context is not a hunk.
	if len(group) > 0 && !(len(group) == 1 && group[0].tag == tagEqual) {
		groups = append(groups, group)
	}
	return groups
}

// match is difflib's Match(a, b, size): a[a:a+size] == b[b:b+size].
type match struct{ a, b, size int }

// matchingBlocks is SequenceMatcher.get_matching_blocks: it finds the
// longest matching block, then recurses into the regions left and right
// of it, then merges any adjacent blocks the recursion produced.
func matchingBlocks(a, b []string) []match {
	b2j := map[string][]int{}
	for j, line := range b {
		b2j[line] = append(b2j[line], j)
	}
	// difflib's autojunk: in a sequence of 200 elements or more, an
	// element occurring in more than 1% of positions is dropped from the
	// index, so it cannot anchor a match. Kept because real files cross
	// that threshold — a 300-line config where a closing brace appears
	// on 40 of its lines is diffed differently with and without it.
	if n := len(b); n >= 200 {
		ntest := n/100 + 1
		for elt, idxs := range b2j {
			if len(idxs) > ntest {
				delete(b2j, elt)
			}
		}
	}

	type region struct{ alo, ahi, blo, bhi int }
	queue := []region{{0, len(a), 0, len(b)}}
	var blocks []match
	for len(queue) > 0 {
		r := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		m := findLongestMatch(a, b, b2j, r.alo, r.ahi, r.blo, r.bhi)
		if m.size == 0 {
			continue
		}
		blocks = append(blocks, m)
		if r.alo < m.a && r.blo < m.b {
			queue = append(queue, region{r.alo, m.a, r.blo, m.b})
		}
		if m.a+m.size < r.ahi && m.b+m.size < r.bhi {
			queue = append(queue, region{m.a + m.size, r.ahi, m.b + m.size, r.bhi})
		}
	}
	sort.Slice(blocks, func(x, y int) bool {
		if blocks[x].a != blocks[y].a {
			return blocks[x].a < blocks[y].a
		}
		if blocks[x].b != blocks[y].b {
			return blocks[x].b < blocks[y].b
		}
		return blocks[x].size < blocks[y].size
	})

	var merged []match
	var cur match
	for _, m := range blocks {
		if cur.a+cur.size == m.a && cur.b+cur.size == m.b {
			cur.size += m.size
			continue
		}
		if cur.size > 0 {
			merged = append(merged, cur)
		}
		cur = m
	}
	if cur.size > 0 {
		merged = append(merged, cur)
	}
	// difflib's dummy terminator, which get_opcodes relies on to emit
	// the final delete/insert after the last real match.
	return append(merged, match{len(a), len(b), 0})
}

// findLongestMatch is SequenceMatcher.find_longest_match: of all the
// longest matching blocks it returns the one starting earliest in a,
// and of those the one starting earliest in b. That tie-breaking is
// precisely what an LCS walk gets wrong.
func findLongestMatch(a, b []string, b2j map[string][]int, alo, ahi, blo, bhi int) match {
	besti, bestj, bestsize := alo, blo, 0
	// j2len[j] is the length of the longest match ending at a[i-1], b[j].
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range b2j[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}

	// Extend over elements autojunk dropped from the index: they cannot
	// anchor a match, but they can still belong to one.
	for besti > alo && bestj > blo && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}
	return match{besti, bestj, bestsize}
}
