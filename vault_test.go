package playbook

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-ansible/vault"
)

func writeVault(t *testing.T, path, plaintext string) {
	t.Helper()
	enc, err := vault.Encrypt([]byte(plaintext), "pw", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestVaultEncryptedVarsFileAndRoleVars covers the files a parse pulls
// in: a vars_files target and a role's own vars, both encrypted. None of
// this could be read before — ansible-playbook had no vault flag at all.
func TestVaultEncryptedVarsFileAndRoleVars(t *testing.T) {
	dir := t.TempDir()
	writeVault(t, filepath.Join(dir, "secrets.yml"), "vf_secret: from_vars_file\n")
	writeVault(t, filepath.Join(dir, "roles/r/vars/main.yml"), "role_secret: from_role_vars\n")
	writePlaybookFile(t, dir, "roles/r/tasks/main.yml",
		"- name: in role\n  debug: {msg: \"{{ role_secret }}\"}\n")
	pbPath := writePlaybookFile(t, dir, "site.yml", `
- hosts: all
  gather_facts: false
  vars_files: [secrets.yml]
  roles: [r]
  tasks:
    - name: from vars file
      debug: {msg: "{{ vf_secret }}"}
`)

	// Without the password the encrypted file is a clear error.
	if _, err := ParseFile(pbPath); err == nil {
		t.Error("encrypted vars_files with no password: got nil error, want one")
	}

	pb, err := ParseFileWithVault(pbPath, "pw")
	if err != nil {
		t.Fatal(err)
	}
	msgs := map[string]string{}
	e := New(localhostInventory())
	e.OnResult = func(r Result) { msgs[r.Task] = r.Msg }
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if msgs["from vars file"] != "from_vars_file" {
		t.Errorf("vars_files secret = %q", msgs["from vars file"])
	}
	if msgs["in role"] != "from_role_vars" {
		t.Errorf("role vars secret = %q", msgs["in role"])
	}
}

// TestVaultEncryptedPlaybookAndIncludeVars covers the other two: the
// playbook file itself, and include_vars, which is read at RUN time and
// so takes its password from the Engine rather than the parse.
func TestVaultEncryptedPlaybookAndIncludeVars(t *testing.T) {
	dir := t.TempDir()
	writeVault(t, filepath.Join(dir, "extra.yml"), "iv_secret: from_include_vars\n")
	writeVault(t, filepath.Join(dir, "site.yml"), `
- hosts: all
  gather_facts: false
  tasks:
    - include_vars: extra.yml
    - name: after include
      debug: {msg: "{{ iv_secret }}"}
`)

	pb, err := ParseFileWithVault(filepath.Join(dir, "site.yml"), "pw")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	e := New(localhostInventory())
	e.BaseDir = dir
	e.VaultPassword = "pw"
	e.OnResult = func(r Result) {
		if r.Task == "after include" {
			got = r.Msg
		}
	}
	if _, err := e.RunPlaybook(context.Background(), pb); err != nil {
		t.Fatal(err)
	}
	if got != "from_include_vars" {
		t.Errorf("include_vars secret = %q, want from_include_vars", got)
	}
}
