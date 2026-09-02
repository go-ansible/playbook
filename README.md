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
linear-strategy execution: `when`/`loop`/`register`, block/rescue/always with
per-host recovery, `notify`/handlers, `become`, and `pre_tasks`/`post_tasks`
ordering. Some playbook keys are parsed but not yet wired into the engine
(`roles`, `tags`, `serial`, `delegate_to`, `vars_files`,
`include_tasks`/`import_tasks`) — see the org's
[feature matrix](https://go-ansible.github.io/) for the current, re-checked
status of each.
