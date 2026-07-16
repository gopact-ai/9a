package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gopact-ai/9a/internal/builtin"
	"github.com/spf13/cobra"
)

type connectRoute struct {
	Type    string `json:"type"`
	UseWhen string `json:"useWhen"`
	Command string `json:"command"`
}

type connectGuideOutput struct {
	Type            string `json:"type"`
	ManifestVersion int    `json:"manifestVersion"`
	Template        string `json:"template"`
	Guide           string `json:"guide"`
	NextAction      struct {
		Instruction string `json:"instruction"`
		Command     string `json:"command"`
	} `json:"nextAction"`
}

var connectRoutes = []connectRoute{
	{Type: "http", UseWhen: "API documentation, OpenAPI, curl, or an HTTP endpoint", Command: "9a connect --guide http --json"},
	{Type: "mcp", UseWhen: "a local MCP executable", Command: "9a connect mcp --name <slug> -- /absolute/executable"},
	{Type: "a2a", UseWhen: "a remote A2A agent URL", Command: "9a connect a2a --name <slug> https://agent.example.com"},
}

const httpManifestTemplate = `version: 1
name: example-api
description: Describe what this integration provides.
type: http
services:
  api:
    baseURL: https://api.example.com
capabilities:
  get-item:
    description: Read one item.
    service: api
    method: GET
    path: /items
    request:
      query:
        id: "{{ input.id }}"
    inputSchema:
      type: object
      required: [id]
      additionalProperties: false
      properties:
        id: {type: string}
    outputSchema:
      type: object
`

const mcpManifestTemplate = `version: 1
name: local-tools
type: mcp
executable: /absolute/path/to/mcp-server
`

const a2aManifestTemplate = `version: 1
name: research-agent
type: a2a
url: https://agent.example.com
`

func writeConnectRoutes(cmd *cobra.Command) error {
	if wantsJSON(cmd) {
		payload, err := json.Marshal(struct {
			Routes []connectRoute `json:"routes"`
		}{Routes: connectRoutes})
		if err != nil {
			return err
		}
		return writeMachineData(cmd, payload)
	}
	var output strings.Builder
	output.WriteString("Choose how to connect\n")
	for _, route := range connectRoutes {
		fmt.Fprintf(&output, "  %s — %s\n    %s\n", route.Type, route.UseWhen, route.Command)
	}
	_, err := fmt.Fprint(cmd.OutOrStdout(), output.String())
	return err
}

func writeConnectGuide(cmd *cobra.Command, kind string) error {
	guide, err := builtin.ConnectionGuide(kind)
	if err != nil {
		return err
	}
	output := connectGuideOutput{Type: kind, ManifestVersion: 1, Guide: string(guide)}
	switch kind {
	case "http":
		output.Template = httpManifestTemplate
		output.NextAction.Instruction = "Write the completed manifest to a YAML file, then connect it"
		output.NextAction.Command = "9a connect <manifest.yaml>"
	case "mcp":
		output.Template = mcpManifestTemplate
		output.NextAction.Instruction = "Use the shortcut, or write the manifest and connect it"
		output.NextAction.Command = "9a connect mcp --name <slug> -- /absolute/executable"
	case "a2a":
		output.Template = a2aManifestTemplate
		output.NextAction.Instruction = "Use the shortcut, or write the manifest and connect it"
		output.NextAction.Command = "9a connect a2a --name <slug> https://agent.example.com"
	}
	if wantsJSON(cmd) {
		payload, err := json.Marshal(output)
		if err != nil {
			return err
		}
		return writeMachineData(cmd, payload)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n\nNext:\n  %s\n", strings.TrimSpace(output.Guide), output.NextAction.Command)
	return err
}
