# Integrations, capabilities, and workspaces

An **integration** is one external system. A **capability** is one action that
system exposes. A **workspace** is the project whose agent can discover and run
those actions.

An HTTP integration has one editable source of truth:

```text
<workspace>/.9a/integrations/<integration>.yaml
```

`9a connect` validates the source, saves it in the workspace, and builds the
runtime index. MCP and A2A shortcuts create the same canonical v1 source. The
runtime index points back to those files and caches capability contracts; it is
not another configuration source. Persisted call history and workspace-scoped
secret metadata remain operational state.

Every prepared workspace has one `using-ninea` gateway Skill. It teaches an
agent how to search and run capabilities. All integrations share that gateway,
which is derived state rather than an authoring surface.

Capability references use `<integration>/<capability>`. Protocol and storage
details stay behind this normal interface, so HTTP, MCP, and A2A capabilities
are searched and run the same way.

Searching a single canonical integration name lists all of its capabilities in
the current workspace. Searching an exact capability reference includes that
capability's input and output contracts.

`disconnect` removes an integration from the active runtime but keeps its
canonical source. Run `connect` with that source to activate it again.
