package playbook

import (
	"fmt"
	"strconv"
	"strings"
)

// The k=v task argument form: `debug: msg="hello"` rather than
// `debug: {msg: hello}`. This is the oldest and still the most common
// way to write an Ansible task, and this port used to drop it entirely
// — a string after the module key became _raw_params whole, so `file:
// path=/tmp/x state=touch` reached the module with no path at all.
//
// Ported from real's ansible/parsing/splitter.py (split_args, join_args,
// parse_kv) and ansible/parsing/quoting.py (is_quoted, unquote). The
// expectations in parsekv_test.go were produced by running real's own
// parse_kv over a battery of inputs, because several behaviours here are
// not what reading the code suggests: "  msg=spaced  " yields a
// _raw_params of a single SPACE, and `k=a\=b` keeps its backslash in the
// value while a bare `a\=b` loses it.

// freeformActions are real's MODULE_REQUIRE_ARGS: the modules whose
// argument string is a command line rather than options, so a k=v pair
// in it is part of the command unless it is one of the few options the
// command family itself understands.
var freeformActions = map[string]bool{
	"command": true, "raw": true, "script": true, "shell": true,
	"win_command": true, "win_shell": true,
}

// commandOptions are the keys real still reads as options even for a
// freeform action — everything else stays in the command line.
var commandOptions = map[string]bool{
	"creates": true, "removes": true, "chdir": true, "executable": true,
	"warn": true, "stdin": true, "stdin_add_newline": true,
	"strip_empty_ends": true,
}

// rawParamModules are real's RAW_PARAM_MODULES: the modules allowed to
// receive _raw_params at all. Any other module given raw params is an
// error, not a silently ignored argument.
var rawParamModules = map[string]bool{
	"add_host": true, "command": true, "group_by": true,
	"import_role": true, "import_tasks": true, "include_role": true,
	"include_tasks": true, "include_vars": true, "meta": true,
	"raw": true, "script": true, "set_fact": true, "shell": true,
	"win_command": true, "win_shell": true,
}

// isQuoted reports whether data begins and ends with the same quote
// character, the unescaped one — real's quoting.is_quoted.
func isQuoted(data string) bool {
	return len(data) > 1 && data[0] == data[len(data)-1] &&
		(data[0] == '"' || data[0] == '\'') && data[len(data)-2] != '\\'
}

// unquote strips one matching pair of surrounding quotes.
func unquote(data string) string {
	if isQuoted(data) {
		return data[1 : len(data)-1]
	}
	return data
}

// getQuoteState walks a token and returns the quote character left open
// at the end of it, or 0 — real's _get_quote_state. A quote preceded by
// a backslash does not count.
func getQuoteState(token string, quoteChar byte) byte {
	for i := 0; i < len(token); i++ {
		c := token[i]
		if c != '"' && c != '\'' {
			continue
		}
		if i > 0 && token[i-1] == '\\' {
			continue
		}
		if quoteChar != 0 {
			if c == quoteChar {
				quoteChar = 0
			}
		} else {
			quoteChar = c
		}
	}
	return quoteChar
}

// countJinja2Blocks adjusts depth by the imbalance of open/close tokens
// in this token, never below zero — real's _count_jinja2_blocks. Note it
// only adjusts when the counts DIFFER, so a token holding a complete
// "{{ x }}" leaves the depth alone.
func countJinja2Blocks(token string, curDepth int, open, close string) int {
	numOpen := strings.Count(token, open)
	numClose := strings.Count(token, close)
	if numOpen != numClose {
		curDepth += numOpen - numClose
		if curDepth < 0 {
			curDepth = 0
		}
	}
	return curDepth
}

// splitArgs splits an argument string on whitespace, reassembling what
// was split inside quotes or inside a Jinja block — real's split_args.
// It preserves runs of spaces and newlines, because join_args puts the
// raw params back together from these tokens.
func splitArgs(args string) ([]string, error) {
	if args == "" {
		return nil, nil
	}
	var params []string
	items := strings.Split(args, "\n")

	var quoteChar byte
	insideQuotes := false
	printDepth, blockDepth, commentDepth := 0, 0, 0

	for itemIdx, item := range items {
		tokens := strings.Split(item, " ")
		lineContinuation := false
		for idx, token := range tokens {
			// Empty entries mean subsequent spaces, held on to so the
			// original spacing can be reconstructed.
			if len(token) == 0 && idx != 0 {
				if len(params) == 0 {
					params = append(params, "")
				}
				params[len(params)-1] += " "
				continue
			}
			if token == `\` && !insideQuotes {
				lineContinuation = true
				continue
			}

			wasInsideQuotes := insideQuotes
			quoteChar = getQuoteState(token, quoteChar)
			insideQuotes = quoteChar != 0

			appended := false
			inBlock := printDepth > 0 || blockDepth > 0 || commentDepth > 0
			switch {
			case insideQuotes && !wasInsideQuotes && !inBlock:
				params = append(params, token)
				appended = true
			case inBlock || insideQuotes || wasInsideQuotes:
				if idx == 0 && wasInsideQuotes {
					params[len(params)-1] += token
				} else {
					spacer := ""
					if idx > 0 {
						spacer = " "
					}
					params[len(params)-1] += spacer + token
				}
				appended = true
			}

			for _, d := range []struct {
				cur          *int
				open, close_ string
			}{
				{&printDepth, "{{", "}}"},
				{&blockDepth, "{%", "%}"},
				{&commentDepth, "{#", "#}"},
			} {
				prev := *d.cur
				*d.cur = countJinja2Blocks(token, *d.cur, d.open, d.close_)
				if *d.cur != prev && !appended {
					params = append(params, token)
					appended = true
				}
			}

			if printDepth == 0 && blockDepth == 0 && commentDepth == 0 &&
				!insideQuotes && !appended && token != "" {
				params = append(params, token)
			}
		}
		// Put the newline back, so the original structure survives.
		if len(items) > 1 && itemIdx != len(items)-1 && !lineContinuation {
			if len(params) == 0 {
				params = append(params, "")
			}
			params[len(params)-1] += "\n"
		}
	}

	if printDepth > 0 || blockDepth > 0 || commentDepth > 0 || insideQuotes {
		return nil, fmt.Errorf("failed at splitting arguments, either an unbalanced jinja2 block or quotes: %s", args)
	}
	return params, nil
}

// joinArgs reassembles what splitArgs produced, retaining the original
// newlines and whitespace — real's join_args. A part following one that
// already ends in a newline is NOT given a space.
func joinArgs(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		if b.Len() == 0 || strings.HasSuffix(b.String(), "\n") {
			b.WriteString(p)
		} else {
			b.WriteString(" " + p)
		}
	}
	return b.String()
}

// decodeEscapes expands the escape sequences real's _decode_escapes
// recognises, and ONLY those: a backslash before anything else — `\=`
// notably — is left alone, which is why an escaped equals survives into
// a value.
func decodeEscapes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		c := s[i+1]
		if n, r, ok := decodeNumericEscape(s, i); ok {
			b.WriteRune(r)
			i += n
			continue
		}
		if r, ok := map[byte]rune{
			'\\': '\\', '\'': '\'', '"': '"', 'a': 7, 'b': 8,
			'f': 12, 'n': 10, 'r': 13, 't': 9, 'v': 11,
		}[c]; ok {
			b.WriteRune(r)
			i += 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// decodeNumericEscape reads \xHH, \uHHHH, \UHHHHHHHH or \NNN (octal) at
// s[i], returning how many bytes it consumed and the rune it means.
func decodeNumericEscape(s string, i int) (int, rune, bool) {
	c := s[i+1]
	hexLen := map[byte]int{'x': 2, 'u': 4, 'U': 8}[c]
	if hexLen > 0 {
		if i+2+hexLen > len(s) {
			return 0, 0, false
		}
		v, err := strconv.ParseUint(s[i+2:i+2+hexLen], 16, 32)
		if err != nil {
			return 0, 0, false
		}
		return 2 + hexLen, rune(v), true
	}
	if c >= '0' && c <= '7' {
		end := i + 2
		for end < len(s) && end < i+5 && s[end] >= '0' && s[end] <= '7' {
			end++
		}
		v, err := strconv.ParseUint(s[i+1:end], 8, 32)
		if err != nil {
			return 0, 0, false
		}
		return end - i, rune(v), true
	}
	return 0, 0, false
}

// parseKV converts a k=v argument string into a map, mirroring real's
// parse_kv. With checkRaw set — which real does for a freeform action —
// a pair whose key is not one of the command family's own options stays
// in the command line instead of becoming an option.
func parseKV(args string, checkRaw bool) (map[string]any, error) {
	options := map[string]any{}
	vargs, err := splitArgs(args)
	if err != nil {
		return nil, err
	}
	var rawParams []string
	for _, origX := range vargs {
		x := decodeEscapes(origX)
		if !strings.Contains(x, "=") {
			rawParams = append(rawParams, origX)
			continue
		}
		// The first '=' at a position above zero that is not escaped.
		pos, found := -1, false
		for p := 1; p < len(x); p++ {
			if x[p] != '=' {
				continue
			}
			if x[p-1] != '\\' {
				pos, found = p, true
				break
			}
		}
		if !found {
			// Only escaped equals signs: this is not a pair at all.
			rawParams = append(rawParams, strings.ReplaceAll(x, `\=`, "="))
			continue
		}
		k, v := x[:pos], x[pos+1:]
		if checkRaw && !commandOptions[strings.TrimSpace(k)] {
			rawParams = append(rawParams, origX)
			continue
		}
		options[strings.TrimSpace(k)] = unquote(strings.TrimSpace(v))
	}
	if len(rawParams) > 0 {
		options["_raw_params"] = joinArgs(rawParams)
	}
	return options, nil
}
