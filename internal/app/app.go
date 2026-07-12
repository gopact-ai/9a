package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	adapterreg "github.com/gopact-ai/9a/internal/adapter"
	"github.com/gopact-ai/9a/internal/authn"
	"github.com/gopact-ai/9a/internal/authz"
	"github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/generator"
	"github.com/gopact-ai/9a/internal/mount/dir"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/provider/a2a"
	"github.com/gopact-ai/9a/internal/provider/executable"
	"github.com/gopact-ai/9a/internal/provider/mcp"
	"github.com/gopact-ai/9a/internal/search"
)

var ErrRestoreRequiresFreshApp = errors.New("restore requires a fresh app")
var ErrAppClosed = errors.New("app is closed")

type appState uint8

const (
	appOpen appState = iota
	appClosing
	appClosed
)

type providerSession struct {
	provider provider.Provider
	adapter  provider.Adapter
}

type operationLease struct {
	app     *App
	ctx     context.Context
	cancel  context.CancelFunc
	target  *providerSession
	release sync.Once
}

type catalogRepository interface {
	ReplaceProviderCapabilities(context.Context, provider.Provider, []capability.Capability) (int64, error)
	GetCapability(context.Context, string) (capability.Capability, error)
	ListProviders(context.Context) ([]provider.Provider, error)
}

type App struct {
	db                          *sql.DB
	cat                         catalogRepository
	az                          *authz.Service
	authn                       *authn.Service
	search                      *search.Service
	mu                          sync.RWMutex
	mutation                    sync.RWMutex
	state                       appState
	leases                      map[*operationLease]struct{}
	idle                        chan struct{}
	closeDone                   chan struct{}
	closeErr                    error
	providers                   map[string]provider.Provider
	adapters                    map[string]provider.Adapter
	adapterDB                   *adapterreg.Repository
	callDB                      *call.Repository
	activeCalls                 map[string]*callRuntime
	callErrors                  map[string]error
	callErrorOrder              []string
	callErrorNext               int
	cancelBeforeRuntimeSnapshot func()
}

func New(db *sql.DB) *App {
	az := authz.New(db)
	idle := make(chan struct{})
	close(idle)
	return &App{db: db, cat: catalog.New(db), az: az, authn: authn.New(db), search: search.New(db, az), leases: map[*operationLease]struct{}{}, idle: idle, closeDone: make(chan struct{}), providers: map[string]provider.Provider{}, adapters: builtInAdapters(), adapterDB: adapterreg.NewRepository(db), callDB: call.NewRepository(db), activeCalls: map[string]*callRuntime{}, callErrors: map[string]error{}}
}

func builtInAdapters() map[string]provider.Adapter {
	return map[string]provider.Adapter{"mcp": mcp.New(), "a2a": a2a.New()}
}

func (a *App) beginOperation(ctx context.Context) (*operationLease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != appOpen {
		return nil, ErrAppClosed
	}
	if len(a.leases) == 0 {
		a.idle = make(chan struct{})
	}
	operationCtx, cancel := context.WithCancel(ctx)
	lease := &operationLease{app: a, ctx: operationCtx, cancel: cancel}
	a.leases[lease] = struct{}{}
	return lease, nil
}

func (l *operationLease) done() {
	l.release.Do(func() {
		l.cancel()
		l.app.mu.Lock()
		delete(l.app.leases, l)
		if len(l.app.leases) == 0 {
			close(l.app.idle)
		}
		l.app.mu.Unlock()
	})
}

func (l *operationLease) check() error {
	l.app.mu.Lock()
	state := l.app.state
	l.app.mu.Unlock()
	if state != appOpen {
		return ErrAppClosed
	}
	return l.ctx.Err()
}

func (l *operationLease) setTarget(adapter provider.Adapter, p provider.Provider) error {
	l.app.mu.Lock()
	defer l.app.mu.Unlock()
	if l.app.state != appOpen {
		return ErrAppClosed
	}
	l.target = &providerSession{provider: p, adapter: adapter}
	return nil
}

func (l *operationLease) result(err error) error {
	if err == nil {
		return nil
	}
	l.app.mu.Lock()
	state := l.app.state
	l.app.mu.Unlock()
	if state != appOpen {
		return errors.Join(ErrAppClosed, err)
	}
	return err
}

func (a *App) Bootstrap(ctx context.Context, token string) error {
	if token == "" {
		return authn.ErrInvalidToken
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tokens`).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return authn.ErrAlreadyBootstrapped
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tokens(token_hash,identity_id,created_at) VALUES(?,?,?)`, authn.TokenDigest(token), "admin", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO acl(identity_id,capability_id,permission) VALUES('admin','*','admin')`); err != nil {
		return err
	}
	return tx.Commit()
}
func (a *App) CreateToken(ctx context.Context, identity string) (string, error) {
	return a.authn.Create(ctx, identity)
}
func (a *App) Authenticate(ctx context.Context, token string) (string, error) {
	return a.authn.Authenticate(ctx, token)
}
func (a *App) IsAdmin(ctx context.Context, identity string) bool {
	return a.az.Allowed(ctx, identity, "*", authz.Admin)
}
func (a *App) NeedsBootstrap(ctx context.Context) (bool, error) {
	n, err := a.authn.Count(ctx)
	return n == 0, err
}

func (a *App) Restore(ctx context.Context) error {
	lease, err := a.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer lease.done()
	a.mutation.Lock()
	defer a.mutation.Unlock()
	if err := lease.check(); err != nil {
		return err
	}
	a.mu.Lock()
	if len(a.providers) != 0 {
		a.mu.Unlock()
		return ErrRestoreRequiresFreshApp
	}
	for protocol := range a.adapters {
		if protocol != "mcp" && protocol != "a2a" {
			a.mu.Unlock()
			return ErrRestoreRequiresFreshApp
		}
	}
	a.mu.Unlock()
	registrations, err := a.adapterDB.List(lease.ctx)
	if err != nil {
		return lease.result(err)
	}
	adapters := builtInAdapters()
	for _, registration := range registrations {
		canonical, validateErr := adapterreg.ValidateRegistration(registration.Protocol, registration.Executable)
		if validateErr != nil {
			return fmt.Errorf("restore adapter %q: %w", registration.Protocol, validateErr)
		}
		if canonical != registration.Executable {
			return fmt.Errorf("restore adapter %q: %w: executable path is not canonical", registration.Protocol, adapterreg.ErrInvalid)
		}
		if adapters[registration.Protocol] != nil {
			return fmt.Errorf("restore adapter %q: %w", registration.Protocol, adapterreg.ErrDuplicate)
		}
		external, createErr := executable.New(registration.Protocol, canonical)
		if createErr != nil {
			return fmt.Errorf("restore adapter %q: %w", registration.Protocol, adapterreg.ErrInvalid)
		}
		adapters[registration.Protocol] = external
	}
	providers, err := a.cat.ListProviders(lease.ctx)
	if err != nil {
		return lease.result(err)
	}
	restoredProviders := make(map[string]provider.Provider, len(providers))
	for _, p := range providers {
		expectedID := p.Protocol + "/" + p.Name
		if p.ID != expectedID {
			return fmt.Errorf("persisted provider %q has inconsistent provider id; expected %q", p.ID, expectedID)
		}
		if adapters[p.Protocol] == nil {
			return errors.New("persisted provider uses unsupported protocol: " + p.Protocol)
		}
		restoredProviders[p.ID] = p
	}
	if _, err := a.callDB.RecoverInterrupted(lease.ctx, "daemon_restarted", "daemon restarted before call completed"); err != nil {
		return lease.result(err)
	}
	if err := lease.check(); err != nil {
		return err
	}
	a.mu.Lock()
	if a.state != appOpen {
		a.mu.Unlock()
		return ErrAppClosed
	}
	a.adapters = adapters
	a.providers = restoredProviders
	a.mu.Unlock()
	return nil
}

func (a *App) AddAdapter(ctx context.Context, protocol, path string) error {
	lease, err := a.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer lease.done()
	a.mutation.Lock()
	defer a.mutation.Unlock()
	if err := lease.check(); err != nil {
		return err
	}
	canonical, err := adapterreg.ValidateRegistration(protocol, path)
	if err != nil {
		return err
	}
	external, err := executable.New(protocol, canonical)
	if err != nil {
		return fmt.Errorf("%w: invalid executable adapter", adapterreg.ErrInvalid)
	}
	a.mu.Lock()
	existing := a.adapters[protocol]
	a.mu.Unlock()
	if existing != nil {
		return adapterreg.ErrDuplicate
	}
	if _, err := a.adapterDB.Add(lease.ctx, protocol, canonical); err != nil {
		return lease.result(err)
	}
	a.mu.Lock()
	if a.state != appOpen {
		a.mu.Unlock()
		return ErrAppClosed
	}
	a.adapters[protocol] = external
	a.mu.Unlock()
	return nil
}

func (a *App) AddProvider(ctx context.Context, p provider.Provider) error {
	lease, err := a.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer lease.done()
	a.mutation.Lock()
	defer a.mutation.Unlock()
	if err := lease.check(); err != nil {
		return err
	}
	expectedID := p.Protocol + "/" + p.Name
	if p.ID != expectedID {
		return fmt.Errorf("provider %q has inconsistent provider id; expected %q", p.ID, expectedID)
	}
	a.mu.Lock()
	ad := a.adapters[p.Protocol]
	a.mu.Unlock()
	if ad == nil {
		return errors.New("unsupported protocol")
	}
	if err := lease.setTarget(ad, p); err != nil {
		return err
	}
	caps, e := ad.Discover(lease.ctx, p)
	if e != nil {
		return lease.result(errors.Join(e, ad.Close(lease.ctx, p)))
	}
	if _, e = a.cat.ReplaceProviderCapabilities(lease.ctx, p, caps); e != nil {
		return lease.result(errors.Join(e, ad.Close(lease.ctx, p)))
	}
	a.mu.Lock()
	if a.state != appOpen {
		a.mu.Unlock()
		return ErrAppClosed
	}
	a.providers[p.ID] = p
	a.mu.Unlock()
	return nil
}
func (a *App) Grant(ctx context.Context, identity, capID string, permissions []string) error {
	for _, p := range permissions {
		if e := a.az.Grant(ctx, identity, capID, authz.Permission(p)); e != nil {
			return e
		}
	}
	return nil
}
func (a *App) Search(ctx context.Context, identity string, q search.Query) ([]search.Result, error) {
	return a.search.Search(ctx, identity, q)
}
func (a *App) Project(ctx context.Context, identity, id, root string) error {
	if !a.az.Allowed(ctx, identity, id, authz.Read) {
		return errors.New("permission_denied")
	}
	c, e := a.cat.GetCapability(ctx, id)
	if e != nil {
		return e
	}
	s, e := generator.Render(c, false)
	if e != nil {
		return e
	}
	return dir.New().Publish(ctx, root, s)
}

type sink struct{ result json.RawMessage }

func (*sink) Started() error { return nil }
func (s *sink) Event(e provider.Event) error {
	if e.Type == "result" {
		s.result = append([]byte(nil), e.Data...)
	}
	return nil
}
func (s *sink) Artifact(string, string, []byte) error { return nil }
func (a *App) Invoke(ctx context.Context, identity, id string, input json.RawMessage) (json.RawMessage, error) {
	lease, err := a.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.done()
	a.mutation.RLock()
	defer a.mutation.RUnlock()
	if err := lease.check(); err != nil {
		return nil, err
	}
	if !a.az.Allowed(lease.ctx, identity, id, authz.Invoke) {
		if err := lease.check(); err != nil {
			return nil, err
		}
		return nil, errors.New("permission_denied")
	}
	c, e := a.cat.GetCapability(lease.ctx, id)
	if e != nil {
		return nil, lease.result(e)
	}
	a.mu.Lock()
	p, ok := a.providers[c.Source.Protocol+"/"+c.Source.Provider]
	ad := a.adapters[p.Protocol]
	state := a.state
	if !ok || ad == nil || state != appOpen {
		a.mu.Unlock()
		if state != appOpen {
			return nil, ErrAppClosed
		}
		return nil, errors.New("provider_unavailable")
	}
	lease.target = &providerSession{provider: p, adapter: ad}
	a.mu.Unlock()
	s := &sink{}
	invocationID, e := call.NewID()
	if e != nil {
		return nil, e
	}
	if e = ad.Invoke(lease.ctx, p, c, invocationID, input, s); e != nil {
		resultErr := lease.result(e)
		if errors.Is(resultErr, ErrAppClosed) {
			resultErr = errors.Join(resultErr, ad.Close(lease.ctx, p))
		}
		return nil, resultErr
	}
	if err := lease.check(); err != nil {
		return nil, errors.Join(err, ad.Close(lease.ctx, p))
	}
	return s.result, nil
}

func (a *App) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.state == appClosed {
		err := a.closeErr
		a.mu.Unlock()
		return err
	}
	if a.state == appClosing {
		done := a.closeDone
		a.mu.Unlock()
		select {
		case <-done:
			a.mu.Lock()
			err := a.closeErr
			a.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a.state = appClosing
	sessionsByID := make(map[string]providerSession, len(a.providers)+len(a.leases))
	for id, p := range a.providers {
		sessionsByID[id] = providerSession{provider: p, adapter: a.adapters[p.Protocol]}
	}
	cancels := make([]context.CancelFunc, 0, len(a.leases))
	for lease := range a.leases {
		cancels = append(cancels, lease.cancel)
		if lease.target != nil {
			sessionsByID[lease.target.provider.ID] = *lease.target
		}
	}
	idle := a.idle
	done := a.closeDone
	a.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	ids := make([]string, 0, len(sessionsByID))
	for id := range sessionsByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var errs []error
	for _, id := range ids {
		session := sessionsByID[id]
		if session.adapter == nil {
			errs = append(errs, fmt.Errorf("close provider %q: adapter unavailable", session.provider.Name))
			continue
		}
		if err := session.adapter.Close(ctx, session.provider); err != nil {
			errs = append(errs, fmt.Errorf("close provider %q: %w", session.provider.Name, err))
		}
	}
	select {
	case <-idle:
	case <-ctx.Done():
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	closeErr := errors.Join(errs...)
	a.mu.Lock()
	a.state = appClosed
	a.closeErr = closeErr
	close(done)
	a.mu.Unlock()
	return closeErr
}
