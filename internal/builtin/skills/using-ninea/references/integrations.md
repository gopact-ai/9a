# Integration sources

NineA accepts a strict integration manifest. When the user supplies arbitrary
API documentation, OpenAPI, curl, or natural language, interpret it and produce
the manifest described in `manifest.md`; do not ask the user to translate it.
Before this gateway exists in a fresh workspace, load the identical embedded
contract with `9a connect --guide http --json`.

The editable source lives at:

```text
<workspace>/.9a/integrations/<name>.yaml
```

Run `9a connect` after creating or changing it. The command is idempotent.
Use `9a disconnect <name>` to remove runtime access while retaining the source.
After connecting, run the printed `9a search <integration> --json` to enumerate
the integration. Then inspect the selected capability with
`9a search <integration>/<capability> --json` before constructing input.

For a single local MCP executable or a remote A2A agent, prefer the shorter
forms; NineA creates the same canonical v1 manifest:

```sh
9a connect mcp --name local-tools -- /absolute/path/to/server
9a connect a2a --name research-agent https://agent.example.com
```

The A2A shortcut is for agents without bearer authentication. If the Agent Card
requires bearer authentication, create a manifest with exactly one credential:

```yaml
version: 1
name: research-agent
type: a2a
url: https://agent.example.com
credentials:
  bearer:
    secret: research-agent.bearer
```

Then run `9a connect <manifest.yaml>` and
`9a secret set research-agent.bearer`. Secret values belong to the current
workspace and must never appear in the manifest or prompt.

Every MCP and A2A run requires an approval preflight and token. MCP
`readOnlyHint` is descriptive metadata, not an approval bypass.
