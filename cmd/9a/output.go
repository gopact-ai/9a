package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/gopact-ai/9a/internal/api"
	apppkg "github.com/gopact-ai/9a/internal/app"
	callmodel "github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/projection"
	searchmodel "github.com/gopact-ai/9a/internal/search"
	workspacepkg "github.com/gopact-ai/9a/internal/workspace"
	"github.com/spf13/cobra"
)

func wantsJSON(cmd *cobra.Command) bool {
	value, err := cmd.Flags().GetBool("json")
	return err == nil && value
}

func writeCommandOutput(cmd *cobra.Command, request api.Request, data json.RawMessage, plainString bool) error {
	if wantsJSON(cmd) {
		if noDataAction(request.Action) && isNullJSON(data) {
			data = json.RawMessage(`{"ok":true}`)
		}
		return writeMachineData(cmd, data)
	}
	if plainString {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("decode daemon response: %w", err)
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), value)
		return err
	}
	output, err := humanResponse(request, data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), output)
	return err
}

func writeMachineData(cmd *cobra.Command, data json.RawMessage) error {
	payload := bytes.TrimSpace(data)
	if !json.Valid(payload) {
		return fmt.Errorf("daemon returned invalid JSON")
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), string(payload))
	return err
}

func noDataAction(action string) bool {
	switch action {
	case "adapter.add",
		"workspace.detach",
		"provider.add",
		"provider.remove",
		"declarative.remove",
		"acl.grant",
		"project.add",
		"call.cancel":
		return true
	default:
		return false
	}
}

func isNullJSON(data json.RawMessage) bool {
	payload := bytes.TrimSpace(data)
	return len(payload) == 0 || bytes.Equal(payload, []byte("null"))
}

func humanResponse(request api.Request, data json.RawMessage) (string, error) {
	switch request.Action {
	case "workspace.attach", "workspace.status":
		return humanWorkspace(request.Action, data)
	case "workspace.update":
		return humanUpdate(request, data)
	case "workspace.detach":
		return fmt.Sprintf("Detached workspace %s\n", terminalSafe(request.Root)), nil
	case "adapter.add":
		return fmt.Sprintf("Registered adapter %s\n  Executable: %s\n", terminalSafe(request.Protocol), terminalSafe(request.Executable)), nil
	case "provider.add":
		return fmt.Sprintf(
			"Added provider %s/%s\n  Endpoint: %s\n",
			terminalSafe(request.Protocol),
			terminalSafe(request.Name),
			terminalSafe(request.Endpoint),
		), nil
	case "provider.remove":
		return fmt.Sprintf("Removed provider %s/%s\n", terminalSafe(request.Protocol), terminalSafe(request.Name)), nil
	case "declarative.add":
		return humanDeclarativeAdd(data)
	case "declarative.diff":
		return humanDeclarativeDiff(data)
	case "declarative.remove":
		return fmt.Sprintf("Removed Skill %s\n", terminalSafe(request.Name)), nil
	case "acl.grant":
		return fmt.Sprintf(
			"Granted %s on %s to %s\n",
			terminalSafe(strings.Join(request.Permissions, ", ")),
			terminalSafe(request.Capability),
			terminalSafe(request.Identity),
		), nil
	case "search":
		return humanSearch(data)
	case "project.add":
		return fmt.Sprintf("Projected %s\n  Skills: %s\n", terminalSafe(request.Capability), terminalSafe(request.Root)), nil
	case "invoke":
		var output strings.Builder
		output.WriteString("Result:\n")
		if err := appendPrettyJSON(&output, data, "  "); err != nil {
			return "", err
		}
		return output.String(), nil
	case "call.get":
		return humanCall(data)
	case "call.events":
		return humanCallEvents(request.CallID, data)
	case "call.cancel":
		return fmt.Sprintf("Canceled call %s\n", terminalSafe(request.CallID)), nil
	default:
		return "", fmt.Errorf("no human-readable output for action %q", request.Action)
	}
}

func humanWorkspace(action string, data json.RawMessage) (string, error) {
	var status projection.Status
	if err := decodeResponse(action, data, &status); err != nil {
		return "", err
	}
	heading := "Workspace"
	if action == "workspace.attach" {
		heading = "Attached"
	}
	var output strings.Builder
	fmt.Fprintf(&output, "%s %s\n", heading, terminalSafe(status.Workspace.Root))
	if status.Workspace.State != "" {
		state := string(status.Workspace.State)
		if status.Workspace.State == workspacepkg.StateHealthy || status.Workspace.State == workspacepkg.StateFallback {
			state = "ready"
		}
		fmt.Fprintf(&output, "  State: %s\n", terminalSafe(state))
	}
	if status.Workspace.SkillsRoot != "" {
		fmt.Fprintf(&output, "  Skills: %s (%d managed)\n", terminalSafe(status.Workspace.SkillsRoot), len(status.Skills))
	}
	if status.Workspace.Backend != "" {
		fallback := ""
		if status.Workspace.FallbackReason != "" {
			fallback = " (automatic fallback; use --json for details)"
		}
		fmt.Fprintf(&output, "  Backend: %s%s\n", terminalSafe(string(status.Workspace.Backend)), fallback)
	}
	return output.String(), nil
}

func humanUpdate(request api.Request, data json.RawMessage) (string, error) {
	var result apppkg.UpdateResult
	if err := decodeResponse(request.Action, data, &result); err != nil {
		return "", err
	}
	var output strings.Builder
	if request.Check {
		output.WriteString("Update preview\n")
	} else {
		output.WriteString("Updated managed Skills\n")
	}
	fmt.Fprintf(&output, "  Providers (%d)\n", len(result.Providers))
	for _, item := range result.Providers {
		fmt.Fprintf(&output, "    %s: %s", terminalSafe(item.ID), terminalSafe(item.State))
		if item.Error != "" {
			fmt.Fprintf(&output, " — %s", terminalSafe(item.Error))
		}
		output.WriteByte('\n')
	}
	fmt.Fprintf(&output, "  Workspaces (%d)\n", len(result.Workspaces))
	for _, item := range result.Workspaces {
		fmt.Fprintf(&output, "    %s: %s\n", terminalSafe(item.Root), workspaceChangeSummary(item))
	}
	if result.Failed > 0 {
		fmt.Fprintf(&output, "  Failures: %d\n", result.Failed)
	}
	return output.String(), nil
}

func workspaceChangeSummary(item apppkg.WorkspaceUpdate) string {
	parts := make([]string, 0, 5)
	for _, value := range []struct {
		count int
		label string
	}{
		{item.Updated, "updated"},
		{item.Unchanged, "unchanged"},
		{item.Repaired, "repaired"},
		{item.Removed, "removed"},
		{item.Failed, "failed"},
	} {
		if value.count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", value.count, value.label))
		}
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}

func humanDeclarativeAdd(data json.RawMessage) (string, error) {
	var result apppkg.DeclarativeResult
	if err := decodeResponse("declarative.add", data, &result); err != nil {
		return "", err
	}
	var output strings.Builder
	fmt.Fprintf(&output, "Skill %s is ready\n", terminalSafe(result.Name))
	fmt.Fprintf(&output, "  Root: %s\n", terminalSafe(result.Root))
	fmt.Fprintf(&output, "  Digest: %s\n", terminalSafe(result.Digest))
	appendStringList(&output, "Capabilities", result.Capabilities)
	return output.String(), nil
}

func humanDeclarativeDiff(data json.RawMessage) (string, error) {
	var diff apppkg.DeclarativeDiff
	if err := decodeResponse("declarative.diff", data, &diff); err != nil {
		return "", err
	}
	if !diff.Changed {
		return fmt.Sprintf("No changes for Skill %s\n", terminalSafe(diff.Name)), nil
	}
	var output strings.Builder
	fmt.Fprintf(&output, "Changes for Skill %s\n", terminalSafe(diff.Name))
	if len(diff.Added) > 0 {
		appendStringList(&output, "Added", diff.Added)
	}
	if len(diff.Modified) > 0 {
		appendStringList(&output, "Modified", diff.Modified)
	}
	if len(diff.Removed) > 0 {
		appendStringList(&output, "Removed", diff.Removed)
	}
	return output.String(), nil
}

func humanSearch(data json.RawMessage) (string, error) {
	var results []searchmodel.Result
	if err := decodeResponse("search", data, &results); err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "No capabilities found.\n", nil
	}
	var output strings.Builder
	fmt.Fprintf(&output, "Capabilities (%d)\n", len(results))
	for _, result := range results {
		fmt.Fprintf(&output, "  %s\n", terminalSafe(result.Capability.ID))
		fmt.Fprintf(&output, "    Name: %s\n", singleLine(result.Capability.Name))
		if result.Capability.Description != "" {
			fmt.Fprintf(&output, "    Description: %s\n", singleLine(result.Capability.Description))
		}
	}
	return output.String(), nil
}

func humanCall(data json.RawMessage) (string, error) {
	var record callmodel.Record
	if err := decodeResponse("call.get", data, &record); err != nil {
		return "", err
	}
	var output strings.Builder
	fmt.Fprintf(&output, "Call %s\n", terminalSafe(record.Call.ID))
	fmt.Fprintf(&output, "  Capability: %s\n", terminalSafe(record.Call.CapabilityID))
	fmt.Fprintf(&output, "  State: %s\n", terminalSafe(string(record.Call.State)))
	if !record.Call.CreatedAt.IsZero() {
		fmt.Fprintf(&output, "  Created: %s\n", record.Call.CreatedAt.Format(time.RFC3339))
	}
	if !record.Call.UpdatedAt.IsZero() {
		fmt.Fprintf(&output, "  Updated: %s\n", record.Call.UpdatedAt.Format(time.RFC3339))
	}
	if record.Call.Code != "" || record.Call.Message != "" {
		fmt.Fprintf(&output, "  Error: %s", terminalSafe(record.Call.Code))
		if record.Call.Code != "" && record.Call.Message != "" {
			output.WriteString(": ")
		}
		output.WriteString(singleLine(record.Call.Message))
		output.WriteByte('\n')
	}
	if len(record.Result) > 0 && string(record.Result) != "null" {
		output.WriteString("  Result:\n")
		if err := appendPrettyJSON(&output, record.Result, "    "); err != nil {
			return "", err
		}
	}
	return output.String(), nil
}

func humanCallEvents(callID string, data json.RawMessage) (string, error) {
	var page callmodel.EventPage
	if err := decodeResponse("call.events", data, &page); err != nil {
		return "", err
	}
	if len(page.Events) == 0 {
		return fmt.Sprintf("No events for %s.\n", terminalSafe(callID)), nil
	}
	var output strings.Builder
	fmt.Fprintf(&output, "Events for %s (%d)\n", terminalSafe(callID), len(page.Events))
	for _, event := range page.Events {
		if err := appendHumanEvent(&output, event); err != nil {
			return "", err
		}
	}
	fmt.Fprintf(&output, "  Next cursor: %d\n", page.NextAfter)
	more := "no"
	if page.HasMore {
		more = "yes"
	}
	fmt.Fprintf(&output, "  More available: %s\n", more)
	return output.String(), nil
}

func appendHumanEvent(output *strings.Builder, event callmodel.Event) error {
	var envelope struct {
		Kind      string          `json:"kind"`
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		MediaType string          `json:"media_type"`
		Encoding  string          `json:"encoding"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(event.Envelope, &envelope); err != nil {
		return fmt.Errorf("decode call event: %w", err)
	}
	switch envelope.Kind {
	case "event":
		fmt.Fprintf(output, "  #%d event: %s\n", event.Sequence, terminalSafe(envelope.Type))
		if !isNullJSON(envelope.Data) {
			output.WriteString("    Data:\n")
			return appendPrettyJSON(output, envelope.Data, "      ")
		}
	case "artifact":
		fmt.Fprintf(output, "  #%d artifact: %s\n", event.Sequence, terminalSafe(envelope.Name))
		fmt.Fprintf(output, "    Media type: %s\n", terminalSafe(envelope.MediaType))
		fmt.Fprintf(output, "    Encoding: %s\n", terminalSafe(envelope.Encoding))
		if !isNullJSON(envelope.Data) {
			output.WriteString("    Data: omitted (use --json)\n")
		}
	default:
		fmt.Fprintf(output, "  #%d:\n", event.Sequence)
		return appendPrettyJSON(output, event.Envelope, "    ")
	}
	return nil
}

func writeValidationOutput(cmd *cobra.Command, result validationResult) error {
	if wantsJSON(cmd) {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return writeMachineData(cmd, data)
	}
	var output strings.Builder
	fmt.Fprintf(&output, "Valid Skill %s\n", terminalSafe(result.Name))
	fmt.Fprintf(&output, "  Digest: %s\n", terminalSafe(result.Digest))
	appendStringList(&output, "Capabilities", result.Capabilities)
	_, err := fmt.Fprint(cmd.OutOrStdout(), output.String())
	return err
}

func writeVersionOutput(cmd *cobra.Command, version string) error {
	if wantsJSON(cmd) {
		data, err := json.Marshal(struct {
			Version string `json:"version"`
		}{Version: version})
		if err != nil {
			return err
		}
		return writeMachineData(cmd, data)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "9a %s\n", terminalSafe(version))
	return err
}

func appendStringList(output *strings.Builder, heading string, values []string) {
	fmt.Fprintf(output, "  %s (%d)\n", heading, len(values))
	for _, value := range values {
		fmt.Fprintf(output, "    %s\n", terminalSafe(value))
	}
}

func appendPrettyJSON(output *strings.Builder, data json.RawMessage, prefix string) error {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, bytes.TrimSpace(data), "", "  "); err != nil {
		return fmt.Errorf("decode daemon response: %w", err)
	}
	for _, line := range strings.Split(pretty.String(), "\n") {
		output.WriteString(prefix)
		output.WriteString(line)
		output.WriteByte('\n')
	}
	return nil
}

func decodeResponse(action string, data json.RawMessage, target any) error {
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode %s response: %w", action, err)
	}
	return nil
}

func singleLine(value string) string {
	return terminalSafe(value)
}

func terminalSafe(value string) string {
	var output strings.Builder
	for _, r := range value {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			output.WriteByte(' ')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			if r <= 0xffff {
				fmt.Fprintf(&output, `\u%04x`, r)
			} else {
				fmt.Fprintf(&output, `\U%08x`, r)
			}
		default:
			output.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(output.String()), " ")
}
