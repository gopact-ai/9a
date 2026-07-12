package executable

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
)

const (
	protocolVersion       = "9a.adapter/v1"
	maxLineBytes          = 8 << 20
	maxCapabilities       = 10_000
	maxDescriptionRunes   = 512
	maxSchemaBytes        = 1 << 20
	maxCapabilityMetadata = 1 << 20
	maxLifecycleStates    = 64
	cancelTerminalTimeout = 5 * time.Second
	maxPendingRequests    = 10_000
	maxAbandonedRequests  = 1_024
)

var errUnterminatedResponseLine = errors.New("adapter response is missing a newline terminator")

type Adapter struct {
	protocol   string
	executable string
	mu         sync.Mutex
	sessions   map[string]*session
}

func New(protocol, executable string) (*Adapter, error) {
	if capability.Slug(protocol) != protocol || protocol == "" {
		return nil, errors.New("adapter protocol must be a non-empty slug")
	}
	if !filepath.IsAbs(executable) {
		return nil, errors.New("adapter executable path must be absolute")
	}
	return &Adapter{protocol: protocol, executable: executable, sessions: map[string]*session{}}, nil
}

type providerParams struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
}

type request struct {
	Version string `json:"version"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type response struct {
	Version  string            `json:"version"`
	ID       string            `json:"id"`
	Result   json.RawMessage   `json:"result"`
	Error    *responseError    `json:"error"`
	Event    *eventEnvelope    `json:"event"`
	Artifact *artifactEnvelope `json:"artifact"`
}

type eventEnvelope struct {
	Sequence int             `json:"sequence"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
}

type artifactEnvelope struct {
	Sequence  int    `json:"sequence"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Encoding  string `json:"encoding"`
	Data      string `json:"data"`
}

type pendingResult struct {
	response response
}

type pendingRequest struct {
	ch              chan pendingResult
	invoke          bool
	lastSeq         int
	canceling       bool
	cancelConfirmed bool
	heldTerminal    *response
	terminalCode    string
	done            chan struct{}
	abandoned       bool
	abandonedCh     chan struct{}
}

type session struct {
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        io.ReadCloser
	scanner       *bufio.Scanner
	writeGate     chan struct{}
	mu            sync.Mutex
	pending       map[string]*pendingRequest
	abandoned     int
	dead          bool
	failErr       error
	stopped       chan struct{}
	done          chan struct{}
	terminateOnce sync.Once
}

func startSession(executable string) (*session, error) {
	cmd := exec.Command(executable)
	configureProcessGroup(cmd)
	cmd.Env = safeEnvironment(os.Environ())
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	s := &session{cmd: cmd, stdin: stdin, stdout: stdout, scanner: bufio.NewScanner(stdout), writeGate: make(chan struct{}, 1), pending: map[string]*pendingRequest{}, stopped: make(chan struct{}), done: make(chan struct{})}
	s.writeGate <- struct{}{}
	s.scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	s.scanner.Split(splitTerminatedResponseLine)
	go s.readLoop()
	return s, nil
}

func splitTerminatedResponseLine(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
		end := newline
		if end > 0 && data[end-1] == '\r' {
			end--
		}
		return newline + 1, data[:end], nil
	}
	if atEOF && len(data) != 0 {
		return 0, nil, errUnterminatedResponseLine
	}
	return 0, nil, nil
}

func (s *session) readLoop() {
	var terminalErr error

readMessages:
	for s.scanner.Scan() {
		if !utf8.Valid(s.scanner.Bytes()) {
			terminalErr = errors.New("adapter message is not valid UTF-8")
			break
		}
		var message response
		if err := json.Unmarshal(s.scanner.Bytes(), &message); err != nil {
			terminalErr = fmt.Errorf("malformed adapter JSON: %w", err)
			break
		}
		if message.Version != protocolVersion {
			terminalErr = errors.New("adapter response has wrong version")
			break
		}
		parts := 0
		if message.Result != nil {
			parts++
		}
		if message.Error != nil {
			parts++
		}
		if message.Event != nil {
			parts++
		}
		if message.Artifact != nil {
			parts++
		}
		if message.ID == "" || parts != 1 {
			terminalErr = errors.New("invalid adapter response envelope")
			break
		}
		if message.Error != nil {
			if _, err := provider.NewAdapterError(message.Error.Code, message.Error.Message); err != nil {
				terminalErr = errors.New("invalid adapter error envelope")
				break
			}
		}
		s.mu.Lock()
		pending := s.pending[message.ID]
		if pending != nil && (message.Event != nil || message.Artifact != nil) {
			if !pending.invoke {
				s.mu.Unlock()
				terminalErr = errors.New("non-invoke request emitted stream item")
				break
			}
			sequence := 0
			if message.Event != nil {
				sequence = message.Event.Sequence
			}
			if message.Artifact != nil {
				sequence = message.Artifact.Sequence
			}
			if sequence <= pending.lastSeq {
				s.mu.Unlock()
				terminalErr = errors.New("adapter event sequence did not increase")
				break
			}
			pending.lastSeq = sequence
			if pending.abandoned {
				s.mu.Unlock()
				continue
			}
		} else if pending != nil {
			if pending.invoke && pending.canceling {
				if !pending.cancelConfirmed {
					if pending.heldTerminal != nil {
						s.mu.Unlock()
						terminalErr = errors.New("adapter emitted multiple invocation terminals while cancellation was pending")
						break
					}
					copy := message
					pending.heldTerminal = &copy
					s.mu.Unlock()
					continue
				}
				if message.Error == nil || message.Error.Code != "canceled" {
					s.mu.Unlock()
					terminalErr = errors.New("adapter confirmed cancellation but invocation did not terminate as canceled")
					break
				}
			}
			if message.Error != nil {
				pending.terminalCode = message.Error.Code
			}
			abandoned := pending.abandoned
			s.finishPendingLocked(message.ID, pending)
			if abandoned {
				s.mu.Unlock()
				continue
			}
		}
		s.mu.Unlock()
		if pending == nil {
			terminalErr = errors.New("adapter response has unknown request id")
			break
		}
		select {
		case pending.ch <- pendingResult{response: message}:
		case <-pending.abandonedCh:
			continue
		case <-s.stopped:
			terminalErr = s.failure()
			break readMessages
		}
	}
	if terminalErr == nil {
		if err := s.scanner.Err(); err != nil {
			terminalErr = fmt.Errorf("adapter protocol read failed: %w", err)
		} else {
			terminalErr = errors.New("adapter process exited")
		}
	}
	s.fail(terminalErr)
	s.terminate()
	_ = s.cmd.Wait()
	close(s.done)
}

func (s *session) fail(err error) {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	s.dead = true
	s.failErr = err
	s.pending = map[string]*pendingRequest{}
	s.abandoned = 0
	close(s.stopped)
	s.mu.Unlock()
}

func (s *session) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failErr == nil {
		return errors.New("adapter session closed")
	}
	return s.failErr
}

func (s *session) abandon(id string) {
	s.mu.Lock()
	pending := s.pending[id]
	if pending == nil || pending.abandoned {
		s.mu.Unlock()
		return
	}
	pending.abandoned = true
	s.abandoned++
	close(pending.abandonedCh)
	overLimit := s.abandoned > maxAbandonedRequests
	s.mu.Unlock()
	if overLimit {
		s.abort(errors.New("too many abandoned adapter requests"))
	}
}

func (s *session) finishPendingLocked(id string, pending *pendingRequest) bool {
	if s.pending[id] != pending {
		return false
	}
	delete(s.pending, id)
	if pending.abandoned {
		s.abandoned--
	}
	close(pending.done)
	return true
}

func (s *session) removeUnsent(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

func (s *session) isDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

func encodeRequestLine(message request) ([]byte, error) {
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode adapter request: %w", err)
	}
	if len(encoded)+1 > maxLineBytes {
		return nil, fmt.Errorf("adapter request exceeds %d byte limit", maxLineBytes)
	}
	return append(encoded, '\n'), nil
}

func (s *session) begin(ctx context.Context, id, method string, params any, invoke bool) (<-chan pendingResult, error) {
	encoded, err := encodeRequestLine(request{Version: protocolVersion, ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	ch := make(chan pendingResult, 1)
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return nil, errors.New("adapter session is closed")
	}
	if _, exists := s.pending[id]; exists {
		s.mu.Unlock()
		return nil, errors.New("duplicate adapter request id")
	}
	if len(s.pending) >= maxPendingRequests {
		s.mu.Unlock()
		return nil, errors.New("too many pending adapter requests")
	}
	s.pending[id] = &pendingRequest{ch: ch, invoke: invoke, done: make(chan struct{}), abandonedCh: make(chan struct{})}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		s.removeUnsent(id)
		return nil, ctx.Err()
	case <-s.stopped:
		return nil, s.failure()
	case <-s.writeGate:
	}
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := io.Copy(s.stdin, bytes.NewReader(encoded))
		writeDone <- writeErr
	}()
	select {
	case err = <-writeDone:
	case <-ctx.Done():
		s.fail(ctx.Err())
		s.terminate()
		<-writeDone
		err = ctx.Err()
	case <-s.stopped:
		s.terminate()
		<-writeDone
		err = s.failure()
	}
	s.writeGate <- struct{}{}
	if err != nil {
		s.fail(fmt.Errorf("write adapter request: %w", err))
		s.terminate()
		return nil, err
	}
	return ch, nil
}

func (s *session) request(ctx context.Context, id, method string, params any) (json.RawMessage, error) {
	ch, err := s.begin(ctx, id, method, params, false)
	if err != nil {
		return nil, err
	}
	select {
	case item := <-ch:
		if item.response.Error != nil {
			adapterErr, _ := provider.NewAdapterError(item.response.Error.Code, item.response.Error.Message)
			return nil, adapterErr
		}
		return item.response.Result, nil
	case <-s.stopped:
		return nil, s.failure()
	case <-ctx.Done():
		s.abandon(id)
		return nil, ctx.Err()
	}
}

func (s *session) terminate() {
	s.terminateOnce.Do(func() {
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		if s.stdout != nil {
			_ = s.stdout.Close()
		}
		if s.cmd != nil {
			killProcessGroup(s.cmd)
		}
	})
}

func (s *session) close(ctx context.Context) error {
	s.fail(errors.New("adapter session closed"))
	s.terminate()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *session) abort(err error) error {
	s.fail(err)
	s.terminate()
	return err
}

func providerKey(p provider.Provider) string {
	if p.ID != "" {
		return p.ID
	}
	return p.Protocol + "/" + p.Name
}

func (a *Adapter) session(p provider.Provider) (*session, error) {
	if p.Protocol != a.protocol {
		return nil, fmt.Errorf("provider protocol %q does not match adapter %q", p.Protocol, a.protocol)
	}
	key := providerKey(p)
	a.mu.Lock()
	defer a.mu.Unlock()
	if current := a.sessions[key]; current != nil && !current.isDead() {
		return current, nil
	}
	s, err := startSession(a.executable)
	if err != nil {
		return nil, err
	}
	a.sessions[key] = s
	return s, nil
}

func (a *Adapter) existingSession(p provider.Provider) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[providerKey(p)]
	if s == nil || s.isDead() {
		return nil
	}
	return s
}

func (a *Adapter) roundTrip(ctx context.Context, p provider.Provider, method string, params any) (json.RawMessage, error) {
	s, err := a.session(p)
	if err != nil {
		return nil, err
	}
	id, err := call.NewID()
	if err != nil {
		return nil, err
	}
	return s.request(ctx, id, method, params)
}

type contractDTO struct {
	Mode       string         `json:"mode"`
	JSONSchema map[string]any `json:"json_schema"`
	MediaTypes []string       `json:"media_types"`
}

type lifecycleDTO struct {
	Sync       bool     `json:"sync"`
	Streaming  bool     `json:"streaming"`
	MultiTurn  bool     `json:"multi_turn"`
	Cancelable bool     `json:"cancelable"`
	States     []string `json:"states"`
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
	Tags         []string     `json:"tags"`
	Examples     []string     `json:"examples"`
}

func (a *Adapter) Discover(ctx context.Context, p provider.Provider) ([]capability.Capability, error) {
	raw, err := a.roundTrip(ctx, p, "discover", map[string]any{"provider": providerParams{Name: p.Name, Endpoint: p.Endpoint}})
	if err != nil {
		return nil, err
	}
	var result struct {
		Capabilities *[]capabilityDTO `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode discover result: %w", err)
	}
	if result.Capabilities == nil {
		return nil, errors.New("discover result is missing capabilities")
	}
	if len(*result.Capabilities) > maxCapabilities {
		return nil, fmt.Errorf("discover returned more than %d capabilities", maxCapabilities)
	}
	out := make([]capability.Capability, 0, len(*result.Capabilities))
	for _, item := range *result.Capabilities {
		if err := validateCapabilityDTO(item); err != nil {
			return nil, err
		}
		c := capability.Capability{
			ID: capability.StableID(a.protocol, p.Name, item.UpstreamName), Kind: item.Kind, Name: item.Name, Description: item.Description,
			Source:    capability.Source{Protocol: a.protocol, Provider: p.Name, UpstreamName: item.UpstreamName},
			Input:     capability.Contract{Mode: item.Input.Mode, JSONSchema: item.Input.JSONSchema, MediaTypes: item.Input.MediaTypes},
			Output:    capability.Contract{Mode: item.Output.Mode, JSONSchema: item.Output.JSONSchema, MediaTypes: item.Output.MediaTypes},
			Lifecycle: capability.Lifecycle{Sync: item.Lifecycle.Sync, Streaming: item.Lifecycle.Streaming, MultiTurn: item.Lifecycle.MultiTurn, Cancelable: item.Lifecycle.Cancelable, States: item.Lifecycle.States},
			Security:  capability.Security{RequiresApproval: item.Security.RequiresApproval, UpstreamAuth: item.Security.UpstreamAuth}, Tags: item.Tags, Examples: item.Examples,
		}
		metadata, _ := json.Marshal(item)
		c.RawMetadata = metadata
		if err := c.Validate(); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func validateCapabilityDTO(item capabilityDTO) error {
	if item.Lifecycle.MultiTurn {
		return errors.New("adapter v1 capability cannot declare multi_turn")
	}
	if len([]rune(item.Description)) > maxDescriptionRunes {
		return errors.New("capability description exceeds limit")
	}
	for _, contract := range []contractDTO{item.Input, item.Output} {
		raw, err := json.Marshal(contract.JSONSchema)
		if err != nil {
			return fmt.Errorf("marshal capability schema: %w", err)
		}
		if len(raw) > maxSchemaBytes {
			return errors.New("capability schema exceeds limit")
		}
	}
	if len(item.Lifecycle.States) > maxLifecycleStates {
		return errors.New("capability lifecycle states exceed limit")
	}
	for _, state := range item.Lifecycle.States {
		if state == "" || len(state) > 128 {
			return errors.New("invalid capability lifecycle state")
		}
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("marshal capability metadata: %w", err)
	}
	if len(raw) > maxCapabilityMetadata {
		return errors.New("capability metadata exceeds limit")
	}
	return nil
}

func (a *Adapter) Health(ctx context.Context, p provider.Provider) provider.Health {
	raw, err := a.roundTrip(ctx, p, "health", map[string]any{"provider": providerParams{Name: p.Name, Endpoint: p.Endpoint}})
	if err != nil {
		return provider.Health{Message: err.Error()}
	}
	var result *struct {
		Healthy *bool  `json:"healthy"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return provider.Health{Message: err.Error()}
	}
	if result == nil || result.Healthy == nil {
		return provider.Health{Message: "invalid health result: missing healthy"}
	}
	return provider.Health{Healthy: *result.Healthy, Message: result.Message}
}

func (a *Adapter) Invoke(ctx context.Context, p provider.Provider, c capability.Capability, invocationID string, input json.RawMessage, sink provider.Sink) error {
	if !json.Valid(input) {
		return errors.New("invoke input is not valid JSON")
	}
	s, err := a.session(p)
	if err != nil {
		return err
	}
	var decodedInput any
	if err := json.Unmarshal(input, &decodedInput); err != nil {
		return err
	}
	ch, err := s.begin(ctx, invocationID, "invoke", map[string]any{
		"provider":   providerParams{Name: p.Name, Endpoint: p.Endpoint},
		"capability": map[string]string{"upstream_name": c.Source.UpstreamName},
		"input":      decodedInput,
	}, true)
	if err != nil {
		return err
	}
	if err := sink.Started(); err != nil {
		s.abandon(invocationID)
		return err
	}
	for {
		select {
		case item := <-ch:
			message := item.response
			switch {
			case message.Event != nil:
				if message.Event.Type == "" || message.Event.Data == nil {
					return s.abort(errors.New("invalid adapter event"))
				}
				if err := sink.Event(provider.Event{Type: message.Event.Type, Data: append(json.RawMessage(nil), message.Event.Data...)}); err != nil {
					s.abandon(invocationID)
					return err
				}
			case message.Artifact != nil:
				artifact := message.Artifact
				if artifact.Encoding != "base64" {
					return s.abort(errors.New("unsupported artifact encoding"))
				}
				if artifact.Name == "" || artifact.MediaType == "" {
					return s.abort(errors.New("invalid adapter artifact"))
				}
				data, err := base64.StdEncoding.DecodeString(artifact.Data)
				if err != nil {
					return s.abort(fmt.Errorf("decode adapter artifact: %w", err))
				}
				if err := sink.Artifact(artifact.Name, artifact.MediaType, data); err != nil {
					s.abandon(invocationID)
					return err
				}
			case message.Error != nil:
				adapterErr, _ := provider.NewAdapterError(message.Error.Code, message.Error.Message)
				return adapterErr
			case message.Result != nil:
				var result struct {
					Output json.RawMessage `json:"output"`
				}
				if err := json.Unmarshal(message.Result, &result); err != nil || result.Output == nil {
					return s.abort(errors.New("invoke result is missing output"))
				}
				return sink.Event(provider.Event{Type: "result", Data: append(json.RawMessage(nil), result.Output...)})
			}
		case <-s.stopped:
			return s.failure()
		case <-ctx.Done():
			s.abandon(invocationID)
			return ctx.Err()
		}
	}
}

func (s *session) resolveCancellation(ctx context.Context, invocationID string, invocation *pendingRequest, confirmed bool) error {
	s.mu.Lock()
	if s.pending[invocationID] != invocation {
		s.mu.Unlock()
		select {
		case <-s.stopped:
			return s.failure()
		default:
			return errors.New("not_cancelable: invocation is no longer active")
		}
	}
	invocation.cancelConfirmed = confirmed
	if !confirmed {
		invocation.canceling = false
	}
	held := invocation.heldTerminal
	invocation.heldTerminal = nil
	if held != nil && confirmed && (held.Error == nil || held.Error.Code != "canceled") {
		s.mu.Unlock()
		return s.abort(errors.New("adapter confirmed cancellation but invocation did not terminate as canceled"))
	}
	if held != nil {
		if held.Error != nil {
			invocation.terminalCode = held.Error.Code
		}
		s.finishPendingLocked(invocationID, invocation)
	}
	abandoned := invocation.abandoned
	s.mu.Unlock()
	if held == nil || abandoned {
		return nil
	}
	select {
	case invocation.ch <- pendingResult{response: *held}:
		return nil
	case <-s.stopped:
		return s.failure()
	case <-ctx.Done():
		return s.abort(ctx.Err())
	}
}

func (a *Adapter) Cancel(ctx context.Context, p provider.Provider, invocationID string) error {
	s := a.existingSession(p)
	if s == nil {
		return errors.New("not_cancelable: invocation is not active")
	}
	s.mu.Lock()
	invocation := s.pending[invocationID]
	if invocation == nil || !invocation.invoke || invocation.canceling || invocation.abandoned {
		s.mu.Unlock()
		return errors.New("not_cancelable: invocation is not active")
	}
	invocation.canceling = true
	s.mu.Unlock()
	cancelID, err := call.NewID()
	if err != nil {
		_ = s.resolveCancellation(ctx, invocationID, invocation, false)
		return err
	}
	raw, err := s.request(ctx, cancelID, "cancel", map[string]any{"invocation_id": invocationID})
	if err != nil {
		if releaseErr := s.resolveCancellation(ctx, invocationID, invocation, false); releaseErr != nil {
			return releaseErr
		}
		return err
	}
	var result struct {
		Canceled bool `json:"canceled"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || !result.Canceled {
		if releaseErr := s.resolveCancellation(ctx, invocationID, invocation, false); releaseErr != nil {
			return releaseErr
		}
		return errors.New("adapter cancel was not confirmed")
	}
	if err := s.resolveCancellation(ctx, invocationID, invocation, true); err != nil {
		return err
	}
	timer := time.NewTimer(cancelTerminalTimeout)
	defer timer.Stop()
	select {
	case <-invocation.done:
		if invocation.terminalCode != "canceled" {
			return s.abort(errors.New("adapter confirmed cancellation but invocation did not terminate as canceled"))
		}
		return nil
	case <-s.stopped:
		return s.failure()
	case <-ctx.Done():
		return s.abort(ctx.Err())
	case <-timer.C:
		return s.abort(errors.New("adapter cancellation terminal timeout"))
	}
}

func (a *Adapter) Close(ctx context.Context, p provider.Provider) error {
	key := providerKey(p)
	a.mu.Lock()
	s := a.sessions[key]
	delete(a.sessions, key)
	a.mu.Unlock()
	if s != nil {
		return s.close(ctx)
	}
	return nil
}

func safeEnvironment(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		key := value
		if index := len(key); index > 0 {
			for i := range key {
				if key[i] == '=' {
					key = key[:i]
					break
				}
			}
		}
		if key != "NINEA_TOKEN" && key != "NINEA_BOOTSTRAP_TOKEN" {
			out = append(out, value)
		}
	}
	return out
}
