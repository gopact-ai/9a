# Integrations

## MCP

Use an absolute executable path for a local stdio server:

```sh
9a providers add mcp weather "stdio:/absolute/path/to/mcp-server"
9a search "weather"
9a project add mcp/weather/get-weather .agents/skills
```

## A2A

Start `9a daemon` with the provider token in
`NINEA_A2A_TOKEN_<NORMALIZED_PROVIDER_NAME>`, then register the HTTPS endpoint:

```sh
9a providers add a2a research-agent https://agent.example.com
9a search "research"
```

## Custom protocols

Register a reviewed executable implementing `9a.adapter/v1`, then add its
provider:

```sh
9a adapters add billing /absolute/path/to/billing-adapter
9a providers add billing production https://billing.example.com
```

Remove a persisted provider and all of its managed views with:

```sh
9a providers remove <protocol> <name>
```

Create a separate token for each agent and grant only the required read and
invoke permissions:

```sh
AGENT_TOKEN="$(9a tokens create support-agent)"
9a acl grant support-agent <capability-id> read,invoke
```
