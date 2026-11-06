package playbook

import (
	"context"
	"strings"
	"testing"

	"github.com/go-ansible/modules"
	remoteexec "github.com/go-remoteexec/transport"
)

// runCapturing runs src with redfish_info and a control module replaced by
// fakes that record the args the ENGINE hands them, so what is asserted is
// the wiring, not the module.
func runCapturing(t *testing.T, src string, extra map[string]any) (redfish, other map[string]any) {
	t.Helper()
	pb, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	e.ExtraVars = extra
	reg := modules.Default()
	reg.Register("redfish_info", func(_ context.Context, _ remoteexec.Connection, args map[string]any) (modules.Result, error) {
		redfish = args
		return modules.Ok("captured"), nil
	})
	reg.Register("control_module", func(_ context.Context, _ remoteexec.Connection, args map[string]any) (modules.Result, error) {
		other = args
		return modules.Ok("captured"), nil
	})
	e.Modules = reg
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if redfish == nil {
		t.Fatal("redfish_info did not run: the test would pass for the wrong reason")
	}
	return redfish, other
}

const redfishPlay = `
- name: p
  hosts: all
  gather_facts: false
  %VARS%
  tasks:
    - community.general.redfish_info:
        category: Systems
        baseuri: bmc.example.org
    - control_module: {}
`

func play(vars string) string {
	return strings.Replace(redfishPlay, "%VARS%", vars, 1)
}

func TestRedfishBackendFromAPlayVar(t *testing.T) {
	got, other := runCapturing(t, play("vars: {goansible_redfish_backend: go-redfish}"), nil)
	if got[modules.RedfishBackendKey] != "go-redfish" {
		t.Errorf("redfish_info got %v=%v, want go-redfish", modules.RedfishBackendKey, got[modules.RedfishBackendKey])
	}
	// Only the three redfish_* modules are told: a module that does not
	// read the key must not receive it.
	if _, set := other[modules.RedfishBackendKey]; set {
		t.Errorf("a non-redfish module received %s", modules.RedfishBackendKey)
	}
}

func TestRedfishBackendFromExtraVars(t *testing.T) {
	got, _ := runCapturing(t, play(""), map[string]any{RedfishBackendVar: "go-redfish"})
	if got[modules.RedfishBackendKey] != "go-redfish" {
		t.Errorf("-e %s=go-redfish did not reach the module: %v", RedfishBackendVar, got[modules.RedfishBackendKey])
	}
}

func TestRedfishBackendIsTemplated(t *testing.T) {
	got, _ := runCapturing(t, play(`vars: {which: go-redfish, goansible_redfish_backend: "{{ which }}"}`), nil)
	if got[modules.RedfishBackendKey] != "go-redfish" {
		t.Errorf("templated value not rendered: %v", got[modules.RedfishBackendKey])
	}
}

// An undefined variable inside the value fails the task with a message
// naming goansible_redfish_backend, before the module runs.
func TestRedfishBackendUndefinedFailsTheTask(t *testing.T) {
	pb, err := Parse([]byte(play(`vars: {goansible_redfish_backend: "{{ nosuchvar }}"}`)))
	if err != nil {
		t.Fatal(err)
	}
	e := New(localhostInventory())
	ran := false
	reg := modules.Default()
	reg.Register("redfish_info", func(context.Context, remoteexec.Connection, map[string]any) (modules.Result, error) {
		ran = true
		return modules.Ok("captured"), nil
	})
	reg.Register("control_module", func(context.Context, remoteexec.Connection, map[string]any) (modules.Result, error) {
		return modules.Ok("captured"), nil
	})
	e.Modules = reg
	res, _ := e.RunPlaybook(context.Background(), pb)
	if ran {
		t.Error("the module ran with an untemplatable backend value")
	}
	found := false
	if res != nil {
		for _, p := range res.Plays {
			for _, r := range p.Results {
				if r.Failed && strings.Contains(r.Msg, RedfishBackendVar) {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("no failed result naming %s", RedfishBackendVar)
	}
}

// The control: with no variable the module receives no key at all, so the
// default path (redfishtool on the target) is exactly yesterday's.
func TestRedfishBackendAbsentByDefault(t *testing.T) {
	got, _ := runCapturing(t, play(""), nil)
	if v, set := got[modules.RedfishBackendKey]; set {
		t.Errorf("no variable set, yet the module received %s=%v", modules.RedfishBackendKey, v)
	}
}

func TestRedfishBackendTaskArgWins(t *testing.T) {
	src := `
- name: p
  hosts: all
  gather_facts: false
  vars: {goansible_redfish_backend: go-redfish}
  tasks:
    - redfish_info:
        category: Systems
        baseuri: bmc.example.org
        _goansible_redfish_backend: redfishtool
    - control_module: {}
`
	got, _ := runCapturing(t, src, nil)
	if got[modules.RedfishBackendKey] != "redfishtool" {
		t.Errorf("the task's own choice was overridden by the play var: %v", got[modules.RedfishBackendKey])
	}
}
