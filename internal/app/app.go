package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gopact-ai/9a/internal/authn"
	"github.com/gopact-ai/9a/internal/authz"
	"github.com/gopact-ai/9a/internal/buildinfo"
	"github.com/gopact-ai/9a/internal/builtin"
	"github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/declarative"
	"github.com/gopact-ai/9a/internal/projection"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/provider/a2a"
	"github.com/gopact-ai/9a/internal/provider/mcp"
	"github.com/gopact-ai/9a/internal/search"
	"github.com/gopact-ai/9a/internal/secret"
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
	ResolveWorkspaceCapability(context.Context, string, string) (capability.Capability, error)
	ListProviders(context.Context) ([]provider.Provider, error)
}

type App struct {
	db             *sql.DB
	cat            catalogRepository
	az             *authz.Service
	authn          *authn.Service
	search         *search.Service
	secrets        *secret.Service
	mu             sync.RWMutex
	mutation       sync.RWMutex
	providerGateMu sync.Mutex
	providerGates  map[string]*sync.RWMutex
	approvalMu     sync.Mutex
	approvals      map[string]approvalChallenge
	approvalOrder  []string
	state          appState
	leases         map[*operationLease]struct{}
	idle           chan struct{}
	closeDone      chan struct{}
	closeErr       error
	providers      map[string]provider.Provider
	adapters       map[string]provider.Adapter
	callDB         *call.Repository
	activeCalls    map[string]*callRuntime
	callErrors     map[string]error
	callErrorOrder []string
	callErrorNext  int
	projections    *projection.Manager
}

func New(db *sql.DB) *App {
	return NewWithSecretBackend(db, secret.NewKeyringBackend())
}

func NewWithSecretBackend(db *sql.DB, backend secret.Backend) *App {
	az := authz.New(db)
	secrets := secret.NewService(db, backend)
	idle := make(chan struct{})
	close(idle)
	builtinSkill, err := builtin.UsingNineA(buildinfo.Version)
	if err != nil {
		panic(err)
	}
	return &App{db: db, cat: catalog.New(db), az: az, authn: authn.New(db), search: search.New(db, az), secrets: secrets, leases: map[*operationLease]struct{}{}, idle: idle, closeDone: make(chan struct{}), providers: map[string]provider.Provider{}, adapters: builtInAdapters(secrets), providerGates: map[string]*sync.RWMutex{}, approvals: map[string]approvalChallenge{}, callDB: call.NewRepository(db), activeCalls: map[string]*callRuntime{}, callErrors: map[string]error{}, projections: projection.New(db, builtinSkill)}
}

func (a *App) providerGate(id string) *sync.RWMutex {
	a.providerGateMu.Lock()
	defer a.providerGateMu.Unlock()
	gate := a.providerGates[id]
	if gate == nil {
		gate = &sync.RWMutex{}
		a.providerGates[id] = gate
	}
	return gate
}

func builtInAdapters(resolver secret.Resolver) map[string]provider.Adapter {
	return map[string]provider.Adapter{"mcp": mcp.New(), "a2a": a2a.NewWithResolver(resolver), "api": declarative.NewAdapterWithResolver(resolver)}
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
	defer func() { _ = tx.Rollback() }()
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
		if protocol != "mcp" && protocol != "a2a" && protocol != "api" {
			a.mu.Unlock()
			return ErrRestoreRequiresFreshApp
		}
	}
	a.mu.Unlock()
	adapters := builtInAdapters(a.secrets)
	providers, err := a.cat.ListProviders(lease.ctx)
	if err != nil {
		return lease.result(err)
	}
	currentCapabilities, err := catalog.New(a.db).ListCapabilities(lease.ctx)
	if err != nil {
		return lease.result(err)
	}
	currentByProvider := make(map[string][]capability.Capability)
	for _, item := range currentCapabilities {
		providerID := capabilityProviderID(item)
		currentByProvider[providerID] = append(currentByProvider[providerID], item)
	}
	restoredProviders := make(map[string]provider.Provider, len(providers))
	for _, p := range providers {
		adapter := adapters[p.Protocol]
		if adapter == nil {
			if deleteErr := catalog.New(a.db).DeleteProvider(lease.ctx, p.ID); deleteErr != nil {
				return fmt.Errorf("remove unsupported persisted integration %q: %w", p.Name, deleteErr)
			}
			continue
		}
		config, parseErr := integrationConfig(p)
		if parseErr != nil {
			if _, replaceErr := a.cat.ReplaceProviderCapabilities(lease.ctx, p, nil); replaceErr != nil {
				return fmt.Errorf("mark integration %q broken: %w", p.Name, replaceErr)
			}
			restoredProviders[p.ID] = p
			continue
		}
		desiredProvider, providerErr := integrationProvider(config, p.Config["workspace_root"])
		if providerErr != nil {
			if _, replaceErr := a.cat.ReplaceProviderCapabilities(lease.ctx, p, nil); replaceErr != nil {
				return fmt.Errorf("mark integration %q broken: %w", p.Name, replaceErr)
			}
			restoredProviders[p.ID] = p
			continue
		}
		if p.ID != desiredProvider.ID {
			if deleteErr := catalog.New(a.db).DeleteProvider(lease.ctx, p.ID); deleteErr != nil {
				return fmt.Errorf("remove stale integration %q: %w", p.Name, deleteErr)
			}
			if _, replaceErr := a.cat.ReplaceProviderCapabilities(lease.ctx, desiredProvider, nil); replaceErr != nil {
				return fmt.Errorf("mark integration %q broken: %w", p.Name, replaceErr)
			}
			restoredProviders[desiredProvider.ID] = desiredProvider
			continue
		}
		capabilities := currentByProvider[p.ID]
		if httpAdapter, ok := adapter.(*declarative.Adapter); ok {
			if registerErr := httpAdapter.Register(desiredProvider, config); registerErr != nil {
				if _, replaceErr := a.cat.ReplaceProviderCapabilities(lease.ctx, desiredProvider, nil); replaceErr != nil {
					return fmt.Errorf("mark integration %q broken: %w", p.Name, replaceErr)
				}
				restoredProviders[desiredProvider.ID] = desiredProvider
				continue
			}
			discovered, discoverErr := adapter.Discover(lease.ctx, desiredProvider)
			if discoverErr != nil {
				httpAdapter.Unregister(desiredProvider.ID)
				if _, replaceErr := a.cat.ReplaceProviderCapabilities(lease.ctx, desiredProvider, nil); replaceErr != nil {
					return fmt.Errorf("mark integration %q broken: %w", p.Name, replaceErr)
				}
				restoredProviders[desiredProvider.ID] = desiredProvider
				continue
			}
			capabilities = scopeIntegrationCapabilities(desiredProvider, discovered)
		} else if !sameIntegrationProvider(p, desiredProvider) {
			capabilities = nil
		}
		if !sameIntegrationProvider(p, desiredProvider) || !sameCapabilities(currentByProvider[p.ID], capabilities) {
			if _, replaceErr := a.cat.ReplaceProviderCapabilities(lease.ctx, desiredProvider, capabilities); replaceErr != nil {
				return fmt.Errorf("restore integration %q: %w", p.Name, replaceErr)
			}
		}
		if _, grantErr := a.grantIntegrationCapabilities(lease.ctx, lease.ctx, "admin", capabilities); grantErr != nil {
			return fmt.Errorf("restore integration %q grants: %w", p.Name, grantErr)
		}
		restoredProviders[desiredProvider.ID] = desiredProvider
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

func (a *App) Search(ctx context.Context, identity, root string, q search.Query) ([]CapabilitySearchResult, error) {
	canonical, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	q.WorkspaceRoot = canonical
	results, err := a.search.Search(ctx, identity, q)
	if err != nil {
		return nil, err
	}
	public := make([]CapabilitySearchResult, 0, len(results))
	includeContracts := exactPublicRef(q.Text)
	for _, result := range results {
		public = append(public, publicSearchResult(result.Capability, includeContracts))
	}
	return public, nil
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
	if err := a.projections.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("close projections: %w", err))
	}
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
