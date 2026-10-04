package playbook

import (
	"testing"

	remoteexec "github.com/go-remoteexec/transport"
)

// The precedence here is measured against ansible-core 2.21.4, not
// chosen. Each source carries a distinguishable flag and the generated
// command line says which one survived:
//
//	ansible-playbook pb.yml -vvv            -> sudo -H -S -n -K1   (keyword)
//	  ... -e '{"ansible_become_flags":...}' -> sudo -H -S -n -K2   (var wins)
//	  ... with ANSIBLE_BECOME_FLAGS set too -> sudo -H -S -n -K2   (var still)
//	  no keyword, var and env               -> sudo -H -S -n -K2   (var over env)
//	  env alone                             -> sudo -H -S -n -K3
//
// The host var beating the playbook keyword is the counter-intuitive
// half, and it is the half a guess would get wrong.
func TestBecomeOptionPrecedence(t *testing.T) {
	const (
		fromVar     = "-K-var"
		fromMethod  = "-K-method-var"
		fromTask    = "-K-task"
		fromPlay    = "-K-play"
		fromEnv     = "-K-env"
		fromMethEnv = "-K-method-env"
	)
	for _, tc := range []struct {
		name     string
		hostVars map[string]any
		env      map[string]string
		task     string
		play     string
		want     string
	}{
		{"nothing set means say nothing", nil, nil, "", "", ""},
		{"play keyword alone", nil, nil, "", fromPlay, fromPlay},
		{"task keyword beats play", nil, nil, fromTask, fromPlay, fromTask},
		{"env alone", nil, map[string]string{"ANSIBLE_BECOME_FLAGS": fromEnv}, "", "", fromEnv},
		{"keyword beats env", nil, map[string]string{"ANSIBLE_BECOME_FLAGS": fromEnv}, "", fromPlay, fromPlay},
		{"host var beats the keyword", map[string]any{"ansible_become_flags": fromVar}, nil, fromTask, fromPlay, fromVar},
		{"host var beats env too", map[string]any{"ansible_become_flags": fromVar},
			map[string]string{"ANSIBLE_BECOME_FLAGS": fromEnv}, "", "", fromVar},
		{"the generic var wins over the method one", map[string]any{
			"ansible_become_flags": fromVar, "ansible_sudo_flags": fromMethod}, nil, "", "", fromVar},
		{"the method var is read when the generic one is absent",
			map[string]any{"ansible_sudo_flags": fromMethod}, nil, "", "", fromMethod},
		{"the method env var is read too", nil,
			map[string]string{"ANSIBLE_SUDO_FLAGS": fromMethEnv}, "", "", fromMethEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := becomeOption(tc.hostVars, remoteexec.BecomeSudo, "flags", tc.task, tc.play)
			if got != tc.want {
				t.Errorf("becomeOption = %q, want %q", got, tc.want)
			}
		})
	}
}

// The method-specific spelling follows the method, which is how each
// become plugin declares its own: ansible_sudo_exe for sudo,
// ansible_su_exe for su.
func TestBecomeOptionFollowsTheMethod(t *testing.T) {
	vars := map[string]any{"ansible_sudo_exe": "/sudo/one", "ansible_su_exe": "/su/one"}
	if got := becomeOption(vars, remoteexec.BecomeSudo, "exe", "", ""); got != "/sudo/one" {
		t.Errorf("sudo read %q", got)
	}
	if got := becomeOption(vars, remoteexec.BecomeSu, "exe", "", ""); got != "/su/one" {
		t.Errorf("su read %q", got)
	}
}

// And the whole thing reaches the transport config, which is the only
// thing that actually changes the command that runs.
func TestBecomeConfigCarriesExeAndFlags(t *testing.T) {
	play := Play{Become: true, BecomeMethod: "sudo", BecomeFlags: "-H -S", BecomeExe: "/opt/sudo"}
	cfg, enabled := becomeConfigFor(play, Task{}, nil)
	if !enabled {
		t.Fatal("become was not enabled")
	}
	if cfg.Flags != "-H -S" || cfg.Exe != "/opt/sudo" {
		t.Errorf("cfg = %+v, want the play's flags and exe", cfg)
	}
	// and an untouched play leaves both empty, so the transport applies
	// the method's own documented default rather than one invented here
	cfg, _ = becomeConfigFor(Play{Become: true, BecomeMethod: "sudo"}, Task{}, nil)
	if cfg.Flags != "" || cfg.Exe != "" {
		t.Errorf("cfg = %+v, want both empty so the transport defaults apply", cfg)
	}
}

// Until now these two keywords were in unhonouredTaskKeys: a playbook
// using them was REFUSED at parse time, which was the right call while
// the transport underneath could not carry them. It can now, so they
// have to parse -- and the refusal list must not still name them, or
// the error returns for a keyword that works.
func TestBecomeKeywordsNoLongerRefused(t *testing.T) {
	for _, k := range []string{"become_exe", "become_flags"} {
		if _, listed := unhonouredTaskKeys[k]; listed {
			t.Errorf("%s is honoured now but still in the refusal table", k)
		}
	}
	src := []byte(`
- hosts: all
  become: true
  become_exe: /opt/sudo
  become_flags: "-H -S"
  tasks:
    - name: one
      command: id -un
      become_exe: /task/sudo
      become_flags: "-i"
`)
	pb, err := Parse(src)
	if err != nil {
		t.Fatalf("a playbook using become_exe/become_flags was refused: %v", err)
	}
	if len(pb) != 1 || len(pb[0].Tasks) != 1 {
		t.Fatalf("parsed %d plays", len(pb))
	}
	p, task := pb[0], pb[0].Tasks[0]
	if p.BecomeExe != "/opt/sudo" || p.BecomeFlags != "-H -S" {
		t.Errorf("play = %+v", p)
	}
	if task.BecomeExe != "/task/sudo" || task.BecomeFlags != "-i" {
		t.Errorf("task = %+v", task)
	}
	// and the task's own values are what reaches the transport
	cfg, enabled := becomeConfigFor(p, task, nil)
	if !enabled || cfg.Exe != "/task/sudo" || cfg.Flags != "-i" {
		t.Errorf("cfg = %+v enabled=%v", cfg, enabled)
	}
}
