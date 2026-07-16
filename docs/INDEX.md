# NineA documentation

NineA is a local capability runtime. Start with the HTTP path, then use the
reference material when an integration needs credentials or another protocol.

## Get started

- [Connect an HTTP integration](guides/connect-http.md)
- [Use NineA from an agent](guides/use-from-an-agent.md)
- [Integration, capability, and workspace](concepts/integrations-capabilities-workspaces.md)
- [Runnable integration examples](../examples/integrations/README.md)

## Reference

- [CLI](reference/cli.md)
- [Integration manifest](reference/manifest.md)
- [Errors and side effects](reference/errors.md)
- [Security](SECURITY.md)
- [Architecture](architecture.md)

## Contributing

- [Contributing guide](CONTRIBUTING.md)

Editable integration sources live at `.9a/integrations/<name>.yaml`. Runtime
indexes and the `using-ninea` gateway Skill are derived state, not
configuration.
