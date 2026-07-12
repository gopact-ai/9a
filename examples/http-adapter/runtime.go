package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"unicode/utf8"
)

const (
	adapterProtocolVersion = "9a.adapter/v1"
	maxProtocolLineBytes   = 8 << 20
	maxActiveInvokes       = 32
)

var errUnterminatedProtocolLine = errors.New("adapter request is missing newline terminator")

type protocolRequest struct {
	Version string          `json:"version"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type protocolResponse struct {
	Version string          `json:"version"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *adapterFault   `json:"error,omitempty"`
}

type contractDTO struct {
	Mode       string         `json:"mode"`
	JSONSchema map[string]any `json:"json_schema"`
}

type lifecycleDTO struct {
	Sync       bool `json:"sync"`
	Streaming  bool `json:"streaming"`
	MultiTurn  bool `json:"multi_turn"`
	Cancelable bool `json:"cancelable"`
}

type securityDTO struct {
	RequiresApproval string `json:"requires_approval"`
	UpstreamAuth     string `json:"upstream_auth"`
}

type capabilityDTO struct {
	UpstreamName string       `json:"upstream_name"`
	Kind         string       `json:"kind"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Input        contractDTO  `json:"input"`
	Output       contractDTO  `json:"output"`
	Lifecycle    lifecycleDTO `json:"lifecycle"`
	Security     securityDTO  `json:"security"`
	Tags         []string     `json:"tags,omitempty"`
	Examples     []string     `json:"examples,omitempty"`
}

type adapterRuntime struct {
	manifest *manifest
	bridge   *httpBridge
	input    io.Reader
	output   io.Writer
	stderr   io.Writer

	writeMu  sync.Mutex
	activeMu sync.Mutex
	active   map[string]struct{}
	invokes  chan struct{}
	wg       sync.WaitGroup
}

func newRuntime(configuration *manifest, input io.Reader, output, stderr io.Writer) *adapterRuntime {
	return &adapterRuntime{
		manifest: configuration,
		bridge:   newHTTPBridge(configuration),
		input:    input,
		output:   output,
		stderr:   stderr,
		active:   make(map[string]struct{}),
		invokes:  make(chan struct{}, maxActiveInvokes),
	}
}

func (r *adapterRuntime) serve(ctx context.Context) error {
	scanner := bufio.NewScanner(r.input)
	scanner.Buffer(make([]byte, 64<<10), maxProtocolLineBytes)
	scanner.Split(splitTerminatedProtocolLine)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if !utf8.Valid(line) {
			r.diagnostic("adapter request is not valid UTF-8")
			r.wg.Wait()
			return errors.New("invalid UTF-8 adapter request")
		}
		request, err := parseProtocolRequest(line)
		if err != nil {
			r.diagnostic("adapter request is malformed")
			r.wg.Wait()
			return err
		}
		if !r.reserveID(request.ID) {
			r.respondError(request.ID, safeFault("duplicate_id", "request ID is already active", nil))
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer r.releaseID(request.ID)
			r.dispatch(ctx, request)
		}()
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, errUnterminatedProtocolLine) {
			r.diagnostic("adapter request is missing a newline terminator")
		} else {
			r.diagnostic("adapter request exceeds the protocol line limit")
		}
		r.wg.Wait()
		return err
	}
	r.wg.Wait()
	return nil
}

func splitTerminatedProtocolLine(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
		end := newline
		if end > 0 && data[end-1] == '\r' {
			end--
		}
		return newline + 1, data[:end], nil
	}
	if atEOF && len(data) != 0 {
		return 0, nil, errUnterminatedProtocolLine
	}
	return 0, nil, nil
}

func parseProtocolRequest(line []byte) (protocolRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	var request protocolRequest
	if err := decoder.Decode(&request); err != nil {
		return protocolRequest{}, errors.New("malformed adapter request")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return protocolRequest{}, errors.New("adapter request contains multiple JSON values")
	}
	if request.ID == "" || len(request.ID) > maxManifestStringBytes || !utf8.ValidString(request.ID) || request.Method == "" || len(request.Method) > 128 || request.Params == nil {
		return protocolRequest{}, errors.New("invalid adapter request envelope")
	}
	return request, nil
}

func (r *adapterRuntime) dispatch(ctx context.Context, request protocolRequest) {
	if request.Version != adapterProtocolVersion {
		r.respondError(request.ID, safeFault("unsupported_version", "expected 9a.adapter/v1", nil))
		return
	}
	switch request.Method {
	case "discover":
		r.discover(request)
	case "health":
		r.health(ctx, request)
	case "invoke":
		r.invoke(ctx, request)
	case "cancel":
		r.respondError(request.ID, safeFault("not_cancelable", "HTTP operations are not cancelable", nil))
	default:
		r.respondError(request.ID, safeFault("unsupported_method", "adapter method is not supported", nil))
	}
}

func (r *adapterRuntime) discover(request protocolRequest) {
	var params struct {
		Provider providerConfig `json:"provider"`
	}
	if json.Unmarshal(request.Params, &params) != nil {
		r.respondError(request.ID, safeFault("invalid_request", "discover params are invalid", nil))
		return
	}
	if _, fault := validateProvider(params.Provider); fault != nil {
		r.respondError(request.ID, fault)
		return
	}
	capabilities := make([]capabilityDTO, 0, len(r.manifest.Operations))
	for _, item := range r.manifest.Operations {
		upstreamAuth := "none"
		if item.Auth == "bearer" {
			upstreamAuth = "adapter-configured"
		}
		capabilities = append(capabilities, capabilityDTO{
			UpstreamName: item.UpstreamName, Kind: "api.operation", Name: item.Name, Description: item.Description,
			Input: contractDTO{Mode: "json", JSONSchema: cloneJSONObject(item.InputSchema)}, Output: contractDTO{Mode: "json", JSONSchema: cloneJSONObject(item.OutputSchema)},
			Lifecycle: lifecycleDTO{Sync: true}, Security: securityDTO{RequiresApproval: item.RequiresApproval, UpstreamAuth: upstreamAuth},
			Tags: append([]string(nil), item.Tags...), Examples: append([]string(nil), item.Examples...),
		})
	}
	r.respondResult(request.ID, map[string]any{"capabilities": capabilities})
}

func (r *adapterRuntime) health(ctx context.Context, request protocolRequest) {
	var params struct {
		Provider providerConfig `json:"provider"`
	}
	if json.Unmarshal(request.Params, &params) != nil {
		r.respondResult(request.ID, map[string]any{"healthy": false, "message": "invalid provider configuration"})
		return
	}
	if fault := r.bridge.health(ctx, params.Provider); fault != nil {
		r.respondResult(request.ID, map[string]any{"healthy": false, "message": fault.Message})
		return
	}
	message := "configuration valid"
	if r.manifest.HealthPath != "" {
		message = "ok"
	}
	r.respondResult(request.ID, map[string]any{"healthy": true, "message": message})
}

func (r *adapterRuntime) invoke(ctx context.Context, request protocolRequest) {
	var params struct {
		Provider   providerConfig `json:"provider"`
		Capability struct {
			UpstreamName string `json:"upstream_name"`
		} `json:"capability"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(request.Params, &params) != nil || params.Capability.UpstreamName == "" || params.Input == nil {
		r.respondError(request.ID, safeFault("invalid_request", "invoke params are invalid", nil))
		return
	}
	select {
	case r.invokes <- struct{}{}:
		defer func() { <-r.invokes }()
	default:
		r.respondError(request.ID, safeFault("resource_exhausted", "HTTP invocation limit reached", nil))
		return
	}
	output, fault := r.bridge.invoke(ctx, params.Provider, params.Capability.UpstreamName, params.Input)
	if fault != nil {
		r.respondError(request.ID, fault)
		return
	}
	r.respondResult(request.ID, map[string]any{"output": json.RawMessage(output)})
}

func (r *adapterRuntime) reserveID(id string) bool {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	if _, exists := r.active[id]; exists {
		return false
	}
	r.active[id] = struct{}{}
	return true
}

func (r *adapterRuntime) releaseID(id string) {
	r.activeMu.Lock()
	delete(r.active, id)
	r.activeMu.Unlock()
}

func (r *adapterRuntime) respondResult(id string, result any) {
	encoded, err := json.Marshal(result)
	if err != nil {
		r.respondError(id, safeFault("internal_error", "adapter could not encode a response", err))
		return
	}
	r.write(protocolResponse{Version: adapterProtocolVersion, ID: id, Result: encoded})
}

func (r *adapterRuntime) respondError(id string, fault *adapterFault) {
	r.write(protocolResponse{Version: adapterProtocolVersion, ID: id, Error: fault})
}

func (r *adapterRuntime) write(response protocolResponse) {
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded)+1 > maxProtocolLineBytes {
		response = protocolResponse{
			Version: adapterProtocolVersion,
			ID:      response.ID,
			Error:   safeFault("response_too_large", "adapter response exceeds limit", err),
		}
		encoded, err = json.Marshal(response)
		if err != nil || len(encoded)+1 > maxProtocolLineBytes {
			r.diagnostic("adapter response could not be encoded within the protocol limit")
			return
		}
	}
	encoded = append(encoded, '\n')
	r.writeMu.Lock()
	_, err = r.output.Write(encoded)
	r.writeMu.Unlock()
	if err != nil {
		r.diagnostic("adapter response could not be written")
	}
}

func (r *adapterRuntime) diagnostic(message string) {
	_, _ = fmt.Fprintln(r.stderr, message)
}
