---
name: using-ninea
description: Use when an AI agent needs to discover, invoke, add, connect, update, diagnose, or remove capabilities managed by 9A, including declarative YAML APIs, MCP servers, A2A agents, projected Skills, and persistent calls.
---

# Using NineA

Use the filesystem for discovery and commands for execution. Treat every
9A-managed Skill as read-only; change its YAML or upstream provider instead of
editing projected files.

`9a search` also indexes user-owned `.agents/skills/<name>/SKILL.md` entries.
Each search scans complete Skill directories in every attached workspace, so
local additions, edits, and removals require no import command.

## Choose the workflow

- Find or run a capability: use `9a search`, inspect the projected schema, then
  pipe one JSON object to its `invoke` entry.
- Add one or more JSON APIs: read `references/declarative.md`, author YAML,
  validate it, then use `9a add`.
- Connect MCP, A2A, or another protocol: read `references/integrations.md`.
- Repair, update, inspect, or remove a workspace view: read
  `references/troubleshooting.md`.
- Upgrade the installed software: read `references/troubleshooting.md`, explain
  the difference between `brew upgrade` and `9a update`, and obtain user
  approval before changing packages or restarting the daemon.

Prefer projected invoke commands over constructing direct provider requests.
Do not place credentials in YAML, prompts, command arguments, or projected
files; provide them to `9a daemon` through its environment.

## Fast path

```sh
9a status --json
9a search "what the user needs"
9a project add <capability-id> .agents/skills
printf '%s\n' '<json-input>' | \
  .agents/skills/<projected-skill>/scripts/invoke
```

Use `9a calls start` instead of synchronous invocation when work must outlive
one CLI request. Never grant an agent broader ACLs than the requested task.
