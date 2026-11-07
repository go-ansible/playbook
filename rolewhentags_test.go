package playbook

import (
	"context"
	"testing"
)

// go-ansible/playbook#192: `when:` and `tags:` on an entry in `roles:` were
// recognised by the parser only to be dropped -- skipped so they would not
// land in the role's vars, and then read by nobody. A role written with
// either ran unconditionally and untagged.

func roleFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writePlaybookFile(t, dir, "roles/myrole/tasks/main.yml", "- command: role\n")
	return dir
}

func TestRoleEntryWhenIsParsed(t *testing.T) {
	dir := roleFixture(t)
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  gather_facts: false
  roles:
    - role: myrole
      when: false
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	role := pb[0].Tasks[0]
	if role.When != "false" {
		t.Errorf("When = %q, want the entry's own condition", role.When)
	}
	// It must NOT have been swept into the role's vars, which is where
	// every unrecognised key on the entry goes.
	if _, leaked := role.RoleVars["when"]; leaked {
		t.Error("when: leaked into the role's vars")
	}
}

func TestRoleEntryTagsReachTheRolesTasks(t *testing.T) {
	dir := roleFixture(t)
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  gather_facts: false
  roles:
    - role: myrole
      tags: [setup]
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	role := pb[0].Tasks[0]
	if !hasTag(role.Tags, "setup") {
		t.Errorf("role tags = %v, want setup", role.Tags)
	}
	// propagateTags recurses into Block, so the role's own task carries it
	// too -- which is what makes `--tags setup` select the role's work and
	// not merely its container.
	if len(role.Block) != 1 {
		t.Fatalf("role block = %+v, want the role's one task", role.Block)
	}
	if !hasTag(role.Block[0].Tags, "setup") {
		t.Errorf("role task tags = %v, want setup propagated into the block", role.Block[0].Tags)
	}
	if _, leaked := role.RoleVars["tags"]; leaked {
		t.Error("tags: leaked into the role's vars")
	}
}

func hasTag(tags []string, want string) bool {
	for _, tg := range tags {
		if tg == want {
			return true
		}
	}
	return false
}

// Parsing it is half the fix; the engine has to act on it. runBlock already
// gates a whole block on its When, so a role with `when: false` must not run
// its tasks at all.
func TestRoleEntryWhenSkipsTheRole(t *testing.T) {
	for _, tc := range []struct {
		name, when string
		wantRun    bool
	}{
		{"false skips", "false", false},
		{"true runs", "true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := roleFixture(t)
			pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  gather_facts: false
  roles:
    - role: myrole
      when: `+tc.when+`
`)
			pb, err := ParseFile(pbPath)
			if err != nil {
				t.Fatal(err)
			}
			e := New(localhostInventory())
			var ran bool
			e.OnResult = func(r Result) {
				if r.Module == "command" && !r.Skipped {
					ran = true
				}
			}
			if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
				t.Fatal(err)
			}
			if ran != tc.wantRun {
				t.Errorf("role task ran = %v, want %v for when: %s", ran, tc.wantRun, tc.when)
			}
		})
	}
}

// A plain string entry has neither, and must not acquire an empty condition
// that something later mistakes for a real one.
func TestAPlainRoleEntryHasNoWhenOrTags(t *testing.T) {
	dir := roleFixture(t)
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  gather_facts: false
  roles:
    - myrole
`)
	pb, err := ParseFile(pbPath)
	if err != nil {
		t.Fatal(err)
	}
	if role := pb[0].Tasks[0]; role.When != "" || len(role.Tags) != 0 {
		t.Errorf("When = %q, Tags = %v, want both empty", role.When, role.Tags)
	}
}
