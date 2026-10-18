package playbook

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	remoteexec "github.com/go-remoteexec/transport"
	"golang.org/x/crypto/ssh"
)

// isolateSSHArgs makes the environment and ansible.cfg say nothing, so
// a developer's own ANSIBLE_SSH_COMMON_ARGS cannot decide a test.
func isolateSSHArgs(t *testing.T) {
	t.Helper()
	t.Setenv("ANSIBLE_SSH_COMMON_ARGS", "")
	t.Setenv("ANSIBLE_SSH_EXTRA_ARGS", "")
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "no-such-ansible.cfg"))
}

// The expectations are Python's shlex.split in its default POSIX mode,
// which is what real's _split_ssh_args calls -- followed by its own
// strip-and-drop-empty pass.
func TestShlexSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"-o ProxyJump=a", []string{"-o", "ProxyJump=a"}},
		{"  -J\tbastion\n", []string{"-J", "bastion"}},
		{`-o "ProxyJump a@b"`, []string{"-o", "ProxyJump a@b"}},
		{`-o 'ProxyJump a@b'`, []string{"-o", "ProxyJump a@b"}},
		{`a'b c'd`, []string{"ab cd"}},
		{`a\ b`, []string{"a b"}},
		{`'a\b'`, []string{`a\b`}},   // literal inside single quotes
		{`"a\"b"`, []string{`a"b`}},  // \" escapes inside double quotes
		{`"a\\b"`, []string{`a\b`}},  // so does \\ ...
		{`"a\nb"`, []string{`a\nb`}}, // ... and nothing else: the backslash stays
		{`'' "" x`, []string{"x"}},   // empty words dropped, as real strips them
		{`" padded "`, []string{"padded"}},
	}
	for _, tc := range cases {
		got, err := shlexSplit(tc.in)
		if err != nil {
			t.Errorf("shlexSplit(%q): %v", tc.in, err)
			continue
		}
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("shlexSplit(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, in := range []string{`-o "ProxyJump a`, `'open`, `trailing\`} {
		if got, err := shlexSplit(in); err == nil {
			t.Errorf("shlexSplit(%q) = %q, want an error, as shlex raises", in, got)
		}
	}
}

func TestParseProxyJump(t *testing.T) {
	me := currentUser()
	hop := func(user, host string, port int) remoteexec.SSHConfig {
		return remoteexec.SSHConfig{User: user, Host: host, Port: port}
	}
	cases := []struct {
		spec string
		want []remoteexec.SSHConfig
	}{
		{"bastion", []remoteexec.SSHConfig{hop(me, "bastion", 22)}},
		{"ops@bastion", []remoteexec.SSHConfig{hop("ops", "bastion", 22)}},
		{"ops@bastion:2222", []remoteexec.SSHConfig{hop("ops", "bastion", 2222)}},
		{"bastion:2222", []remoteexec.SSHConfig{hop(me, "bastion", 2222)}},
		{"a@one,two:2200, b@three", []remoteexec.SSHConfig{hop("a", "one", 22), hop(me, "two", 2200), hop("b", "three", 22)}},
		{"ops@[2001:db8::1]:2222", []remoteexec.SSHConfig{hop("ops", "2001:db8::1", 2222)}},
		{"[2001:db8::1]", []remoteexec.SSHConfig{hop(me, "2001:db8::1", 22)}},
		{"ssh://ops@bastion:2222", []remoteexec.SSHConfig{hop("ops", "bastion", 2222)}},
		{"ssh://bastion/", []remoteexec.SSHConfig{hop(me, "bastion", 22)}},
		// The user is everything before the LAST '@'.
		{"ops@corp.example@bastion", []remoteexec.SSHConfig{hop("ops@corp.example", "bastion", 22)}},
		{"none", nil},
	}
	for _, tc := range cases {
		got, err := parseProxyJump(tc.spec)
		if err != nil {
			t.Errorf("parseProxyJump(%q): %v", tc.spec, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseProxyJump(%q) = %+v, want %+v", tc.spec, got, tc.want)
		}
	}
	for _, bad := range []string{"", "a,,b", "@host", "user@", "host:", "host:0", "host:65536", "host:ssh",
		"[::1", "[::1]x", "[::1]:", "ssh://host/path", ":22"} {
		if got, err := parseProxyJump(bad); err == nil {
			t.Errorf("parseProxyJump(%q) = %+v, want an error", bad, got)
		}
	}
}

func TestSSHProxyJumpForSpellings(t *testing.T) {
	isolateSSHArgs(t)
	want := []remoteexec.SSHConfig{{User: "ops", Host: "bastion", Port: 2222}}
	for _, args := range []string{
		"-o ProxyJump=ops@bastion:2222",
		"-oProxyJump=ops@bastion:2222",
		`-o "ProxyJump ops@bastion:2222"`,
		`-o "ProxyJump = ops@bastion:2222"`,
		"-o proxyjump=ops@bastion:2222", // ssh_config keywords are case-insensitive
		`-o 'ProxyJump="ops@bastion:2222"'`,
		"-J ops@bastion:2222",
		"-Jops@bastion:2222",
		"-C -J ops@bastion:2222", // after another flag ...
		"-qJ ops@bastion:2222",   // ... and clustered with one
	} {
		hops, warnings, err := sshProxyJumpFor(map[string]any{"ansible_ssh_common_args": args})
		if err != nil {
			t.Errorf("%s: %v", args, err)
			continue
		}
		if !reflect.DeepEqual(hops, want) {
			t.Errorf("%s: hops = %+v, want %+v", args, hops, want)
		}
		for _, w := range warnings {
			if strings.Contains(w, "ProxyJump") && !strings.Contains(w, "only ProxyJump") {
				t.Errorf("%s: ProxyJump itself was warned about: %s", args, w)
			}
		}
	}
}

func TestSSHProxyJumpForWarnsAboutEverythingElse(t *testing.T) {
	isolateSSHArgs(t)
	hops, warnings, err := sshProxyJumpFor(map[string]any{
		"ansible_ssh_common_args": "-o StrictHostKeyChecking=no -o ProxyJump=bastion -tt",
		"ansible_ssh_extra_args":  "-i -J -p 2222 stray",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 1 || hops[0].Host != "bastion" {
		t.Errorf("hops = %+v, want bastion alone", hops)
	}
	wantIgnored := []string{
		"ansible_ssh_common_args: -o StrictHostKeyChecking=no is not supported",
		"ansible_ssh_common_args: -t is not supported", // once per letter; the Warner dedups the repeat
		// `-i -J` is a key file named "-J": the argument of -i, never
		// a jump host. Getting that wrong would read the next word as
		// a bastion.
		"ansible_ssh_extra_args: -i -J is not supported",
		"ansible_ssh_extra_args: -p 2222 is not supported",
		`ansible_ssh_extra_args: "stray" is not supported`,
	}
	joined := strings.Join(warnings, "\n")
	for _, w := range wantIgnored {
		if !strings.Contains(joined, w) {
			t.Errorf("no warning containing %q; got:\n%s", w, joined)
		}
	}
	if len(warnings) != 6 {
		t.Errorf("got %d warnings, want 6 (-t twice):\n%s", len(warnings), joined)
	}
}

// ssh_config(5): the first obtained value is used. Common args come
// before extra args on real's command line, so the common one wins --
// and `none` is a value like any other.
func TestSSHProxyJumpForFirstValueWins(t *testing.T) {
	isolateSSHArgs(t)
	cases := []struct {
		common, extra string
		wantHost      string // "" = no jump host
	}{
		{"-J first", "-J second", "first"},
		{"-o ProxyJump=first -J second", "", "first"},
		{"-J none", "-J second", ""},
		{"", "-o ProxyJump=second", "second"},
	}
	for _, tc := range cases {
		hops, warnings, err := sshProxyJumpFor(map[string]any{
			"ansible_ssh_common_args": tc.common, "ansible_ssh_extra_args": tc.extra,
		})
		if err != nil {
			t.Errorf("%q / %q: %v", tc.common, tc.extra, err)
			continue
		}
		got := ""
		if len(hops) > 0 {
			got = hops[0].Host
		}
		if got != tc.wantHost {
			t.Errorf("%q / %q: jump host %q, want %q", tc.common, tc.extra, got, tc.wantHost)
		}
		if strings.Contains(tc.common+tc.extra, "second") && tc.wantHost != "second" {
			if len(warnings) != 1 || !strings.Contains(warnings[0], "the first one wins") {
				t.Errorf("%q / %q: the losing ProxyJump should be reported once; got %q", tc.common, tc.extra, warnings)
			}
		}
	}
}

func TestSSHProxyJumpForErrors(t *testing.T) {
	isolateSSHArgs(t)
	cases := []struct{ args, want string }{
		{`-o "ProxyJump bastion`, "ansible_ssh_common_args: no closing quotation"},
		{"-J", "ansible_ssh_common_args: option -J needs an argument"},
		{"-J bastion:notaport", `ansible_ssh_common_args: -J bastion:notaport: jump host "bastion:notaport": invalid port`},
		{"-o ProxyJump=", "empty ProxyJump"},
	}
	for _, tc := range cases {
		_, _, err := sshProxyJumpFor(map[string]any{"ansible_ssh_common_args": tc.args})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want it to contain %q", tc.args, err, tc.want)
		}
	}
}

// host var > ANSIBLE_SSH_* > ansible.cfg [ssh_connection], per setting.
func TestSSHArgSettingsPrecedence(t *testing.T) {
	isolateSSHArgs(t)
	cfg := filepath.Join(t.TempDir(), "ansible.cfg")
	if err := os.WriteFile(cfg, []byte("[defaults]\nssh_common_args = -J wrong-section\n"+
		"[ssh_connection]\nssh_common_args = -J from-ini\nssh_extra_args = -J extra-ini\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANSIBLE_CONFIG", cfg)

	got := sshArgSettings(map[string]any{})
	want := []sshArgs{
		{"ssh_common_args in ansible.cfg [ssh_connection]", "-J from-ini"},
		{"ssh_extra_args in ansible.cfg [ssh_connection]", "-J extra-ini"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ini only: %+v, want %+v", got, want)
	}

	t.Setenv("ANSIBLE_SSH_COMMON_ARGS", "-J from-env")
	got = sshArgSettings(map[string]any{})
	want[0] = sshArgs{"ANSIBLE_SSH_COMMON_ARGS", "-J from-env"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env over ini: %+v, want %+v", got, want)
	}

	got = sshArgSettings(map[string]any{"ansible_ssh_common_args": "-J from-var"})
	want[0] = sshArgs{"ansible_ssh_common_args", "-J from-var"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("host var over env: %+v, want %+v", got, want)
	}
}

func TestEngineConnectCarriesTheWarner(t *testing.T) {
	var buf bytes.Buffer
	e := &Engine{Warn: NewWarner(&buf)}
	var seen Warner
	e.Connect = func(ctx context.Context, _ string, _ map[string]any) (remoteexec.Connection, error) {
		seen = warnerFrom(ctx)
		return nil, fmt.Errorf("not dialling")
	}
	e.connect(context.Background(), "h", nil)
	if seen == nil {
		t.Fatal("the Connector found no Warner in its ctx")
	}
	seen("hello")
	if buf.String() != "[WARNING]: hello\n" {
		t.Errorf("warning went to %q, not the Engine's Warner", buf.String())
	}

	// A nil Warn stays silent rather than panicking.
	e.Warn, seen = nil, nil
	e.connect(context.Background(), "h", nil)
	if seen != nil {
		t.Error("a nil Engine.Warn produced a Warner")
	}
}

// --- end to end, through DefaultConnect and real SSH ------------------

// proxyTestServer is an in-process sshd that answers every exec with
// its own name and the command, and forwards direct-tcpip -- enough to
// prove which host a command really ran on, and that it got there
// through the bastion.
type proxyTestServer struct {
	name string
	host string
	port int

	mu        sync.Mutex
	forwarded []string
}

func startProxyTestServer(t *testing.T, name, user, pass string) *proxyTestServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if c.User() == user && string(p) == pass {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	config.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &proxyTestServer{name: name}
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	s.host = h
	s.port, _ = strconv.Atoi(p)
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(nc, config)
		}
	}()
	return s
}

func (s *proxyTestServer) serve(nc net.Conn, config *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, config)
	if err != nil {
		nc.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		switch nch.ChannelType() {
		case "session":
			ch, chReqs, err := nch.Accept()
			if err != nil {
				return
			}
			go func() {
				defer ch.Close()
				for req := range chReqs {
					if req.Type != "exec" {
						req.Reply(false, nil)
						continue
					}
					var payload struct{ Command string }
					ssh.Unmarshal(req.Payload, &payload)
					req.Reply(true, nil)
					fmt.Fprintf(ch, "%s ran: %s\n", s.name, payload.Command)
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					return
				}
			}()
		case "direct-tcpip":
			var p struct {
				DestAddr   string
				DestPort   uint32
				OriginAddr string
				OriginPort uint32
			}
			ssh.Unmarshal(nch.ExtraData(), &p)
			dest := net.JoinHostPort(p.DestAddr, strconv.Itoa(int(p.DestPort)))
			s.mu.Lock()
			s.forwarded = append(s.forwarded, dest)
			s.mu.Unlock()
			out, err := net.Dial("tcp", dest)
			if err != nil {
				nch.Reject(ssh.ConnectionFailed, err.Error())
				continue
			}
			ch, chReqs, err := nch.Accept()
			if err != nil {
				out.Close()
				continue
			}
			go ssh.DiscardRequests(chReqs)
			go func() {
				done := make(chan struct{}, 2)
				go func() { io.Copy(out, ch); done <- struct{}{} }()
				go func() { io.Copy(ch, out); done <- struct{}{} }()
				<-done
				out.Close()
				ch.Close()
			}()
		default:
			nch.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

// ⭐ THE INVENTORY REAL ANSIBLE USERS ALREADY HAVE. A target behind a
// bastion, reached with nothing but ansible_ssh_common_args -- the
// command must run on the TARGET, and the bastion must be the one that
// connected to it. The ignored -o option is reported exactly once over
// two connections, through the Warner the engine puts in ctx.
func TestDefaultConnectThroughAProxyJump(t *testing.T) {
	isolateSSHArgs(t)
	bastion := startProxyTestServer(t, "bastion", "ops", "secret")
	target := startProxyTestServer(t, "target", "deploy", "secret")

	var warnings bytes.Buffer
	ctx := withWarner(context.Background(), NewWarner(&warnings))
	hostVars := map[string]any{
		"ansible_host":              target.host,
		"ansible_port":              target.port,
		"ansible_user":              "deploy",
		"ansible_password":          "secret",
		"ansible_host_key_checking": false,
		"ansible_ssh_common_args": fmt.Sprintf("-o ProxyJump=ops@%s:%d -o ServerAliveInterval=30",
			bastion.host, bastion.port),
	}
	for i := 0; i < 2; i++ {
		conn, err := DefaultConnect(ctx, "behind-bastion", hostVars)
		if err != nil {
			t.Fatalf("DefaultConnect through the bastion: %v", err)
		}
		res, err := conn.Exec(context.Background(), "uptime", nil)
		conn.Close()
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if got := strings.TrimSpace(res.Stdout); got != "target ran: uptime" {
			t.Fatalf("stdout = %q, want the TARGET to have run it", got)
		}
	}
	targetAddr := net.JoinHostPort(target.host, strconv.Itoa(target.port))
	bastion.mu.Lock()
	forwarded := append([]string(nil), bastion.forwarded...)
	bastion.mu.Unlock()
	if len(forwarded) != 2 || forwarded[0] != targetAddr || forwarded[1] != targetAddr {
		t.Errorf("bastion forwarded to %v, want the target %s twice", forwarded, targetAddr)
	}
	want := "[WARNING]: ansible_ssh_common_args: -o ServerAliveInterval=30 is not supported by this " +
		"port's SSH client and is ignored (only ProxyJump / -J is honoured from these arguments)\n"
	if warnings.String() != want {
		t.Errorf("warnings =\n%q\nwant exactly one line:\n%q", warnings.String(), want)
	}
}

// A spec that cannot be parsed must not fall back to dialling the
// target directly: that would be the wrong route, silently.
func TestDefaultConnectRefusesABadProxyJump(t *testing.T) {
	isolateSSHArgs(t)
	_, err := DefaultConnect(context.Background(), "behind-bastion", map[string]any{
		"ansible_host":            "192.0.2.1",
		"ansible_password":        "x",
		"ansible_ssh_common_args": "-J ops@bastion:http",
	})
	if err == nil || !strings.Contains(err.Error(), "connecting to behind-bastion: ansible_ssh_common_args: -J ops@bastion:http") {
		t.Fatalf("err = %v, want the bad spec named, before any dial", err)
	}
}
