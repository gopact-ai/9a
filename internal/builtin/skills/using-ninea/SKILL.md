---
name: using-ninea
description: Use when an agent needs to connect, find, run, inspect, or disconnect capabilities managed by NineA.
---

# Using NineA

NineA is a local capability runtime. Search before loading details, and run a
capability only when the user's task requires an upstream action.

## Find and run

```sh
9a search "what the user needs" --json
9a search <integration>/<capability> --json
9a run <integration>/<capability> --input '<json>' --json
```

Use `--input @file` for an existing request file or pipe one JSON value to
stdin. As an agent, use `--json` for every `search` and `run`; the structured
error fields are part of the safety contract.

Reading this Skill, running `search`, and inspecting a contract have no
upstream side effects. `run` is the execution boundary.

If NineA reports `approval_required`, explain the proposed upstream action and
ask the user to approve it. Preserve `data.approvalToken` and the exact input.
Only after explicit approval, retry that unchanged input with
`--approve <approvalToken>`. Never use a token from earlier or implied approval.
The token is single-use, expires after 10 minutes, and is invalidated by a
daemon restart. If it expires, was already used, or the input or capability
changes, run a new preflight and ask again.

On a `run` error, inspect `data.sideEffect`. `none` means no upstream request
was sent, so follow `nextAction`. `possible` means the outcome is unknown:
never retry automatically, inspect upstream state when possible, and tell the
user what may have happened. `retryable` never overrides these rules.

## Connect an HTTP API

On a fresh workspace where this Skill has not been projected yet, the same
embedded contract is available through `9a connect --guide http --json`. Read
`references/manifest.md`, create an integration manifest, then run:

```sh
9a connect <manifest.yaml>
```

Treat `.9a/integrations/<name>.yaml` as the source of truth. The generated
`.agents/skills/using-ninea` gateway is disposable and must not be edited.
After connecting, run the integration `search --json` command printed by NineA,
select a capability, then inspect its exact
`<integration>/<capability>` contract before constructing input.

For local MCP servers and remote A2A agents, use the shortcuts in
`references/integrations.md`.

## Remove runtime access

```sh
9a disconnect <integration>
```

This keeps the source manifest. Never place credential values in manifests,
prompts, command arguments, or generated files. `9a secret` values are scoped
to the current workspace.

Read `references/troubleshooting.md` when a command fails. Stay within the seven
public command groups and follow the next action printed by NineA.
