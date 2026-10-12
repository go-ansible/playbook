package playbook

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	remoteexec "github.com/go-remoteexec/transport"
)

// sshArgs is one OpenSSH argument string and where it came from, so an
// error or a warning can name the place a person has to go and edit.
type sshArgs struct {
	source string
	value  string
}

// sshArgSettings resolves the two argument strings real's ssh
// connection plugin splices into its ssh command line, each on the
// precedence that plugin declares for it:
//
//	host var  >  ANSIBLE_* environment  >  ansible.cfg [ssh_connection]
//
// ssh_common_args comes first and ssh_extra_args second, because that
// is the order the plugin's _build_command adds them to the command
// line -- and with OpenSSH, order decides which of two values wins.
//
// The --ssh-common-args / --ssh-extra-args command-line options are not
// read here: they belong to go-ansible/cli, which can set the host var.
//
// Each host var is read with its name spelled out, not built from a
// table, because TestConnectionVarsCoversDefaultConnect finds the
// variables a connection depends on by reading these calls.
func sshArgSettings(hostVars map[string]any) []sshArgs {
	var out []sshArgs
	for _, s := range []struct{ hostVal, hostVar, env, ini string }{
		{strVar(hostVars, "ansible_ssh_common_args", ""), "ansible_ssh_common_args", "ANSIBLE_SSH_COMMON_ARGS", "ssh_common_args"},
		{strVar(hostVars, "ansible_ssh_extra_args", ""), "ansible_ssh_extra_args", "ANSIBLE_SSH_EXTRA_ARGS", "ssh_extra_args"},
	} {
		if s.hostVal != "" {
			out = append(out, sshArgs{s.hostVar, s.hostVal})
			continue
		}
		if v := os.Getenv(s.env); v != "" {
			out = append(out, sshArgs{s.env, v})
			continue
		}
		if v, ok := configFileValueIn("ssh_connection", s.ini); ok && v != "" {
			out = append(out, sshArgs{s.ini + " in ansible.cfg [ssh_connection]", v})
		}
	}
	return out
}

// sshProxyJumpFor reads the jump hosts out of ansible_ssh_common_args
// and ansible_ssh_extra_args (and their environment and ansible.cfg
// spellings -- see sshArgSettings): `-o ProxyJump=spec`, `-oProxyJump=spec`,
// `-o "ProxyJump spec"`, `-J spec` and `-Jspec`.
//
// This is the one OpenSSH option worth translating rather than
// refusing. It is how a host behind a bastion is reached in practice,
// it says nothing the Go client cannot do natively, and without it an
// inventory written for real Ansible cannot reach those hosts at all.
//
// WHICH VALUE WINS. ssh_config(5): "for each parameter, the first
// obtained value will be used". So the first ProxyJump or -J found --
// common args before extra args, left to right -- is the one used, and
// any later one is reported and ignored. `none` counts as a value: it
// is how OpenSSH switches an inherited ProxyJump off.
//
// EVERYTHING ELSE in these strings is ignored, as it always was here,
// but no longer silently: each distinct ignored argument produces one
// warning. ServerAliveInterval or StrictHostKeyChecking in a common
// args string changes what real does, and a run that quietly does
// something else is the outcome this port tries hardest to avoid.
//
// A string that cannot be split (an unclosed quote) or a ProxyJump spec
// that cannot be parsed is an ERROR, not a warning. Real fails there
// too -- shlex raises, or ssh refuses the spec -- and connecting
// directly to a host the inventory says to reach through a bastion
// would be the wrong host, or a firewall log entry, or both.
//
// The hops this returns name only Host, Port and User. Everything else
// is left to the transport's own inheritance (see remoteexec.SSHConfig's
// ProxyJump), which gives a hop the target's credentials and the
// target's host-key checking; see DefaultConnect for how that compares
// with OpenSSH.
func sshProxyJumpFor(hostVars map[string]any) (hops []remoteexec.SSHConfig, warnings []string, err error) {
	found := false
	for _, a := range sshArgSettings(hostVars) {
		words, err := shlexSplit(a.value)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", a.source, err)
		}
		opts, err := parseSSHOptions(words)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", a.source, err)
		}
		for _, o := range opts {
			if o.proxyJump == nil {
				warnings = append(warnings, fmt.Sprintf(
					"%s: %s is not supported by this port's SSH client and is ignored "+
						"(only ProxyJump / -J is honoured from these arguments)", a.source, o.text))
				continue
			}
			if found {
				warnings = append(warnings, fmt.Sprintf(
					"%s: %s is ignored: a ProxyJump was already given, and as in OpenSSH "+
						"the first one wins", a.source, o.text))
				continue
			}
			found = true
			if hops, err = parseProxyJump(*o.proxyJump); err != nil {
				return nil, nil, fmt.Errorf("%s: %s: %w", a.source, o.text, err)
			}
		}
	}
	return hops, warnings, nil
}

// sshOption is one option from an ssh command line: its text as a
// person would recognise it, and, for ProxyJump/-J, the jump spec.
type sshOption struct {
	text      string
	proxyJump *string
}

// sshOptsWithArg are the ssh(1) options that take an argument -- from
// OpenSSH's own getopt string, "1246ab:c:e:fgi:kl:m:no:p:qstvx"
// "AB:CD:E:F:GI:J:KL:MNO:P:Q:R:S:TVw:W:XYy". Knowing them matters even
// for options this port ignores: `-i -J` is a key file NAMED "-J", not
// a jump host, and `-p 2222` must be warned about as one option rather
// than as a flag followed by a stray word.
const sshOptsWithArg = "bceilmopBDEFIJLOPQRSwW"

// parseSSHOptions walks words the way ssh's getopt does: options may be
// clustered (-tt, -4C), and an option's argument is either the rest of
// its own word (-oProxyJump=x) or the next word (-o ProxyJump=x).
func parseSSHOptions(words []string) ([]sshOption, error) {
	var out []sshOption
	for i := 0; i < len(words); i++ {
		w := words[i]
		if len(w) < 2 || w[0] != '-' || w == "--" {
			out = append(out, sshOption{text: strconv.Quote(w)})
			continue
		}
		for j := 1; j < len(w); j++ {
			c := w[j]
			if !strings.ContainsRune(sshOptsWithArg, rune(c)) {
				out = append(out, sshOption{text: "-" + string(c)})
				continue
			}
			arg := w[j+1:]
			if arg == "" {
				if i+1 >= len(words) {
					return nil, fmt.Errorf("option -%c needs an argument", c)
				}
				i++
				arg = words[i]
			}
			out = append(out, sshOptionFor(c, arg))
			break
		}
	}
	return out, nil
}

func sshOptionFor(c byte, arg string) sshOption {
	text := fmt.Sprintf("-%c %s", c, arg)
	switch c {
	case 'J':
		return sshOption{text: text, proxyJump: &arg}
	case 'o':
		// ssh_config syntax: "Key=Value" or "Key Value", the keyword
		// case-insensitive, the value optionally in double quotes.
		key, value := arg, ""
		if k := strings.IndexAny(arg, "= \t"); k >= 0 {
			key, value = arg[:k], arg[k+1:]
			value = strings.TrimLeft(strings.TrimSpace(value), "=")
			value = strings.TrimSpace(value)
		}
		if strings.EqualFold(strings.TrimSpace(key), "ProxyJump") {
			if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
				value = value[1 : len(value)-1]
			}
			return sshOption{text: text, proxyJump: &value}
		}
	}
	return sshOption{text: text}
}

// parseProxyJump parses OpenSSH's jump spec: `none`, or a comma list of
// `[user@]host[:port]` or `ssh://[user@]host[:port]`, with an IPv6
// address in brackets. The user is everything before the LAST '@', as
// OpenSSH's parse_user_host_port splits it.
//
// A hop without a user gets the LOCAL user, and a hop without a port
// gets 22. That is OpenSSH's own fallback, minus ~/.ssh/config, which
// this port does not read (a stated gap -- a `Host bastion` block's
// User, Port or IdentityFile is honoured by real and not here). It is
// deliberately NOT ansible_user or ansible_port: those are the TARGET's,
// and real passes them to the outer ssh only, which does not hand them
// to the jump host.
func parseProxyJump(spec string) ([]remoteexec.SSHConfig, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("empty ProxyJump")
	}
	if spec == "none" {
		return nil, nil
	}
	var hops []remoteexec.SSHConfig
	for _, h := range strings.Split(spec, ",") {
		hop, err := parseJumpHost(strings.TrimSpace(h))
		if err != nil {
			return nil, err
		}
		hops = append(hops, hop)
	}
	return hops, nil
}

func parseJumpHost(s string) (remoteexec.SSHConfig, error) {
	orig := s
	bad := func(why string) (remoteexec.SSHConfig, error) {
		return remoteexec.SSHConfig{}, fmt.Errorf("jump host %q: %s", orig, why)
	}
	if s == "" {
		return bad("empty entry in the list")
	}
	if rest, ok := strings.CutPrefix(s, "ssh://"); ok {
		s = strings.TrimSuffix(rest, "/")
		if strings.Contains(s, "/") {
			return bad("an ssh:// URI may not have a path")
		}
	}
	hop := remoteexec.SSHConfig{User: currentUser(), Port: 22}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		if at == 0 {
			return bad("empty user")
		}
		hop.User, s = s[:at], s[at+1:]
	}
	portStr := ""
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return bad("unclosed '['")
		}
		hop.Host, s = s[1:end], s[end+1:]
		if s != "" {
			if s[0] != ':' {
				return bad("unexpected text after ']'")
			}
			portStr = s[1:]
			if portStr == "" {
				return bad("empty port")
			}
		}
	} else if k := strings.Index(s, ":"); k >= 0 {
		hop.Host, portStr = s[:k], s[k+1:]
		if portStr == "" {
			return bad("empty port")
		}
	} else {
		hop.Host = s
	}
	if hop.Host == "" {
		return bad("empty host")
	}
	if portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil || p < 1 || p > 65535 {
			return bad("invalid port " + strconv.Quote(portStr))
		}
		hop.Port = p
	}
	return hop, nil
}

// shlexSplit splits s as Python's shlex.split does in its default POSIX
// mode -- which is what real's ssh plugin calls (_split_ssh_args) on
// these strings, before stripping each word and dropping empty ones.
// parsekv.go's splitArgs is NOT that: it is parse_kv's splitter, keeps
// quotes in its words and knows about Jinja2 blocks, none of which
// shlex does.
//
// The POSIX rules, as shlex implements them:
//
//   - whitespace separates words; quotes join onto the word around them
//     (a'b'c is abc);
//   - inside single quotes everything is literal;
//   - inside double quotes a backslash escapes only '"' and '\' and is
//     otherwise kept;
//   - outside quotes a backslash escapes any character;
//   - an unclosed quote, or a backslash at the very end, is an error.
func shlexSplit(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune // 0, '\'' or '"'
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			if quote == '"' && r != '"' && r != '\\' {
				cur.WriteRune('\\')
			}
			cur.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if escaped {
		return nil, fmt.Errorf("no escaped character after a trailing backslash")
	}
	if quote != 0 {
		return nil, fmt.Errorf("no closing quotation")
	}
	if inWord {
		words = append(words, cur.String())
	}
	// _split_ssh_args: [x.strip() for x in shlex.split(argstring) if x.strip()]
	out := words[:0]
	for _, w := range words {
		if w = strings.TrimSpace(w); w != "" {
			out = append(out, w)
		}
	}
	return out, nil
}
