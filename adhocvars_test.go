package playbook

import (
	"os"
	"reflect"
	"testing"

	"github.com/go-ansible/inventory"
)

// Every expectation was measured against real ansible-core 2.21.4 with
// `ansible -i inv h1 -m debug -a 'msg={{ <var> }}'` over this same
// two-host inventory.
func TestAdHocHostVars(t *testing.T) {
	inv, err := inventory.ParseINI([]byte("[web]\nh1\nh2\n"))
	if err != nil {
		t.Fatal(err)
	}
	inv.Groups["web"].Vars["wv"] = "from-web-group"

	got := AdHocHostVars(inv, "h1", []string{"h1"})

	t.Run("the magic host variables", func(t *testing.T) {
		for k, want := range map[string]any{
			"inventory_hostname":       "h1",
			"inventory_hostname_short": "h1",
		} {
			if got[k] != want {
				t.Errorf("%s = %#v, want %#v", k, got[k], want)
			}
		}
		if names, _ := got["group_names"].([]string); !reflect.DeepEqual(names, []string{"web"}) {
			t.Errorf("group_names = %#v, want [web]", got["group_names"])
		}
	})

	t.Run("a group_vars value reaches the host", func(t *testing.T) {
		if got["wv"] != "from-web-group" {
			t.Errorf("wv = %#v, want from-web-group", got["wv"])
		}
	})

	t.Run("groups is transitive", func(t *testing.T) {
		groups, _ := got["groups"].(map[string]any)
		web, _ := groups["web"].([]string)
		if len(web) != 2 {
			t.Errorf("groups.web = %#v, want two hosts", groups["web"])
		}
		if all, _ := groups["all"].([]string); len(all) != 2 {
			t.Errorf("groups.all = %#v, want two hosts", groups["all"])
		}
	})

	t.Run("hostvars reaches another host's vars", func(t *testing.T) {
		hv, _ := got["hostvars"].(map[string]any)
		h2, _ := hv["h2"].(map[string]any)
		if h2["wv"] != "from-web-group" {
			t.Errorf(`hostvars["h2"].wv = %#v, want from-web-group`, h2["wv"])
		}
	})

	t.Run("the three play lists are the matched set", func(t *testing.T) {
		for _, k := range []string{"ansible_play_hosts", "ansible_play_hosts_all", "ansible_play_batch"} {
			if hosts, _ := got[k].([]string); !reflect.DeepEqual(hosts, []string{"h1"}) {
				t.Errorf("%s = %#v, want [h1]", k, got[k])
			}
		}
	})

	// inventory_dir and playbook_dir are NOT the same thing: measured,
	// `-i sub/inv2` gives inventory_dir .../sub while playbook_dir stays
	// the working directory, there being no playbook.
	t.Run("playbook_dir is the working directory", func(t *testing.T) {
		wd, err := os.Getwd()
		if err != nil {
			t.Skip(err)
		}
		if got["playbook_dir"] != wd {
			t.Errorf("playbook_dir = %#v, want %q", got["playbook_dir"], wd)
		}
	})

	t.Run("a host_var cannot shadow a magic one", func(t *testing.T) {
		inv2, err := inventory.ParseINI([]byte("[web]\nh1 inventory_hostname=IMPOSTOR\n"))
		if err != nil {
			t.Fatal(err)
		}
		if v := AdHocHostVars(inv2, "h1", []string{"h1"})["inventory_hostname"]; v != "h1" {
			t.Errorf("inventory_hostname = %#v, want h1 — a host_var shadowed a magic variable", v)
		}
	})
}
