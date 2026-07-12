package declarative

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/gopact-ai/9a/internal/mount"
)

type projectedContract struct {
	Name         string         `json:"name"`
	Kind         string         `json:"kind"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"inputSchema,omitempty"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
}

func RenderSkill(config *Config) (mount.Skill, error) {
	if config == nil {
		return mount.Skill{}, fmt.Errorf("declarative config is nil")
	}
	operationNames := sortedOperationNames(config.Operations)
	workflowNames := sortedWorkflowNames(config.Workflows)
	var guide strings.Builder
	fmt.Fprintf(&guide, "---\nname: %s\ndescription: %q\n---\n\n# %s\n\n%s\n\n", config.Metadata.Name, config.Metadata.Description, config.Metadata.Name, config.Metadata.Description)
	guide.WriteString("This skill exposes API operations as local filesystem tools. Send one JSON object to an `invoke` file; it forwards the request through 9a, applies configured hooks, and prints JSON.\n\n")
	guide.WriteString("## Operations\n\n")
	for _, name := range operationNames {
		description := config.Operations[name].Description
		if description == "" {
			description = "Invoke " + name + "."
		}
		fmt.Fprintf(&guide, "- `%s`: %s Run `printf '%%s' '<json>' | operations/%s/invoke`.\n", name, description, name)
	}
	if len(workflowNames) > 0 {
		guide.WriteString("\n## Workflows\n\n")
		for _, name := range workflowNames {
			description := config.Workflows[name].Description
			if description == "" {
				description = "Run the configured multi-step workflow."
			}
			fmt.Fprintf(&guide, "- `%s`: %s Run `printf '%%s' '<json>' | workflows/%s/invoke`.\n", name, description, name)
		}
	}
	guide.WriteString("\nInput and output contracts are stored beside every invoke file. Runtime credentials come from the environment and are never copied into this directory.\n")

	files := []mount.File{{Path: "SKILL.md", Mode: 0o644, Data: []byte(guide.String())}}
	for _, name := range operationNames {
		operation := config.Operations[name]
		contract := projectedContract{name, "operation", operation.Description, operation.InputSchema, operation.OutputSchema}
		generated, err := projectedFiles(config, "operations", name, contract)
		if err != nil {
			return mount.Skill{}, err
		}
		files = append(files, generated...)
	}
	for _, name := range workflowNames {
		workflow := config.Workflows[name]
		contract := projectedContract{name, "workflow", workflow.Description, workflow.InputSchema, workflow.OutputSchema}
		generated, err := projectedFiles(config, "workflows", name, contract)
		if err != nil {
			return mount.Skill{}, err
		}
		files = append(files, generated...)
	}
	files = append(files, mount.File{Path: "references/source.yaml", Mode: 0o600, Data: append([]byte(nil), config.Source...)})
	return mount.Skill{Name: config.Metadata.Name, CapabilityID: "api/" + config.Metadata.Name, Revision: 1, Files: files}, nil
}

func projectedFiles(config *Config, group, name string, contract projectedContract) ([]mount.File, error) {
	schema, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		return nil, err
	}
	schema = append(schema, '\n')
	invoke := fmt.Sprintf("#!/bin/sh\nset -eu\nexec 9a invoke api/%s/%s \"$@\"\n", config.Metadata.Name, name)
	return []mount.File{
		{Path: group + "/" + name + "/schema.json", Mode: 0o644, Data: schema},
		{Path: group + "/" + name + "/invoke", Mode: fs.FileMode(0o755), Data: []byte(invoke)},
	}, nil
}

func sortedOperationNames(values map[string]Operation) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedWorkflowNames(values map[string]Workflow) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
