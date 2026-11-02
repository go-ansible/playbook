# playbook

Playbook/role/task/handler engine: loops, conditionals, blocks, strategies.

Part of [go-ansible](https://github.com/go-ansible) — a pure-Go (CGO=0),
functional-parity port of [Ansible](https://www.ansible.com/).

[![CI](https://github.com/go-ansible/playbook/actions/workflows/ci.yml/badge.svg)](https://github.com/go-ansible/playbook/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-ansible/playbook.svg)](https://pkg.go.dev/github.com/go-ansible/playbook)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue.svg)](LICENSE)

## Usage

```go
data, err := os.ReadFile("site.yml")
pb, err := playbook.Parse(data)

eng := playbook.New(inv) // inv: a *github.com/go-ansible/inventory.Inventory
result, err := eng.RunPlaybook(ctx, pb)
if result.Failed() {
    // per-host summaries are in result.Hosts
}
```

Ties inventory+vars+template+modules+facts together with real per-host
execution under either strategy Ansible ships, `linear` and `free`:
`when`/`loop`/`register`, block/rescue/always with per-host recovery,
`notify`/handlers, `become` (including `become_exe`/`become_flags`),
`pre_tasks`/`post_tasks` ordering, `roles`, `include_role`/`import_role`,
`include_tasks`/`import_tasks`, `import_playbook`, `vars_files`,
`vars_prompt`, `delegate_to`, `serial`/`order`/`throttle`, `run_once`,
`until`/`retries`, `async`/`poll`, `tags`, `meta:`, `add_host`, `group_by`
and `include_vars`.

`Connect` reaches a host over `local`, `ssh` or `winrm` —
`ansible_connection: winrm` speaks WS-Management to a Windows target, using
the real plugin's own `ansible_winrm_*` variables.

## Migrating an old playbook

`CompatScan` reports the constructs in a playbook source that no current
Ansible will run — the reference's or this one's — and says what to write
instead:

```go
findings, err := playbook.CompatScan(data)
for _, f := range findings {
    fmt.Println(f) // 4:3: play keyword "sudo": use `become: true`
}
```

It works on sources that do **not** parse, which is the point: a playbook
carrying `sudo:` is refused by both implementations, so a report needing a
parsed playbook would be silent in the one case it is for. The reference's
own diagnosis names the symptom only — measured against ansible-core
2.21.4, `sudo:` on a task exits 4 with `conflicting action statements:
debug, sudo`, which does not hint that `sudo` was ever a keyword.

Every keyword in the table was measured to be refused by the reference
rather than taken from a changelog, and the test that measures them
carries its own control. What the table deliberately does not claim is the
version each one went away in: the reference does not ship that history.

A module name is *not* a compatibility problem here. The 2.10 split that
moved modules out of core is invisible to this port, because the registry
is keyed by short name: a 2.9-era `ufw:` resolves exactly as
`community.general.ufw` does. A name that does not resolve is one this
port has not ported yet.

A handful of task keywords are still refused by name rather than ignored
(`connection`/`remote_user`/`port` at task level, `collections`,
`delegate_facts`, `debugger`, and any strategy other than those two) — see
the org's [feature matrix](https://go-ansible.github.io/), which is read
from `engine.go` and `playbook.go` directly, for the current status of each.
