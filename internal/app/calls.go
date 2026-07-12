package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"

	"github.com/gopact-ai/9a/internal/authz"
	"github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
)

var (
	ErrCallNotFound      = errors.New("call not found")
	ErrCallNotCancelable = errors.New("call is not cancelable")
	ErrCallNotActive     = errors.New("call is not active")
	ErrCallPersistence   = errors.New("call terminal state persistence failed")
	ErrCallCancelFailed  = errors.New("adapter cancellation failed")
)

const maxRuntimeCallErrors = 1_024

type callRuntime struct {
	id          string
	owner       string
	capability  capability.Capability
	provider    provider.Provider
	adapter     provider.Adapter
	lease       *operationLease
	ready       chan struct{}
	readyOnce   sync.Once
	done        chan struct{}
	terminalErr error
}

type persistentCallSink struct {
	repo    *call.Repository
	callID  string
	result  json.RawMessage
	runtime *callRuntime
}

func (s *persistentCallSink) Started() error {
	s.runtime.readyOnce.Do(func() { close(s.runtime.ready) })
	return nil
}

func (s *persistentCallSink) Event(event provider.Event) error {
	if len(event.Data) > call.MaxPayloadBytes || !json.Valid(event.Data) {
		return call.ErrPayloadTooLarge
	}
	envelope, err := json.Marshal(map[string]any{"kind": "event", "type": event.Type, "data": event.Data})
	if err != nil {
		return err
	}
	if _, err := s.repo.AppendEvent(context.Background(), s.callID, envelope); err != nil {
		return err
	}
	if event.Type == "result" {
		s.result = append(json.RawMessage(nil), event.Data...)
	}
	return nil
}

func (s *persistentCallSink) Artifact(name, mediaType string, data []byte) error {
	if name == "" || mediaType == "" || len(data) > call.MaxPayloadBytes {
		return call.ErrPayloadTooLarge
	}
	envelope, err := json.Marshal(map[string]any{"kind": "artifact", "name": name, "media_type": mediaType, "encoding": "base64", "data": base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		return err
	}
	_, err = s.repo.AppendEvent(context.Background(), s.callID, envelope)
	return err
}

func (a *App) StartCall(ctx context.Context, identity, capabilityID string, input json.RawMessage) (string, error) {
	if len(input) > call.MaxPayloadBytes {
		return "", call.ErrPayloadTooLarge
	}
	if !json.Valid(input) {
		return "", errors.New("call input is not valid JSON")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	lease, err := a.beginOperation(context.Background())
	if err != nil {
		return "", err
	}
	a.mutation.RLock()
	launched := false
	defer func() {
		if !launched {
			a.mutation.RUnlock()
			lease.done()
		}
	}()
	if err := lease.check(); err != nil {
		return "", err
	}
	if !a.az.Allowed(ctx, identity, capabilityID, authz.Invoke) {
		return "", errors.New("permission_denied")
	}
	c, err := a.cat.GetCapability(ctx, capabilityID)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	p, ok := a.providers[c.Source.Protocol+"/"+c.Source.Provider]
	ad := a.adapters[p.Protocol]
	state := a.state
	a.mu.Unlock()
	if state != appOpen {
		return "", ErrAppClosed
	}
	if !ok || ad == nil {
		return "", errors.New("provider_unavailable")
	}
	if err := lease.setTarget(ad, p); err != nil {
		return "", err
	}
	id, err := call.NewID()
	if err != nil {
		return "", err
	}
	if err := a.callDB.Create(ctx, call.Call{ID: id, CapabilityID: capabilityID, IdentityID: identity, State: call.Submitted}, input); err != nil {
		return "", err
	}
	runtime := &callRuntime{id: id, owner: identity, capability: c, provider: p, adapter: ad, lease: lease, ready: make(chan struct{}), done: make(chan struct{})}
	a.mu.Lock()
	if a.state != appOpen {
		a.mu.Unlock()
		if transitionErr := a.callDB.Transition(context.Background(), id, call.Failed, "app_closed", "application closed before call started"); transitionErr != nil {
			return "", errors.Join(ErrAppClosed, transitionErr)
		}
		return "", ErrAppClosed
	}
	a.activeCalls[id] = runtime
	a.mu.Unlock()
	launched = true
	go a.runCall(runtime, input)
	return id, nil
}

func (a *App) runCall(runtime *callRuntime, input json.RawMessage) {
	defer func() {
		a.mu.Lock()
		delete(a.activeCalls, runtime.id)
		close(runtime.done)
		a.mu.Unlock()
		a.mutation.RUnlock()
		runtime.lease.done()
	}()
	if err := a.callDB.Transition(context.Background(), runtime.id, call.Working, "", ""); err != nil {
		a.fallbackTerminal(runtime, err)
		return
	}
	sink := &persistentCallSink{repo: a.callDB, callID: runtime.id, runtime: runtime}
	err := runtime.adapter.Invoke(runtime.lease.ctx, runtime.provider, runtime.capability, runtime.id, input, sink)
	if err != nil {
		var adapterErr *provider.AdapterError
		isAdapterErr := errors.As(err, &adapterErr)
		validAdapterErr := isAdapterErr && adapterErr != nil && adapterErr.Valid()
		switch {
		case validAdapterErr && adapterErr.Code() == "canceled":
			a.persistTerminal(runtime, func() error {
				return a.callDB.Transition(context.Background(), runtime.id, call.Canceled, adapterErr.Code(), adapterErr.Message())
			})
		case validAdapterErr:
			a.persistTerminal(runtime, func() error {
				return a.callDB.Transition(context.Background(), runtime.id, call.Failed, adapterErr.Code(), adapterErr.Message())
			})
		case isAdapterErr:
			a.persistTerminal(runtime, func() error {
				return a.callDB.Transition(context.Background(), runtime.id, call.Failed, "internal_error", "adapter returned invalid error")
			})
		case errors.Is(err, call.ErrTooManyEvents), errors.Is(err, call.ErrEventBudgetExceeded):
			a.persistTerminal(runtime, func() error {
				return a.callDB.Transition(context.Background(), runtime.id, call.Failed, "event_limit", "call event limit exceeded")
			})
		case errors.Is(err, call.ErrPayloadTooLarge):
			a.persistTerminal(runtime, func() error {
				return a.callDB.Transition(context.Background(), runtime.id, call.Failed, "payload_too_large", "adapter payload exceeded limit")
			})
		case errors.Is(err, context.Canceled):
			a.persistTerminal(runtime, func() error {
				return a.callDB.Transition(context.Background(), runtime.id, call.Failed, "app_closed", "call canceled during application shutdown")
			})
		default:
			a.persistTerminal(runtime, func() error {
				return a.callDB.Transition(context.Background(), runtime.id, call.Failed, "invoke_failed", "adapter invocation failed")
			})
		}
		return
	}
	if sink.result == nil {
		a.persistTerminal(runtime, func() error {
			return a.callDB.Transition(context.Background(), runtime.id, call.Failed, "missing_result", "adapter invocation returned no result")
		})
		return
	}
	a.persistTerminal(runtime, func() error {
		return a.callDB.Complete(context.Background(), runtime.id, sink.result)
	})
}

func (a *App) persistTerminal(runtime *callRuntime, terminal func() error) {
	if err := terminal(); err != nil {
		a.fallbackTerminal(runtime, err)
	}
}

func (a *App) fallbackTerminal(runtime *callRuntime, primaryErr error) {
	fallbackErr := a.callDB.Transition(context.Background(), runtime.id, call.Failed, "internal_error", ErrCallPersistence.Error())
	if fallbackErr == nil {
		return
	}
	terminalErr := errors.Join(primaryErr, fallbackErr)
	a.mu.Lock()
	runtime.terminalErr = terminalErr
	a.recordCallErrorLocked(runtime.id, terminalErr)
	a.mu.Unlock()
}

func (a *App) recordCallError(id string, err error) {
	a.mu.Lock()
	a.recordCallErrorLocked(id, err)
	a.mu.Unlock()
}

func (a *App) recordCallErrorLocked(id string, err error) {
	if _, exists := a.callErrors[id]; exists {
		a.callErrors[id] = err
		return
	}
	if len(a.callErrorOrder) < maxRuntimeCallErrors {
		a.callErrorOrder = append(a.callErrorOrder, id)
	} else {
		evicted := a.callErrorOrder[a.callErrorNext]
		delete(a.callErrors, evicted)
		a.callErrorOrder[a.callErrorNext] = id
		a.callErrorNext = (a.callErrorNext + 1) % maxRuntimeCallErrors
	}
	a.callErrors[id] = err
}

func (a *App) hasCallError(id string) bool {
	a.mu.Lock()
	_, exists := a.callErrors[id]
	a.mu.Unlock()
	return exists
}

func (a *App) runtimePersistenceFailed(runtime *callRuntime) bool {
	a.mu.Lock()
	failed := runtime.terminalErr != nil
	a.mu.Unlock()
	return failed
}

func (a *App) authorizedCall(ctx context.Context, identity, id string) (call.Record, error) {
	record, err := a.callDB.Get(ctx, id)
	if errors.Is(err, call.ErrNotFound) {
		return call.Record{}, ErrCallNotFound
	}
	if err != nil {
		return call.Record{}, err
	}
	if record.Call.IdentityID != identity && !a.IsAdmin(ctx, identity) {
		return call.Record{}, ErrCallNotFound
	}
	return record, nil
}

func (a *App) GetCall(ctx context.Context, identity, id string) (call.Record, error) {
	record, err := a.authorizedCall(ctx, identity, id)
	if err != nil {
		return call.Record{}, err
	}
	if a.hasCallError(id) {
		return call.Record{}, ErrCallPersistence
	}
	return record, nil
}

func (a *App) ListCallEvents(ctx context.Context, identity, id string) ([]call.Event, error) {
	page, err := a.ListCallEventPage(ctx, identity, id, 0, call.MaxEventPageSize)
	return page.Events, err
}

func (a *App) ListCallEventPage(ctx context.Context, identity, id string, after, limit int) (call.EventPage, error) {
	if _, err := a.authorizedCall(ctx, identity, id); err != nil {
		return call.EventPage{}, err
	}
	page, err := a.callDB.ListEventPage(ctx, id, after, limit)
	if errors.Is(err, call.ErrNotFound) {
		return call.EventPage{}, ErrCallNotFound
	}
	return page, err
}

func (a *App) CancelCall(ctx context.Context, identity, id string) error {
	record, err := a.authorizedCall(ctx, identity, id)
	if err != nil {
		return err
	}
	if a.hasCallError(id) {
		return ErrCallPersistence
	}
	if a.cancelBeforeRuntimeSnapshot != nil {
		a.cancelBeforeRuntimeSnapshot()
	}
	c, err := a.cat.GetCapability(ctx, record.Call.CapabilityID)
	if err != nil {
		return ErrCallNotFound
	}
	if !c.Lifecycle.Cancelable {
		return ErrCallNotCancelable
	}
	lease, err := a.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer lease.done()
	a.mutation.RLock()
	defer a.mutation.RUnlock()
	if err := lease.check(); err != nil {
		return err
	}
	a.mu.Lock()
	_, persistenceFailed := a.callErrors[id]
	runtime := a.activeCalls[id]
	if runtime != nil {
		lease.target = &providerSession{provider: runtime.provider, adapter: runtime.adapter}
	}
	a.mu.Unlock()
	if persistenceFailed {
		return ErrCallPersistence
	}
	if runtime == nil {
		return ErrCallNotActive
	}
	select {
	case <-runtime.done:
		if a.runtimePersistenceFailed(runtime) {
			return ErrCallPersistence
		}
		return ErrCallNotActive
	default:
	}
	select {
	case <-runtime.ready:
	case <-runtime.done:
		if a.runtimePersistenceFailed(runtime) {
			return ErrCallPersistence
		}
		return ErrCallNotActive
	case <-lease.ctx.Done():
		return lease.result(lease.ctx.Err())
	}
	if err := runtime.adapter.Cancel(lease.ctx, runtime.provider, id); err != nil {
		var adapterErr *provider.AdapterError
		if errors.As(err, &adapterErr) {
			if adapterErr == nil || !adapterErr.Valid() {
				return ErrCallCancelFailed
			}
			if adapterErr.Code() == "not_cancelable" {
				return ErrCallNotActive
			}
		}
		return err
	}
	select {
	case <-runtime.done:
		if a.runtimePersistenceFailed(runtime) {
			return ErrCallPersistence
		}
		return nil
	case <-lease.ctx.Done():
		return lease.result(lease.ctx.Err())
	}
}
