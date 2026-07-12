package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	adapterreg "github.com/gopact-ai/9a/internal/adapter"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/processgroup"
	"github.com/gopact-ai/9a/internal/provider"
)

type Adapter struct {
	mu                sync.Mutex
	sessions          map[*session]activeSession
	invocations       map[string]*session
	activeSessions    int
	maxActiveSessions int
}

type activeSession struct {
	providerID   string
	invocationID string
}

const (
	maxDiscoveryBytes    = 8 << 20
	maxDiscoveryTools    = 10_000
	maxResponseLineBytes = 2 << 20
	maxActiveMCPSessions = 64
	processStopTimeout   = 5 * time.Second
)

var errUnterminatedMCPResponse = errors.New("MCP response is missing a newline terminator")

type discoveryBudget struct {
	bytes, tools int
	seen         map[string]struct{}
}

func (b *discoveryBudget) add(responseBytes, pageTools int, nextCursor string) error {
	b.bytes += responseBytes
	b.tools += pageTools
	if b.bytes > maxDiscoveryBytes {
		return errors.New("mcp discovery exceeds aggregate byte limit")
	}
	if b.tools > maxDiscoveryTools {
		return errors.New("mcp discovery exceeds tool limit")
	}
	if nextCursor != "" {
		if b.seen == nil {
			b.seen = map[string]struct{}{}
		}
		if _, exists := b.seen[nextCursor]; exists {
			return errors.New("mcp tools pagination cursor repeated")
		}
		b.seen[nextCursor] = struct{}{}
	}
	return nil
}

func New() *Adapter {
	return &Adapter{
		sessions:          make(map[*session]activeSession),
		invocations:       make(map[string]*session),
		maxActiveSessions: maxActiveMCPSessions,
	}
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
type session struct {
	cmd      *exec.Cmd
	in       io.WriteCloser
	scan     *bufio.Scanner
	next     int
	stopOnce sync.Once
	done     chan struct{}
	waitMu   sync.Mutex
	waitErr  error
	canceled atomic.Bool
}

func startSession(ctx context.Context, p provider.Provider) (*session, error) {
	if !strings.HasPrefix(p.Endpoint, "stdio:") {
		return nil, errors.New("mcp endpoint must use stdio:")
	}
	executable, err := adapterreg.ValidateExecutable(strings.TrimPrefix(p.Endpoint, "stdio:"))
	if err != nil {
		return nil, fmt.Errorf("invalid MCP executable: %w", err)
	}
	cmd := exec.CommandContext(ctx, executable)
	processgroup.Configure(cmd)
	cmd.Cancel = func() error {
		if err := processgroup.Kill(cmd); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		return nil
	}
	cmd.WaitDelay = processStopTimeout
	cmd.Env = safeEnvironment(os.Environ())
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	s := &session{cmd: cmd, in: in, scan: bufio.NewScanner(out), done: make(chan struct{})}
	s.scan.Buffer(make([]byte, 64<<10), maxResponseLineBytes)
	s.scan.Split(splitTerminatedMCPResponseLine)
	go func() {
		err := cmd.Wait()
		s.waitMu.Lock()
		s.waitErr = err
		s.waitMu.Unlock()
		close(s.done)
	}()
	return s, nil
}

func splitTerminatedMCPResponseLine(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
		end := newline
		if end > 0 && data[end-1] == '\r' {
			end--
		}
		return newline + 1, data[:end], nil
	}
	if atEOF && len(data) != 0 {
		return 0, nil, errUnterminatedMCPResponse
	}
	return 0, nil, nil
}

func (s *session) failure() error {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	if s.waitErr == nil {
		return io.ErrUnexpectedEOF
	}
	return s.waitErr
}

func (s *session) stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
		_ = s.in.Close()
		_ = processgroup.Kill(s.cmd)
	})
	waitCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, processStopTimeout)
		defer cancel()
	}
	select {
	case <-s.done:
		return nil
	case <-waitCtx.Done():
		_ = processgroup.Kill(s.cmd)
		return waitCtx.Err()
	}
}

func (a *Adapter) acquireSession() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeSessions >= a.maxActiveSessions {
		adapterErr, _ := provider.NewAdapterError("resource_exhausted", "MCP concurrent session limit reached")
		return adapterErr
	}
	a.activeSessions++
	return nil
}

func (a *Adapter) releaseReservation() {
	a.mu.Lock()
	a.activeSessions--
	a.mu.Unlock()
}

func (a *Adapter) open(ctx context.Context, p provider.Provider, invocationID string) (*session, error) {
	if err := a.acquireSession(); err != nil {
		return nil, err
	}
	s, err := startSession(ctx, p)
	if err != nil {
		a.releaseReservation()
		return nil, err
	}
	a.mu.Lock()
	if invocationID != "" {
		if _, duplicate := a.invocations[invocationID]; duplicate {
			a.mu.Unlock()
			_ = s.stop(context.Background())
			a.releaseReservation()
			return nil, errors.New("duplicate MCP invocation ID")
		}
		a.invocations[invocationID] = s
	}
	a.sessions[s] = activeSession{providerID: p.ID, invocationID: invocationID}
	a.mu.Unlock()
	initRaw, initErr := s.call("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "ninea", "version": "v1"}})
	if initErr != nil {
		a.release(s)
		_ = s.stop(context.Background())
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, initErr
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(initRaw, &initialized) != nil || initialized.ProtocolVersion != "2025-11-25" {
		a.release(s)
		_ = s.stop(context.Background())
		return nil, errors.New("mcp initialize returned invalid protocol version")
	}
	_ = json.NewEncoder(s.in).Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return s, nil
}

func (a *Adapter) release(s *session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	active, exists := a.sessions[s]
	if !exists {
		return
	}
	delete(a.sessions, s)
	if active.invocationID != "" && a.invocations[active.invocationID] == s {
		delete(a.invocations, active.invocationID)
	}
	a.activeSessions--
}
func (s *session) call(method string, params any) (json.RawMessage, error) {
	return s.callStarted(method, params, nil)
}
func (s *session) callStarted(method string, params any, started func() error) (json.RawMessage, error) {
	s.next++
	if err := json.NewEncoder(s.in).Encode(map[string]any{"jsonrpc": "2.0", "id": s.next, "method": method, "params": params}); err != nil {
		return nil, err
	}
	if started != nil {
		if err := started(); err != nil {
			return nil, err
		}
	}
	for s.scan.Scan() {
		var r response
		if json.Unmarshal(s.scan.Bytes(), &r) != nil || r.ID != s.next {
			continue
		}
		if r.JSONRPC != "2.0" || (r.Error == nil) == (r.Result == nil) {
			return nil, errors.New("invalid MCP JSON-RPC response envelope")
		}
		if r.Error != nil {
			return nil, fmt.Errorf("mcp %d: %s", r.Error.Code, r.Error.Message)
		}
		return r.Result, nil
	}
	if err := s.scan.Err(); err != nil {
		return nil, fmt.Errorf("read MCP response: %w", err)
	}
	select {
	case <-s.done:
		return nil, s.failure()
	default:
		return nil, io.ErrUnexpectedEOF
	}
}
func (a *Adapter) Discover(ctx context.Context, p provider.Provider) ([]capability.Capability, error) {
	s, e := a.open(ctx, p, "")
	if e != nil {
		return nil, e
	}
	defer func() {
		a.release(s)
		_ = s.stop(context.Background())
	}()
	var v struct {
		Tools *[]struct {
			Name, Description string
			InputSchema       map[string]any `json:"inputSchema"`
		} `json:"tools"`
		NextCursor string `json:"nextCursor"`
	}
	var tools []struct {
		Name, Description string
		InputSchema       map[string]any `json:"inputSchema"`
	}
	cursor := ""
	var budget discoveryBudget
	for page := 0; page < 1000; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, e := s.call("tools/list", params)
		if e != nil {
			return nil, e
		}
		v.Tools = nil
		v.NextCursor = ""
		if e = json.Unmarshal(raw, &v); e != nil {
			return nil, e
		}
		if v.Tools == nil {
			return nil, errors.New("mcp tools/list result is missing tools")
		}
		if err := budget.add(len(raw), len(*v.Tools), v.NextCursor); err != nil {
			return nil, err
		}
		tools = append(tools, (*v.Tools)...)
		if v.NextCursor == "" {
			break
		}
		cursor = v.NextCursor
		if page == 999 {
			return nil, errors.New("mcp tools pagination exceeded 1000 pages")
		}
	}
	out := make([]capability.Capability, 0, len(tools))
	for _, t := range tools {
		meta, _ := json.Marshal(t)
		out = append(out, capability.Capability{ID: capability.StableID("mcp", p.Name, t.Name), Kind: "mcp.tool", Name: capability.Slug(t.Name), Description: t.Description, Source: capability.Source{Protocol: "mcp", Provider: p.Name, UpstreamName: t.Name}, Input: capability.Contract{Mode: "json", JSONSchema: t.InputSchema}, Output: capability.Contract{Mode: "mcp.toolResult"}, Lifecycle: capability.Lifecycle{Sync: true, Cancelable: true}, Security: capability.Security{UpstreamAuth: "provider-configured"}, RawMetadata: meta})
	}
	return out, nil
}
func (a *Adapter) Invoke(ctx context.Context, p provider.Provider, c capability.Capability, invocationID string, input json.RawMessage, sink provider.Sink) error {
	s, e := a.open(ctx, p, invocationID)
	if e != nil {
		return e
	}
	defer func() {
		a.release(s)
		_ = s.stop(context.Background())
	}()
	var args any
	if e = json.Unmarshal(input, &args); e != nil {
		return e
	}
	raw, e := s.callStarted("tools/call", map[string]any{"name": c.Source.UpstreamName, "arguments": args}, sink.Started)
	if s.canceled.Load() {
		adapterErr, _ := provider.NewAdapterError("canceled", "MCP invocation was canceled")
		return adapterErr
	}
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return e
	}
	return sink.Event(provider.Event{Type: "result", Data: raw})
}
func (a *Adapter) Cancel(ctx context.Context, p provider.Provider, invocationID string) error {
	a.mu.Lock()
	s := a.invocations[invocationID]
	active, exists := a.sessions[s]
	if !exists || active.providerID != p.ID || !s.canceled.CompareAndSwap(false, true) {
		a.mu.Unlock()
		adapterErr, _ := provider.NewAdapterError("not_cancelable", "MCP invocation is not active")
		return adapterErr
	}
	a.mu.Unlock()
	err := s.stop(ctx)
	a.release(s)
	return err
}
func (a *Adapter) Health(context.Context, provider.Provider) provider.Health {
	return provider.Health{Healthy: true}
}
func (a *Adapter) Close(ctx context.Context, p provider.Provider) error {
	a.mu.Lock()
	var sessions []*session
	for s, active := range a.sessions {
		if active.providerID == p.ID {
			sessions = append(sessions, s)
		}
	}
	a.mu.Unlock()
	var errs []error
	for _, s := range sessions {
		if err := s.stop(ctx); err != nil {
			errs = append(errs, err)
		}
		a.release(s)
	}
	return errors.Join(errs...)
}

func safeEnvironment(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		key, _, _ := strings.Cut(value, "=")
		if key == "NINEA_TOKEN" || key == "NINEA_BOOTSTRAP_TOKEN" {
			continue
		}
		out = append(out, value)
	}
	return out
}
