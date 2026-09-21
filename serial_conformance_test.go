package playbook

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-ansible/inventory"
)

// TestBatchHostsMatchesRealAnsible pins serial: batching to sizes
// measured from real ansible-core 2.21.4. Every `want` below is the
// batch sizes a real run actually produced for that serial value over
// that many hosts.
func TestBatchHostsMatchesRealAnsible(t *testing.T) {
	tests := []struct {
		serial []string
		hosts  int
		want   []int
	}{
		// Plain counts.
		{[]string{"2"}, 5, []int{2, 2, 1}},
		{[]string{"1"}, 5, []int{1, 1, 1, 1, 1}},
		{[]string{"5"}, 5, []int{5}},
		{[]string{"99"}, 5, []int{5}},
		// Zero or less means "all the rest at once", not an error.
		{[]string{"0"}, 5, []int{5}},
		{[]string{"-1"}, 5, []int{5}},
		{nil, 5, []int{5}},
		// Percentages, of the TOTAL host count.
		{[]string{"50%"}, 5, []int{2, 2, 1}},
		{[]string{"30%"}, 5, []int{1, 1, 1, 1, 1}},
		{[]string{"100%"}, 5, []int{5}},
		// A percentage that works out to zero becomes one, so a small
		// percentage of a small fleet still makes progress.
		{[]string{"10%"}, 5, []int{1, 1, 1, 1, 1}},
		{[]string{"0%"}, 5, []int{1, 1, 1, 1, 1}},
		// Lists: consumed in order, last entry repeating.
		{[]string{"1", "2"}, 5, []int{1, 2, 2}},
		{[]string{"1", "60%"}, 5, []int{1, 3, 1}},
		{[]string{"2"}, 5, []int{2, 2, 1}},
		// The float-truncation case: Python computes
		// int((29 / 100.0) * 100), and 0.29*100 is 28.999999999999996,
		// so real ansible-core batches 28 — not the 29 that integer
		// arithmetic would give. Measured on a real 100-host run.
		{[]string{"29%"}, 100, []int{28, 28, 28, 16}},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%s over %d", strings.Join(tt.serial, ","), tt.hosts)
		if len(tt.serial) == 0 {
			name = fmt.Sprintf("unset over %d", tt.hosts)
		}
		t.Run(name, func(t *testing.T) {
			hosts := nHosts(t, tt.hosts)
			var got []int
			for _, b := range batchHosts(hosts, tt.serial) {
				got = append(got, len(b))
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("batches = %v, real ansible-core gives %v", got, tt.want)
			}
			// However they are split, every host runs exactly once and
			// in inventory order.
			var flat []string
			for _, b := range batchHosts(hosts, tt.serial) {
				for _, h := range b {
					flat = append(flat, h.Name)
				}
			}
			if len(flat) != tt.hosts {
				t.Errorf("batches cover %d hosts, want %d", len(flat), tt.hosts)
			}
			for i, name := range flat {
				if want := hosts[i].Name; name != want {
					t.Errorf("host %d is %s, want %s — batching must preserve inventory order", i, name, want)
				}
			}
		})
	}
}

// A play whose pattern matched nothing still gets one empty batch, so
// it is bannered and reports that it matched no hosts.
func TestBatchHostsWithNoHosts(t *testing.T) {
	for _, serial := range [][]string{nil, {"2"}, {"50%"}} {
		got := batchHosts(nil, serial)
		if len(got) != 1 || len(got[0]) != 0 {
			t.Errorf("serial %v: got %d batches, want exactly one empty one", serial, len(got))
		}
	}
}

func TestSerialParsesScalarsAndLists(t *testing.T) {
	tests := []struct {
		yaml string
		want []string
	}{
		{"serial: 2", []string{"2"}},
		{`serial: "50%"`, []string{"50%"}},
		{"serial: [1, 2]", []string{"1", "2"}},
		{`serial: [1, "60%"]`, []string{"1", "60%"}},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run(tt.yaml, func(t *testing.T) {
			pb, err := Parse([]byte("- hosts: all\n  gather_facts: false\n  " + tt.yaml + "\n  tasks: []\n"))
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(pb[0].Serial) != fmt.Sprint(tt.want) {
				t.Errorf("Serial = %q, want %q", pb[0].Serial, tt.want)
			}
		})
	}
}

func nHosts(t *testing.T, n int) []*inventory.Host {
	t.Helper()
	var b strings.Builder
	b.WriteString("all:\n  hosts:\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "    h%d: {ansible_connection: local}\n", i)
	}
	inv, err := inventory.ParseYAML([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := inv.Match("all")
	if err != nil {
		t.Fatal(err)
	}
	return hosts
}
