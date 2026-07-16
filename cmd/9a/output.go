package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"

	"github.com/gopact-ai/9a/internal/api"
	"github.com/spf13/cobra"
)

type connectResult struct {
	Name         string   `json:"name"`
	Source       string   `json:"source"`
	Capabilities []string `json:"capabilities"`
}

func wantsJSON(cmd *cobra.Command) bool {
	value, err := cmd.Flags().GetBool("json")
	return err == nil && value
}

func writeCommandOutput(cmd *cobra.Command, request api.Request, data json.RawMessage) error {
	if wantsJSON(cmd) {
		if (request.Action == "disconnect" || request.Action == "secret.set" || request.Action == "secret.unset") && isNullJSON(data) {
			data = json.RawMessage(`{"ok":true}`)
		}
		return writeMachineData(cmd, data)
	}
	output, err := humanResponse(request, data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), output)
	return err
}

func writeRemoteErrorData(cmd *cobra.Command, request api.Request, data json.RawMessage) error {
	if request.Action == "run" && !wantsJSON(cmd) {
		var failure struct {
			CallID     string `json:"call_id"`
			SideEffect string `json:"sideEffect"`
			NextAction struct {
				Command     string `json:"command"`
				Instruction string `json:"instruction"`
			} `json:"nextAction"`
		}
		if json.Unmarshal(data, &failure) == nil {
			var output strings.Builder
			if failure.CallID != "" {
				fmt.Fprintf(&output, "Call: %s\n", terminalSafe(failure.CallID))
			}
			if failure.NextAction.Command != "" {
				fmt.Fprintf(&output, "Next:\n  %s\n", terminalSafe(failure.NextAction.Command))
			} else if failure.NextAction.Instruction != "" {
				fmt.Fprintf(&output, "Next:\n  %s\n", terminalSafe(failure.NextAction.Instruction))
			}
			switch failure.SideEffect {
			case "none":
				output.WriteString("Nothing was sent upstream.\n")
			case "possible":
				output.WriteString("The upstream outcome may be partial or unknown. Check before retrying.\n")
			}
			if output.Len() > 0 {
				_, err := fmt.Fprint(cmd.OutOrStdout(), output.String())
				return err
			}
		}
		return nil
	}
	return writeCommandOutput(cmd, request, data)
}

func writeMachineData(cmd *cobra.Command, data json.RawMessage) error {
	payload := bytes.TrimSpace(data)
	if !json.Valid(payload) {
		return fmt.Errorf("daemon returned invalid JSON")
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), string(payload))
	return err
}

func writeMachineError(cmd *cobra.Command, remote *rpcError) error {
	payload := struct {
		Code  string          `json:"code"`
		Error string          `json:"error"`
		Data  json.RawMessage `json:"data,omitempty"`
	}{Code: remote.code, Error: remote.message}
	if len(remote.data) > 0 && !isNullJSON(remote.data) {
		if !json.Valid(remote.data) {
			return fmt.Errorf("daemon returned invalid error data")
		}
		payload.Data = remote.data
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode daemon error: %w", err)
	}
	return writeMachineData(cmd, data)
}

func writeTopLevelMachineError(cmd *cobra.Command, err error) error {
	var remote *rpcError
	if errors.As(err, &remote) && remote != nil {
		return writeMachineError(cmd, remote)
	}
	code := "invalid_request"
	var data any
	var transport *rpcTransportError
	if errors.As(err, &transport) && transport != nil {
		code = "transport_error"
		if transport.action == "run" {
			sideEffect := "none"
			if transport.requestMayHaveSent {
				sideEffect = "possible"
			}
			data = map[string]any{
				"sideEffect": sideEffect,
				"retryable":  !transport.requestMayHaveSent,
				"nextAction": map[string]string{
					"instruction": "Check runtime and upstream state before deciding whether to retry",
				},
			}
		}
	}
	payload, marshalErr := json.Marshal(struct {
		Code  string `json:"code"`
		Error string `json:"error"`
		Data  any    `json:"data,omitempty"`
	}{Code: code, Error: err.Error(), Data: data})
	if marshalErr != nil {
		return fmt.Errorf("encode command error: %w", marshalErr)
	}
	return writeMachineData(cmd, payload)
}

func isNullJSON(data json.RawMessage) bool {
	payload := bytes.TrimSpace(data)
	return len(payload) == 0 || bytes.Equal(payload, []byte("null"))
}

func humanResponse(request api.Request, data json.RawMessage) (string, error) {
	switch request.Action {
	case "connect":
		return humanConnect(data)
	case "status":
		return humanStatus(data)
	case "doctor":
		return humanDoctor(data)
	case "secret.set":
		return fmt.Sprintf("Stored %s\n", terminalSafe(request.Name)), nil
	case "secret.list":
		return humanSecretList(data)
	case "secret.unset":
		return fmt.Sprintf("Removed %s\n", terminalSafe(request.Name)), nil
	case "search":
		return humanSearch(data)
	case "run":
		var output strings.Builder
		output.WriteString("Result:\n")
		if err := appendPrettyJSON(&output, data, "  "); err != nil {
			return "", err
		}
		return output.String(), nil
	case "disconnect":
		return fmt.Sprintf("Disconnected %s\n", terminalSafe(request.Name)), nil
	default:
		return "", fmt.Errorf("no human-readable output for action %q", request.Action)
	}
}

func humanSecretList(data json.RawMessage) (string, error) {
	var statuses []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	if err := decodeResponse("secret list", data, &statuses); err != nil {
		return "", err
	}
	if len(statuses) == 0 {
		return "No secrets found.\n", nil
	}
	var output strings.Builder
	output.WriteString("Secrets\n")
	for _, status := range statuses {
		fmt.Fprintf(&output, "  %s: %s\n", terminalSafe(status.Name), terminalSafe(status.State))
	}
	return output.String(), nil
}

func humanDoctor(data json.RawMessage) (string, error) {
	var report struct {
		Healthy bool `json:"healthy"`
		Fixed   int  `json:"fixed"`
		Checks  []struct {
			Name       string `json:"name"`
			State      string `json:"state"`
			Message    string `json:"message"`
			NextAction string `json:"nextAction"`
		} `json:"checks"`
	}
	if err := decodeResponse("doctor", data, &report); err != nil {
		return "", err
	}
	var output strings.Builder
	if report.Healthy {
		if report.Fixed > 0 {
			fmt.Fprintf(&output, "Healthy (%d fixed)\n", report.Fixed)
		} else {
			output.WriteString("Healthy\n")
		}
	} else {
		output.WriteString("Problems found\n")
	}
	for _, check := range report.Checks {
		fmt.Fprintf(&output, "  %s: %s — %s\n", terminalSafe(check.Name), terminalSafe(check.State), terminalSafe(check.Message))
		if check.NextAction != "" {
			fmt.Fprintf(&output, "    Next: %s\n", terminalSafe(check.NextAction))
		}
	}
	return output.String(), nil
}

func humanConnect(data json.RawMessage) (string, error) {
	var result connectResult
	if err := decodeResponse("connect", data, &result); err != nil {
		return "", err
	}
	capabilities := result.Capabilities
	source := result.Source
	if source == "" {
		source = path.Join(".9a", "integrations", result.Name+".yaml")
	}
	noun := "capabilities"
	if len(capabilities) == 1 {
		noun = "capability"
	}
	var output strings.Builder
	fmt.Fprintf(&output, "Connected %s (%d %s)\n", terminalSafe(result.Name), len(capabilities), noun)
	if len(capabilities) > 0 {
		fmt.Fprintf(&output, "\nNext:\n  9a search %s --json\n", terminalSafe(result.Name))
	}
	fmt.Fprintf(&output, "\nSource:\n  %s\n", terminalSafe(source))
	return output.String(), nil
}

func humanStatus(data json.RawMessage) (string, error) {
	var status struct {
		State        string `json:"state"`
		Message      string `json:"message"`
		Integrations []struct {
			Name           string   `json:"name"`
			State          string   `json:"state"`
			Capabilities   int      `json:"capabilities"`
			MissingSecrets []string `json:"missingSecrets"`
			Message        string   `json:"message"`
		} `json:"integrations"`
	}
	if err := decodeResponse("status", data, &status); err != nil {
		return "", err
	}
	if status.State == "empty" {
		return "Not ready\n  No integrations connected.\n  Next: 9a connect <manifest.yaml>\n", nil
	}

	heading := map[string]string{
		"ready":        "Ready",
		"needs-secret": "Needs secret",
		"broken":       "Broken",
	}[status.State]
	if heading == "" {
		return "", fmt.Errorf("decode status response: unknown state %q", status.State)
	}
	var output strings.Builder
	output.WriteString(heading)
	output.WriteByte('\n')
	if status.Message != "" {
		fmt.Fprintf(&output, "  Workspace: broken — %s\n", terminalSafe(status.Message))
		output.WriteString("    Next: 9a doctor\n")
	}
	for _, integration := range status.Integrations {
		noun := "capabilities"
		if integration.Capabilities == 1 {
			noun = "capability"
		}
		fmt.Fprintf(&output, "  %s: %s (%d %s)", terminalSafe(integration.Name), terminalSafe(integration.State), integration.Capabilities, noun)
		if integration.Message != "" {
			fmt.Fprintf(&output, " — %s", terminalSafe(integration.Message))
		}
		output.WriteByte('\n')
		for _, reference := range integration.MissingSecrets {
			fmt.Fprintf(&output, "    Missing: %s\n", terminalSafe(reference))
			fmt.Fprintf(&output, "    Next: 9a secret set %s\n", terminalSafe(reference))
		}
		if integration.State == "broken" {
			output.WriteString("    Next: 9a doctor\n")
		}
	}
	return output.String(), nil
}

func humanSearch(data json.RawMessage) (string, error) {
	var results []struct {
		Ref              string `json:"ref"`
		Name             string `json:"name"`
		Description      string `json:"description"`
		RequiresApproval bool   `json:"requiresApproval"`
	}
	if err := decodeResponse("search", data, &results); err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "No capabilities found.\nNext: try a broader query or run 9a status.\n", nil
	}
	var output strings.Builder
	fmt.Fprintf(&output, "Capabilities (%d)\n", len(results))
	for _, result := range results {
		ref := result.Ref
		fmt.Fprintf(&output, "  %s\n", terminalSafe(ref))
		if result.Name != "" {
			fmt.Fprintf(&output, "    Name: %s\n", singleLine(result.Name))
		}
		if result.Description != "" {
			fmt.Fprintf(&output, "    Description: %s\n", singleLine(result.Description))
		}
		if result.RequiresApproval {
			output.WriteString("    Approval: required before execution\n")
		}
		fmt.Fprintf(&output, "    Inspect: 9a search %s --json\n", terminalSafe(ref))
	}
	return output.String(), nil
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
