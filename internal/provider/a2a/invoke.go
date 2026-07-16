package a2a

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/secret"
)

var ErrInvalidInput = errors.New("invalid A2A invoke input")

type inputConfiguration struct {
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
	HistoryLength       *int     `json:"historyLength,omitempty"`
	ReturnImmediately   *bool    `json:"returnImmediately,omitempty"`
}

type invokeInput struct {
	Parts         []json.RawMessage   `json:"parts"`
	Configuration *inputConfiguration `json:"configuration,omitempty"`
	Metadata      json.RawMessage     `json:"metadata,omitempty"`
}

func invokeInputSchema() map[string]any {
	part := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"text": map[string]any{
				"type":      "string",
				"maxLength": maxBodyBytes,
			},
			"raw": map[string]any{
				"type":      "string",
				"maxLength": 4 * ((maxBodyBytes + 2) / 3),
				"pattern":   `^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`,
			},
			"url": map[string]any{
				"type":      "string",
				"minLength": 1,
				"maxLength": maxStringBytes / utf8.UTFMax,
				"anyOf": []any{
					map[string]any{"pattern": `^https?://[A-Za-z0-9.-]+(?::[0-9]{1,5})?(?:[/?#](?:[A-Za-z0-9._~:/?#@!$&'()*+,;=\[\]-]|%[0-9A-Fa-f]{2})*)?$`},
					map[string]any{
						"allOf": []any{
							map[string]any{"pattern": `^[A-Za-z][A-Za-z0-9+.-]*:(?:[A-Za-z0-9._~:/?#@!$&'()*+,;=\[\]-]|%[0-9A-Fa-f]{2})+$`},
							map[string]any{"not": map[string]any{"pattern": `^https?:`}},
						},
					},
				},
			},
			"data":     map[string]any{},
			"metadata": map[string]any{"type": "object"},
			"filename": map[string]any{
				"type":      "string",
				"minLength": 1,
				"maxLength": maxStringBytes / utf8.UTFMax,
			},
			"mediaType": map[string]any{
				"type":      "string",
				"minLength": 3,
				"maxLength": maxStringBytes,
				"pattern":   "^[A-Za-z0-9!#$%&'*+.^_`|~-]+/[A-Za-z0-9!#$%&'*+.^_`|~-]+$",
			},
		},
		"oneOf": []any{
			map[string]any{"required": []string{"text"}},
			map[string]any{"required": []string{"raw"}},
			map[string]any{"required": []string{"url"}},
			map[string]any{"required": []string{"data"}},
		},
	}
	return map[string]any{
		"type":                 "object",
		"required":             []string{"parts"},
		"additionalProperties": false,
		"properties": map[string]any{
			"parts": map[string]any{
				"type":     "array",
				"minItems": 1,
				"maxItems": maxListItems,
				"items":    part,
			},
			"configuration": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"acceptedOutputModes": map[string]any{
						"type":     "array",
						"maxItems": maxListItems,
						"items": map[string]any{
							"type":      "string",
							"minLength": 1,
							"maxLength": maxStringBytes / utf8.UTFMax,
						},
					},
					"historyLength": map[string]any{
						"type":    "integer",
						"minimum": 0,
						"maximum": maxListItems,
					},
				},
			},
			"metadata": map[string]any{"type": "object"},
		},
	}
}

type sendConfiguration struct {
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
	HistoryLength       *int     `json:"historyLength,omitempty"`
	ReturnImmediately   bool     `json:"returnImmediately"`
}

type sendMessageRequest struct {
	Tenant        string            `json:"tenant,omitempty"`
	Message       outboundMessage   `json:"message"`
	Configuration sendConfiguration `json:"configuration"`
	Metadata      json.RawMessage   `json:"metadata,omitempty"`
}

type outboundMessage struct {
	MessageID string            `json:"messageId"`
	Role      string            `json:"role"`
	Parts     []json.RawMessage `json:"parts"`
}

type partWire struct {
	Text      *string         `json:"text,omitempty"`
	Raw       *string         `json:"raw,omitempty"`
	URL       *string         `json:"url,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Filename  *string         `json:"filename,omitempty"`
	MediaType *string         `json:"mediaType,omitempty"`
}

type taskStatus struct {
	State     string          `json:"state"`
	Message   json.RawMessage `json:"message,omitempty"`
	Timestamp string          `json:"timestamp,omitempty"`
}

type taskArtifact struct {
	ArtifactID string            `json:"artifactId"`
	Name       string            `json:"name,omitempty"`
	Parts      []json.RawMessage `json:"parts"`
}

type taskResponse struct {
	ID        string            `json:"id"`
	ContextID string            `json:"contextId,omitempty"`
	Status    json.RawMessage   `json:"status"`
	Artifacts []json.RawMessage `json:"artifacts,omitempty"`
	History   []json.RawMessage `json:"history,omitempty"`
	Metadata  json.RawMessage   `json:"metadata,omitempty"`
}

type messageWire struct {
	MessageID        string            `json:"messageId"`
	ContextID        string            `json:"contextId,omitempty"`
	TaskID           string            `json:"taskId,omitempty"`
	Role             string            `json:"role"`
	Parts            []json.RawMessage `json:"parts"`
	Metadata         json.RawMessage   `json:"metadata,omitempty"`
	Extensions       []string          `json:"extensions,omitempty"`
	ReferenceTaskIDs []string          `json:"referenceTaskIds,omitempty"`
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func parseInvokeInput(data json.RawMessage) (invokeInput, error) {
	if len(data) == 0 || len(data) > maxBodyBytes || !json.Valid(data) {
		return invokeInput{}, ErrInvalidInput
	}
	var input invokeInput
	if err := decodeStrict(data, &input); err != nil || len(input.Parts) == 0 || len(input.Parts) > maxListItems {
		return invokeInput{}, ErrInvalidInput
	}
	for _, part := range input.Parts {
		if validatePart(part) != nil {
			return invokeInput{}, ErrInvalidInput
		}
	}
	if input.Configuration != nil {
		if input.Configuration.ReturnImmediately != nil || !validateStrings(input.Configuration.AcceptedOutputModes, false) || input.Configuration.HistoryLength != nil && (*input.Configuration.HistoryLength < 0 || *input.Configuration.HistoryLength > maxListItems) {
			return invokeInput{}, ErrInvalidInput
		}
	}
	if len(input.Metadata) != 0 && !validJSONObject(input.Metadata) {
		return invokeInput{}, ErrInvalidInput
	}
	return input, nil
}

func validJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

func validatePart(raw json.RawMessage) error {
	var part partWire
	if err := decodeStrict(raw, &part); err != nil {
		return errors.New("invalid A2A Part")
	}
	contentFields := 0
	for _, present := range []bool{part.Text != nil, part.Raw != nil, part.URL != nil, len(part.Data) != 0} {
		if present {
			contentFields++
		}
	}
	if contentFields != 1 || len(part.Data) != 0 && !json.Valid(part.Data) || !validJSONObject(part.Metadata) {
		return errors.New("invalid A2A Part")
	}
	if part.Text != nil && (len(*part.Text) > maxBodyBytes || !utf8.ValidString(*part.Text)) {
		return errors.New("invalid A2A Part")
	}
	if part.Raw != nil {
		decoded, err := base64.StdEncoding.DecodeString(*part.Raw)
		if err != nil || len(decoded) > maxBodyBytes {
			return errors.New("invalid A2A Part")
		}
	}
	if part.URL != nil {
		parsed, err := url.ParseRequestURI(*part.URL)
		if err != nil || *part.URL == "" || !parsed.IsAbs() || len(*part.URL) > maxStringBytes {
			return errors.New("invalid A2A Part")
		}
		if (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host == "" {
			return errors.New("invalid A2A Part")
		}
	}
	if part.Filename != nil && (*part.Filename == "" || len(*part.Filename) > maxStringBytes || !utf8.ValidString(*part.Filename)) {
		return errors.New("invalid A2A Part")
	}
	if part.MediaType != nil {
		mediaType, _, err := mime.ParseMediaType(*part.MediaType)
		if err != nil || mediaType == "" || len(*part.MediaType) > maxStringBytes || !utf8.ValidString(*part.MediaType) {
			return errors.New("invalid A2A Part")
		}
	}
	return nil
}

func newMessageID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func (a *Adapter) cachedOrResolve(ctx context.Context, p provider.Provider) (resolvedProvider, error) {
	a.mu.Lock()
	resolved, ok := a.cache[p.ID]
	a.mu.Unlock()
	if ok {
		return resolved, nil
	}
	_, resolved, err := a.resolve(ctx, p)
	return resolved, err
}

func (a *Adapter) operationJSON(ctx context.Context, p provider.Provider, bearer bool, method, endpoint string, requestBody any) ([]byte, error) {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return nil, adapterError("invalid_request", "A2A request could not be encoded")
		}
		if len(encoded) > maxBodyBytes {
			return nil, adapterError("invalid_request", "A2A request exceeds limit")
		}
		body = bytes.NewReader(encoded)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, endpoint, body)
	if err != nil {
		return nil, adapterError("invalid_request", "A2A request could not be constructed")
	}
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/a2a+json")
	}
	request.Header.Set("Accept", "application/a2a+json")
	request.Header.Set("A2A-Version", protocolVersion)
	if bearer {
		reference := p.Config["credential_reference"]
		if reference == "" || a.resolver == nil {
			return nil, &secret.MissingError{Reference: reference}
		}
		token, resolveErr := a.resolver.Resolve(secret.WithWorkspace(ctx, p.Config["workspace_root"]), reference)
		if resolveErr != nil {
			return nil, resolveErr
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := a.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, adapterError("a2a_unavailable", "A2A operation endpoint is unavailable")
	}
	responseBody, err := readBoundedJSON(response, "application/a2a+json")
	closeErr := response.Body.Close()
	if err != nil {
		return nil, adapterError("invalid_response", "A2A operation returned an invalid response")
	}
	if closeErr != nil {
		return nil, adapterError("invalid_response", "A2A operation returned an invalid response")
	}
	return responseBody, nil
}

func parseTask(raw json.RawMessage, expectedID string) (taskResponse, taskStatus, error) {
	var task taskResponse
	if err := json.Unmarshal(raw, &task); err != nil || task.ID == "" || len(task.ID) > maxStringBytes || expectedID != "" && task.ID != expectedID || len(task.ContextID) > maxStringBytes || len(task.Artifacts) > maxListItems || len(task.History) > maxListItems || !validJSONObject(task.Metadata) {
		return taskResponse{}, taskStatus{}, errors.New("invalid A2A Task response")
	}
	var status taskStatus
	if len(task.Status) == 0 || json.Unmarshal(task.Status, &status) != nil || status.State == "" || len(status.State) > maxStringBytes || status.Timestamp != "" && !validTimestamp(status.Timestamp) {
		return taskResponse{}, taskStatus{}, errors.New("invalid A2A Task status")
	}
	if len(status.Message) != 0 {
		if err := validateMessage(status.Message, true, task.ID, task.ContextID); err != nil {
			return taskResponse{}, taskStatus{}, errors.New("invalid A2A Task status message")
		}
	}
	for _, historyMessage := range task.History {
		if err := validateMessage(historyMessage, false, task.ID, task.ContextID); err != nil {
			return taskResponse{}, taskStatus{}, errors.New("invalid A2A Task history message")
		}
	}
	return task, status, nil
}

func validTimestamp(value string) bool {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func canonicalJSONHash(raw json.RawMessage) ([32]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return [32]byte{}, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

func emitTaskUpdate(task taskResponse, status taskStatus, previousStatus *string, seenArtifacts map[string][32]byte, sink provider.Sink) (bool, error) {
	changed := false
	statusKey := string(task.Status)
	if statusKey != *previousStatus {
		if err := sink.Event(provider.Event{Type: "status", Data: append(json.RawMessage(nil), task.Status...)}); err != nil {
			return false, fmt.Errorf("persist A2A status: %w", err)
		}
		*previousStatus = statusKey
		changed = true
	}
	for _, raw := range task.Artifacts {
		var artifact taskArtifact
		if err := json.Unmarshal(raw, &artifact); err != nil || artifact.ArtifactID == "" || len(artifact.ArtifactID) > maxStringBytes || len(artifact.Parts) == 0 || len(artifact.Parts) > maxListItems {
			return false, adapterError("invalid_response", "invalid A2A Task artifact")
		}
		for _, part := range artifact.Parts {
			if err := validatePart(part); err != nil {
				return false, adapterError("invalid_response", "invalid A2A Task artifact")
			}
		}
		hash, err := canonicalJSONHash(raw)
		if err != nil {
			return false, adapterError("invalid_response", "invalid A2A Task artifact")
		}
		if previous, exists := seenArtifacts[artifact.ArtifactID]; exists && previous == hash {
			continue
		}
		if _, exists := seenArtifacts[artifact.ArtifactID]; !exists && len(seenArtifacts) >= maxListItems {
			return false, adapterError("resource_exhausted", "A2A Task artifact tracking limit exceeded")
		}
		name := capability.Slug(artifact.Name)
		if name == "" {
			name = capability.Slug(artifact.ArtifactID)
		}
		if name == "" {
			name = "artifact"
		}
		if err := sink.Artifact(name, "application/json", append([]byte(nil), raw...)); err != nil {
			return false, fmt.Errorf("persist A2A artifact: %w", err)
		}
		seenArtifacts[artifact.ArtifactID] = hash
		changed = true
	}
	return changed, nil
}

func terminalTaskError(state string) error {
	code, message := "upstream_error", "A2A task entered an unsupported terminal state"
	switch state {
	case "TASK_STATE_FAILED":
		code, message = "failed", "A2A task failed"
	case "TASK_STATE_CANCELED":
		code, message = "canceled", "A2A task was canceled"
	case "TASK_STATE_REJECTED":
		code, message = "rejected", "A2A task was rejected"
	case "TASK_STATE_INPUT_REQUIRED":
		code, message = "input_required", "A2A task requires additional input"
	case "TASK_STATE_AUTH_REQUIRED":
		code, message = "auth_required", "A2A task requires authentication"
	}
	adapterErr, _ := provider.NewAdapterError(code, message)
	return adapterErr
}

func taskEndpoint(baseURL, taskID, tenant, suffix string) string {
	endpoint := strings.TrimRight(baseURL, "/") + "/tasks/" + url.PathEscape(taskID) + suffix
	if tenant != "" {
		endpoint += "?tenant=" + url.QueryEscape(tenant)
	}
	return endpoint
}

func terminalTaskState(state string) bool {
	return state != "TASK_STATE_SUBMITTED" && state != "TASK_STATE_WORKING"
}

func (a *Adapter) claimPollTerminal(invocationID string, active *activeTask, state string) (persist, waitForCancel bool) {
	active.terminalMu.Lock()
	defer active.terminalMu.Unlock()
	if active.owner == terminalCancel {
		if state == "TASK_STATE_CANCELED" {
			return true, false
		}
		return false, true
	}
	if active.owner != terminalUnclaimed {
		return false, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active[invocationID] != active {
		return false, false
	}
	active.owner = terminalPoll
	delete(a.active, invocationID)
	return true, false
}

func (a *Adapter) finishTaskExit(invocationID string, active *activeTask, localErr error) error {
	active.terminalMu.Lock()
	defer active.terminalMu.Unlock()
	if active.owner == terminalCancel {
		select {
		case <-active.updates:
		default:
		}
		return terminalTaskError("TASK_STATE_CANCELED")
	}
	if active.owner == terminalPoll {
		return localErr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active[invocationID] == active {
		active.owner = terminalPoll
		delete(a.active, invocationID)
	}
	return localErr
}

func (a *Adapter) pollTask(ctx context.Context, p provider.Provider, invocationID string, resolved resolvedProvider, bearer bool, initial json.RawMessage, sink provider.Sink) (resultErr error) {
	task, status, err := parseTask(initial, "")
	if err != nil {
		return adapterError("invalid_response", "invalid A2A Task response")
	}
	taskCtx, cancel := context.WithTimeout(ctx, a.taskTimeout)
	active := &activeTask{providerID: p.ID, taskID: task.ID, tenant: resolved.tenant, bearer: bearer, cancel: cancel, updates: make(chan json.RawMessage, 1)}
	a.mu.Lock()
	if _, exists := a.active[invocationID]; exists {
		a.mu.Unlock()
		cancel()
		return adapterError("invocation_conflict", "A2A invocation ID is already active")
	}
	a.active[invocationID] = active
	a.mu.Unlock()
	defer func() {
		cancel()
		resultErr = a.finishTaskExit(invocationID, active, resultErr)
	}()
	if err := sink.Started(); err != nil {
		return err
	}
	previousStatus := ""
	seenArtifacts := make(map[string][32]byte)
	pollInterval := a.pollInterval
	for {
		if terminalTaskState(status.State) {
			persist, waitForCancel := a.claimPollTerminal(invocationID, active, status.State)
			if waitForCancel {
				update := <-active.updates
				next, nextStatus, parseErr := parseTask(update, task.ID)
				if parseErr != nil {
					return adapterError("invalid_response", "invalid A2A Task response")
				}
				task, status, initial = next, nextStatus, update
				continue
			}
			if !persist {
				return adapterError("not_cancelable", "A2A invocation is no longer active")
			}
		}
		changed, err := emitTaskUpdate(task, status, &previousStatus, seenArtifacts, sink)
		if err != nil {
			return err
		}
		if changed {
			pollInterval = a.pollInterval
		} else if pollInterval < a.maxPollInterval {
			pollInterval *= 2
			if pollInterval > a.maxPollInterval {
				pollInterval = a.maxPollInterval
			}
		}
		switch status.State {
		case "TASK_STATE_COMPLETED":
			return sink.Event(provider.Event{Type: "result", Data: append(json.RawMessage(nil), initial...)})
		case "TASK_STATE_SUBMITTED", "TASK_STATE_WORKING":
		case "TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_REJECTED", "TASK_STATE_INPUT_REQUIRED", "TASK_STATE_AUTH_REQUIRED":
			return terminalTaskError(status.State)
		default:
			return terminalTaskError(status.State)
		}
		timer := time.NewTimer(pollInterval)
		select {
		case update := <-active.updates:
			timer.Stop()
			next, nextStatus, parseErr := parseTask(update, task.ID)
			if parseErr != nil {
				return adapterError("invalid_response", "invalid A2A Task response")
			}
			task, status, initial = next, nextStatus, update
			continue
		case <-taskCtx.Done():
			timer.Stop()
			select {
			case update := <-active.updates:
				next, nextStatus, parseErr := parseTask(update, task.ID)
				if parseErr != nil {
					return adapterError("invalid_response", "invalid A2A Task response")
				}
				task, status, initial = next, nextStatus, update
				continue
			default:
				if errors.Is(taskCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
					return adapterError("a2a_timeout", "A2A task lifetime exceeded")
				}
				return taskCtx.Err()
			}
		case <-timer.C:
		}
		endpoint := taskEndpoint(resolved.baseURL, task.ID, resolved.tenant, "")
		body, err := a.operationJSON(taskCtx, p, bearer, http.MethodGet, endpoint, nil)
		if err != nil {
			select {
			case update := <-active.updates:
				body = update
			default:
				if errors.Is(taskCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
					return adapterError("a2a_timeout", "A2A task lifetime exceeded")
				}
				return err
			}
		}
		next, nextStatus, err := parseTask(body, task.ID)
		if err != nil {
			return adapterError("invalid_response", "invalid A2A Task response")
		}
		task, status, initial = next, nextStatus, body
	}
}

func validateMessage(raw json.RawMessage, requireServer bool, expectedTaskID, expectedContextID string) error {
	var message messageWire
	if err := json.Unmarshal(raw, &message); err != nil || message.MessageID == "" || len(message.MessageID) > maxStringBytes || len(message.ContextID) > maxStringBytes || len(message.TaskID) > maxStringBytes || len(message.Parts) == 0 || len(message.Parts) > maxListItems || !validJSONObject(message.Metadata) || !validateStrings(message.Extensions, false) || !validateStrings(message.ReferenceTaskIDs, false) {
		return errors.New("invalid A2A Message response")
	}
	if message.Role != "ROLE_USER" && message.Role != "ROLE_AGENT" || requireServer && message.Role != "ROLE_AGENT" || message.Role == "ROLE_AGENT" && message.ContextID == "" {
		return errors.New("invalid A2A Message response")
	}
	if expectedTaskID != "" && message.TaskID != "" && message.TaskID != expectedTaskID || expectedContextID != "" && message.ContextID != "" && message.ContextID != expectedContextID {
		return errors.New("invalid A2A Message response")
	}
	for _, part := range message.Parts {
		if err := validatePart(part); err != nil {
			return errors.New("invalid A2A Message response")
		}
	}
	return nil
}

func validateResponseMessage(raw json.RawMessage) error {
	var message messageWire
	if err := json.Unmarshal(raw, &message); err != nil || message.TaskID != "" {
		return errors.New("invalid direct A2A Message response")
	}
	return validateMessage(raw, true, "", "")
}

func (a *Adapter) Invoke(ctx context.Context, p provider.Provider, c capability.Capability, invocationID string, data json.RawMessage, sink provider.Sink) error {
	input, err := parseInvokeInput(data)
	if err != nil {
		return err
	}
	if !a.acquireInvocation() {
		return adapterError("resource_exhausted", "A2A invocation limit reached")
	}
	defer a.releaseInvocation()
	resolved, err := a.cachedOrResolve(ctx, p)
	if err != nil {
		return err
	}
	bearer := resolved.authBySkill[c.Source.UpstreamName]
	messageID, err := newMessageID()
	if err != nil {
		return err
	}
	configuration := sendConfiguration{ReturnImmediately: true}
	if input.Configuration != nil {
		configuration.AcceptedOutputModes = input.Configuration.AcceptedOutputModes
		configuration.HistoryLength = input.Configuration.HistoryLength
	}
	request := sendMessageRequest{
		Tenant: resolved.tenant, Message: outboundMessage{MessageID: messageID, Role: "ROLE_USER", Parts: input.Parts},
		Configuration: configuration, Metadata: input.Metadata,
	}
	endpoint, err := url.JoinPath(resolved.baseURL, "message:send")
	if err != nil {
		return adapterError("invalid_request", "A2A operation endpoint is invalid")
	}
	body, err := a.operationJSON(ctx, p, bearer, http.MethodPost, endpoint, request)
	if err != nil {
		return err
	}
	var response struct {
		Message json.RawMessage `json:"message"`
		Task    json.RawMessage `json:"task"`
	}
	if err := json.Unmarshal(body, &response); err != nil || (len(response.Message) == 0) == (len(response.Task) == 0) {
		return adapterError("invalid_response", "invalid A2A SendMessage response")
	}
	if len(response.Task) != 0 {
		return a.pollTask(ctx, p, invocationID, resolved, bearer, response.Task, sink)
	}
	if err := validateResponseMessage(response.Message); err != nil {
		return adapterError("invalid_response", "invalid A2A Message response")
	}
	if err := sink.Started(); err != nil {
		return err
	}
	if err := sink.Event(provider.Event{Type: "result", Data: append(json.RawMessage(nil), response.Message...)}); err != nil {
		return fmt.Errorf("persist A2A result: %w", err)
	}
	return nil
}
