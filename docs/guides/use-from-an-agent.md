# Use NineA from an agent

NineA places one gateway Skill, `using-ninea`, in the workspace. It does not
generate a Skill for each integration.

The normal agent flow is:

1. Run `9a status` and follow any printed next action.
2. Search for the requested behavior with `9a search`.
3. Inspect the selected contract with
   `9a search <integration>/<capability> --json`.
4. Run it with `9a run <integration>/<capability>`.

Searching and checking status do not contact the upstream system. `run` is the
explicit side-effect boundary. If NineA requires `--approve`, the agent must
obtain explicit user approval before retrying; it must not infer approval from
the original request. The first run returns `data.approvalToken` without sending
anything upstream. After approval, the agent must retry the exact same input
with `--approve <that-token>`; a changed input or capability requires a new
preflight and approval. The token is single-use, expires after 10 minutes, and
is invalidated by a daemon restart; if it cannot be used, run a new preflight
instead of reusing it.

When connecting a new HTTP API, the agent should create a version 1 manifest,
run `9a connect`, and use the next command from the result. NineA copies the
validated source to `.9a/integrations/<name>.yaml`; later edits should target
that canonical file.

Before the first connection has projected the gateway Skill, load the embedded
authoring contract with:

```sh
9a connect --guide http --json
```

For a local MCP server or remote A2A agent, use the shorter forms:

```sh
9a connect mcp --name local-tools -- /absolute/path/to/server
9a connect a2a --name research-agent https://agent.example.com
```

For an A2A agent that requires bearer authentication, use an
[A2A manifest](../reference/manifest.md#a2a) with one declared credential and
ask the user to store it with `9a secret set`.

If a declared credential is missing, `status` or `run` prints a reference such
as `private-api.api-token`. The agent may ask the user to run
`9a secret set private-api.api-token`, but must never request the value in chat
or place it in a manifest, command argument, prompt, or log. Secret references
resolve only within the current workspace.
