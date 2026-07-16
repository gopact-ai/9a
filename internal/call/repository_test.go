package call

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/store"
)

func openCallRepository(t *testing.T) (*Repository, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ninea.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewRepository(db), path
}

func submittedCall(id string) Call {
	return Call{ID: id, CapabilityID: "echo/demo/echo", IdentityID: "agent", State: Submitted}
}

func quotaCall(id, identity string) Call {
	return Call{ID: id, CapabilityID: "echo/demo/echo", IdentityID: identity, State: Submitted}
}

func seedQuotaCalls(t *testing.T, repo *Repository, prefix string, count int, identity func(int) string, state State) {
	t.Helper()
	tx, err := repo.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := range count {
		id := fmt.Sprintf("%s-%d", prefix, i)
		if _, err := tx.Exec(`INSERT INTO calls(id,capability_id,identity_id,state,created_at,updated_at) VALUES(?,?,?,?,?,?)`, id, "echo/demo/echo", identity(i), state, "2026-07-12T00:00:00Z", "2026-07-12T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO call_inputs(call_id,data_json) VALUES(?,?)`, id, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO call_event_usage(call_id,event_count,byte_count) VALUES(?,0,0)`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO call_storage_usage(call_id,byte_count) VALUES(?,?)`, id, len(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func setCallStorageUsage(t *testing.T, repo *Repository, id string, bytes int64) {
	t.Helper()
	if _, err := repo.db.Exec(`UPDATE call_storage_usage SET byte_count=? WHERE call_id=?`, bytes, id); err != nil {
		t.Fatal(err)
	}
}

func requireQuotaError(t *testing.T, err error) {
	t.Helper()
	if err == nil || !errors.Is(err, ErrQuotaExceeded) || err.Error() != "call quota exceeded" {
		t.Fatalf("quota error=%v", err)
	}
}

func requireQuotaLimit(t *testing.T, err, limit error) {
	t.Helper()
	requireQuotaError(t, err)
	if !errors.Is(err, limit) {
		t.Fatalf("quota error=%v, want limit %v", err, limit)
	}
}

func TestRepositoryCreateQuotaBoundaries(t *testing.T) {
	const (
		identityActiveLimit   = 8
		globalActiveLimit     = 64
		identityRetainedLimit = 1_000
		globalRetainedLimit   = 10_000
		identityByteLimit     = int64(256 << 20)
		globalByteLimit       = int64(2 << 30)
	)
	ctx := context.Background()
	input := json.RawMessage(`{}`)

	t.Run("identity active", func(t *testing.T) {
		repo, _ := openCallRepository(t)
		seedQuotaCalls(t, repo, "call-identity-active", identityActiveLimit-1, func(int) string { return "owner" }, Submitted)
		if err := repo.Create(ctx, quotaCall("call-identity-active-boundary", "owner"), input); err != nil {
			t.Fatalf("exact boundary Create() error=%v", err)
		}
		requireQuotaLimit(t, repo.Create(ctx, quotaCall("call-identity-active-over", "owner"), input), ErrIdentityActiveQuota)
	})

	t.Run("global active", func(t *testing.T) {
		repo, _ := openCallRepository(t)
		seedQuotaCalls(t, repo, "call-global-active", globalActiveLimit-1, func(i int) string { return fmt.Sprintf("agent-%d", i) }, Submitted)
		if err := repo.Create(ctx, quotaCall("call-global-active-boundary", "boundary-agent"), input); err != nil {
			t.Fatalf("exact boundary Create() error=%v", err)
		}
		requireQuotaLimit(t, repo.Create(ctx, quotaCall("call-global-active-over", "other-agent"), input), ErrGlobalActiveQuota)
	})

	t.Run("identity retained count", func(t *testing.T) {
		repo, _ := openCallRepository(t)
		seedQuotaCalls(t, repo, "call-identity-retained", identityRetainedLimit-1, func(int) string { return "owner" }, Failed)
		if err := repo.Create(ctx, quotaCall("call-identity-retained-boundary", "owner"), input); err != nil {
			t.Fatalf("exact boundary Create() error=%v", err)
		}
		if err := repo.Create(ctx, quotaCall("call-identity-retained-over", "owner"), input); err != nil {
			t.Fatalf("pruning Create() error=%v", err)
		}
		var retained int
		if err := repo.db.QueryRow(`SELECT count(*) FROM calls WHERE identity_id='owner'`).Scan(&retained); err != nil {
			t.Fatal(err)
		}
		if retained != identityRetainedLimit {
			t.Fatalf("retained calls=%d want=%d", retained, identityRetainedLimit)
		}
		if _, err := repo.Get(ctx, "call-identity-retained-0"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("oldest terminal Get() error=%v", err)
		}
		if _, err := repo.Get(ctx, "call-identity-retained-boundary"); err != nil {
			t.Fatalf("active boundary call was pruned: %v", err)
		}
	})

	t.Run("global retained count", func(t *testing.T) {
		repo, _ := openCallRepository(t)
		seedQuotaCalls(t, repo, "call-global-retained", globalRetainedLimit-1, func(i int) string { return fmt.Sprintf("agent-%d", i) }, Failed)
		if err := repo.Create(ctx, quotaCall("call-global-retained-boundary", "boundary-agent"), input); err != nil {
			t.Fatalf("exact boundary Create() error=%v", err)
		}
		if err := repo.Create(ctx, quotaCall("call-global-retained-over", "other-agent"), input); err != nil {
			t.Fatalf("pruning Create() error=%v", err)
		}
		var retained int
		if err := repo.db.QueryRow(`SELECT count(*) FROM calls`).Scan(&retained); err != nil {
			t.Fatal(err)
		}
		if retained != globalRetainedLimit {
			t.Fatalf("retained calls=%d want=%d", retained, globalRetainedLimit)
		}
		if _, err := repo.Get(ctx, "call-global-retained-0"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("oldest terminal Get() error=%v", err)
		}
		if _, err := repo.Get(ctx, "call-global-retained-boundary"); err != nil {
			t.Fatalf("active boundary call was pruned: %v", err)
		}
	})

	t.Run("identity retained bytes", func(t *testing.T) {
		repo, _ := openCallRepository(t)
		seedQuotaCalls(t, repo, "call-identity-bytes", 1, func(int) string { return "owner" }, Failed)
		setCallStorageUsage(t, repo, "call-identity-bytes-0", identityByteLimit-int64(len(input)))
		if err := repo.Create(ctx, quotaCall("call-identity-bytes-boundary", "owner"), input); err != nil {
			t.Fatalf("exact boundary Create() error=%v", err)
		}
		if err := repo.Create(ctx, quotaCall("call-identity-bytes-over", "owner"), input); err != nil {
			t.Fatalf("pruning Create() error=%v", err)
		}
		if got := totalStorageUsage(t, repo); got != int64(2*len(input)) {
			t.Fatalf("storage bytes=%d want=%d", got, 2*len(input))
		}
		if _, err := repo.Get(ctx, "call-identity-bytes-0"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("terminal byte owner Get() error=%v", err)
		}
		if _, err := repo.Get(ctx, "call-identity-bytes-boundary"); err != nil {
			t.Fatalf("active boundary call was pruned: %v", err)
		}
	})

	t.Run("global retained bytes", func(t *testing.T) {
		repo, _ := openCallRepository(t)
		seedQuotaCalls(t, repo, "call-global-bytes", 1, func(int) string { return "other" }, Failed)
		setCallStorageUsage(t, repo, "call-global-bytes-0", globalByteLimit-int64(len(input)))
		if err := repo.Create(ctx, quotaCall("call-global-bytes-boundary", "owner"), input); err != nil {
			t.Fatalf("exact boundary Create() error=%v", err)
		}
		if err := repo.Create(ctx, quotaCall("call-global-bytes-over", "another"), input); err != nil {
			t.Fatalf("pruning Create() error=%v", err)
		}
		if got := totalStorageUsage(t, repo); got != int64(2*len(input)) {
			t.Fatalf("storage bytes=%d want=%d", got, 2*len(input))
		}
		if _, err := repo.Get(ctx, "call-global-bytes-0"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("terminal byte owner Get() error=%v", err)
		}
		if _, err := repo.Get(ctx, "call-global-bytes-boundary"); err != nil {
			t.Fatalf("active boundary call was pruned: %v", err)
		}
	})

	t.Run("active bytes remain a hard limit", func(t *testing.T) {
		repo, _ := openCallRepository(t)
		seedQuotaCalls(t, repo, "call-active-bytes", 1, func(int) string { return "owner" }, Submitted)
		setCallStorageUsage(t, repo, "call-active-bytes-0", identityByteLimit)
		requireQuotaLimit(t, repo.Create(ctx, quotaCall("call-active-bytes-over", "owner"), input), ErrIdentityByteQuota)
		if _, err := repo.Get(ctx, "call-active-bytes-0"); err != nil {
			t.Fatalf("active byte owner was pruned: %v", err)
		}
	})

	t.Run("insufficient pruning rolls back", func(t *testing.T) {
		repo, _ := openCallRepository(t)
		seedQuotaCalls(t, repo, "call-rollback-terminal", 1, func(int) string { return "owner" }, Failed)
		seedQuotaCalls(t, repo, "call-rollback-active", 1, func(int) string { return "owner" }, Submitted)
		setCallStorageUsage(t, repo, "call-rollback-terminal-0", 1)
		setCallStorageUsage(t, repo, "call-rollback-active-0", identityByteLimit-1)
		requireQuotaLimit(t, repo.Create(ctx, quotaCall("call-rollback-over", "owner"), input), ErrIdentityByteQuota)
		if _, err := repo.Get(ctx, "call-rollback-terminal-0"); err != nil {
			t.Fatalf("failed admission committed terminal pruning: %v", err)
		}
		if _, err := repo.Get(ctx, "call-rollback-active-0"); err != nil {
			t.Fatalf("failed admission removed active call: %v", err)
		}
	})
}

func TestRepositoryCreateQuotaIsolatesIdentitiesAndSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	repo, path := openCallRepository(t)
	seedQuotaCalls(t, repo, "call-full-owner", 8, func(int) string { return "full-owner" }, Submitted)
	seedQuotaCalls(t, repo, "call-retained-owner", MaxIdentityRetainedCalls, func(int) string { return "retained-owner" }, Failed)
	if err := repo.Create(ctx, quotaCall("call-other-owner", "other-owner"), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("other identity Create() error=%v", err)
	}
	if err := repo.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reopened := NewRepository(db)
	requireQuotaLimit(t, reopened.Create(ctx, quotaCall("call-full-owner-after-reopen", "full-owner"), json.RawMessage(`{}`)), ErrIdentityActiveQuota)
	if err := reopened.Create(ctx, quotaCall("call-retained-owner-after-reopen", "retained-owner"), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("retained owner Create() after reopen error=%v", err)
	}
}

func TestRepositoryConcurrentCreatePrunesTerminalCallsTransactionally(t *testing.T) {
	ctx := context.Background()
	repo, path := openCallRepository(t)
	seedQuotaCalls(t, repo, "call-concurrent-retained", MaxIdentityRetainedCalls, func(int) string { return "owner" }, Failed)
	dbTwo, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbTwo.Close() })
	repos := []*Repository{repo, NewRepository(dbTwo)}

	const attempts = MaxIdentityActiveCalls
	start := make(chan struct{})
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs <- repos[i%len(repos)].Create(ctx, quotaCall(fmt.Sprintf("call-concurrent-prune-%d", i), "owner"), json.RawMessage(`{}`))
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent pruning Create() error=%v", err)
		}
	}
	var retained, active int
	if err := repo.db.QueryRow(`SELECT count(*),sum(CASE WHEN state NOT IN ('completed','failed','canceled','rejected') THEN 1 ELSE 0 END) FROM calls WHERE identity_id='owner'`).Scan(&retained, &active); err != nil {
		t.Fatal(err)
	}
	if retained != MaxIdentityRetainedCalls || active != attempts {
		t.Fatalf("retained=%d active=%d want retained=%d active=%d", retained, active, MaxIdentityRetainedCalls, attempts)
	}
}

func TestRepositoryConcurrentAdmissionHonorsIdentityActiveQuota(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ninea.db")
	dbOne, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbOne.Close() })
	dbTwo, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbTwo.Close() })
	repos := []*Repository{NewRepository(dbOne), NewRepository(dbTwo)}

	const attempts = 32
	start := make(chan struct{})
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs <- repos[i%len(repos)].Create(ctx, quotaCall(fmt.Sprintf("call-concurrent-admission-%d", i), "owner"), json.RawMessage(`{}`))
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	succeeded := 0
	for err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		requireQuotaLimit(t, err, ErrIdentityActiveQuota)
	}
	if succeeded != 8 {
		t.Fatalf("successful creates=%d want=8", succeeded)
	}
}

func totalStorageUsage(t *testing.T, repo *Repository) int64 {
	t.Helper()
	var total int64
	if err := repo.db.QueryRow(`SELECT coalesce(sum(byte_count),0) FROM call_storage_usage`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	return total
}

func TestRepositoryTerminalReleasesActiveButRetainsCountAndBytes(t *testing.T) {
	ctx := context.Background()
	repo, _ := openCallRepository(t)
	seedQuotaCalls(t, repo, "call-terminal-release", MaxIdentityActiveCalls, func(int) string { return "owner" }, Submitted)
	requireQuotaError(t, repo.Create(ctx, quotaCall("call-before-terminal", "owner"), json.RawMessage(`{}`)))
	if err := repo.Transition(ctx, "call-terminal-release-0", Failed, "failed", "finished"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, quotaCall("call-after-terminal", "owner"), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("replacement Create() error=%v", err)
	}
	var retained int
	if err := repo.db.QueryRow(`SELECT count(*) FROM calls WHERE identity_id='owner'`).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != MaxIdentityActiveCalls+1 || totalStorageUsage(t, repo) != int64((MaxIdentityActiveCalls+1)*len(`{}`)) {
		t.Fatalf("retained=%d bytes=%d", retained, totalStorageUsage(t, repo))
	}
	requireQuotaError(t, repo.Create(ctx, quotaCall("call-after-refill", "owner"), json.RawMessage(`{}`)))
}

func TestRepositoryCompleteQuotaIsAtomicAtIdentityByteBoundary(t *testing.T) {
	ctx := context.Background()
	result := json.RawMessage(`{"ok":true}`)

	t.Run("exact boundary", func(t *testing.T) {
		repo, _ := openCallRepository(t)
		id := "call-complete-byte-boundary"
		if err := repo.Create(ctx, quotaCall(id, "owner"), json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if err := repo.Transition(ctx, id, Working, "", ""); err != nil {
			t.Fatal(err)
		}
		setCallStorageUsage(t, repo, id, MaxIdentityRetainedBytes-int64(len(result)))
		if err := repo.Complete(ctx, id, result); err != nil {
			t.Fatalf("Complete() error=%v", err)
		}
		if got := totalStorageUsage(t, repo); got != MaxIdentityRetainedBytes {
			t.Fatalf("storage bytes=%d want=%d", got, MaxIdentityRetainedBytes)
		}
	})

	t.Run("over boundary rolls back", func(t *testing.T) {
		repo, _ := openCallRepository(t)
		id := "call-complete-byte-over"
		if err := repo.Create(ctx, quotaCall(id, "owner"), json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if err := repo.Transition(ctx, id, Working, "", ""); err != nil {
			t.Fatal(err)
		}
		setCallStorageUsage(t, repo, id, MaxIdentityRetainedBytes)
		err := repo.Complete(ctx, id, result)
		if !errors.Is(err, ErrQuotaExceeded) || !errors.Is(err, ErrIdentityByteQuota) {
			t.Fatalf("Complete() error=%v", err)
		}
		record, getErr := repo.Get(ctx, id)
		if getErr != nil || record.Call.State != Working || record.Result != nil || totalStorageUsage(t, repo) != MaxIdentityRetainedBytes {
			t.Fatalf("record=%#v storage=%d err=%v", record, totalStorageUsage(t, repo), getErr)
		}
	})
}

func TestRepositoryAppendEventQuotaIsAtomicAtGlobalByteBoundary(t *testing.T) {
	ctx := context.Background()
	envelope := json.RawMessage(`{"kind":"event"}`)
	repo, _ := openCallRepository(t)
	target := "call-event-byte-boundary"
	if err := repo.Create(ctx, quotaCall(target, "event-owner"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	seedQuotaCalls(t, repo, "call-global-byte-fill", 8, func(i int) string { return fmt.Sprintf("fill-%d", i) }, Failed)
	for i := range 7 {
		setCallStorageUsage(t, repo, fmt.Sprintf("call-global-byte-fill-%d", i), MaxIdentityRetainedBytes)
	}
	lastBytes := MaxGlobalRetainedBytes - 7*MaxIdentityRetainedBytes - int64(len(`{}`)) - int64(len(envelope))
	setCallStorageUsage(t, repo, "call-global-byte-fill-7", lastBytes)

	if _, err := repo.AppendEvent(ctx, target, envelope); err != nil {
		t.Fatalf("exact boundary AppendEvent() error=%v", err)
	}
	if got := totalStorageUsage(t, repo); got != MaxGlobalRetainedBytes {
		t.Fatalf("storage bytes=%d want=%d", got, MaxGlobalRetainedBytes)
	}
	_, err := repo.AppendEvent(ctx, target, json.RawMessage(`0`))
	if !errors.Is(err, ErrQuotaExceeded) || !errors.Is(err, ErrGlobalByteQuota) {
		t.Fatalf("over boundary AppendEvent() error=%v", err)
	}
	events, listErr := repo.ListEvents(ctx, target, 10)
	if listErr != nil || len(events) != 1 || totalStorageUsage(t, repo) != MaxGlobalRetainedBytes {
		t.Fatalf("events=%#v storage=%d err=%v", events, totalStorageUsage(t, repo), listErr)
	}
}

func TestRepositoryCreateGetTransitionAndCompleteIdempotently(t *testing.T) {
	ctx := context.Background()
	repo, _ := openCallRepository(t)
	input := json.RawMessage(`{"secret":"provider-input"}`)
	if err := repo.Create(ctx, submittedCall("call-one"), input); err != nil {
		t.Fatal(err)
	}
	record, err := repo.Get(ctx, "call-one")
	if err != nil {
		t.Fatal(err)
	}
	if record.Call.IdentityID != "agent" || string(record.Input) != string(input) || record.Result != nil || record.Call.CreatedAt.IsZero() || record.Call.UpdatedAt.IsZero() {
		t.Fatalf("Get()=%#v", record)
	}
	if err := repo.Transition(ctx, "call-one", Working, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.Transition(ctx, "call-one", Submitted, "", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("invalid Transition() error=%v", err)
	}
	result := json.RawMessage(`{"ok":true}`)
	if err := repo.Complete(ctx, "call-one", result); err != nil {
		t.Fatal(err)
	}
	if err := repo.Complete(ctx, "call-one", result); err != nil {
		t.Fatalf("idempotent Complete() error=%v", err)
	}
	record, err = repo.Get(ctx, "call-one")
	if err != nil || record.Call.State != Completed || string(record.Result) != string(result) {
		t.Fatalf("completed Get()=%#v, %v", record, err)
	}
}

func TestRepositoryCompleteRollsBackResultWhenCASLoses(t *testing.T) {
	ctx := context.Background()
	repo, _ := openCallRepository(t)
	if err := repo.Create(ctx, submittedCall("call-complete-cas"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Transition(ctx, "call-complete-cas", Working, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `CREATE TRIGGER lose_complete_cas BEFORE UPDATE OF state ON calls WHEN OLD.id='call-complete-cas' AND NEW.state='completed' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if err := repo.Complete(ctx, "call-complete-cas", json.RawMessage(`{"ok":true}`)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Complete() error=%v", err)
	}
	var results int
	if err := repo.db.QueryRowContext(ctx, `SELECT count(*) FROM call_results WHERE call_id='call-complete-cas'`).Scan(&results); err != nil {
		t.Fatal(err)
	}
	record, err := repo.Get(ctx, "call-complete-cas")
	if err != nil || results != 0 || record.Call.State != Working || record.Result != nil {
		t.Fatalf("results=%d record=%#v err=%v", results, record, err)
	}
}

func TestRepositoryCompleteAndFailCompeteWithoutSplitTerminalState(t *testing.T) {
	ctx := context.Background()
	for i := range 50 {
		repo, _ := openCallRepository(t)
		id := fmt.Sprintf("call-terminal-race-%d", i)
		if err := repo.Create(ctx, submittedCall(id), json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if err := repo.Transition(ctx, id, Working, "", ""); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			errs <- repo.Complete(ctx, id, json.RawMessage(`{"ok":true}`))
		}()
		go func() {
			<-start
			errs <- repo.Transition(ctx, id, Failed, "failed", "failed safely")
		}()
		close(start)
		first, second := <-errs, <-errs
		if (first == nil) == (second == nil) {
			t.Fatalf("iteration %d errors=(%v,%v), want exactly one success", i, first, second)
		}
		record, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		switch record.Call.State {
		case Completed:
			if string(record.Result) != `{"ok":true}` {
				t.Fatalf("iteration %d completed without result: %#v", i, record)
			}
		case Failed:
			if record.Result != nil {
				t.Fatalf("iteration %d failed with committed result: %#v", i, record)
			}
		default:
			t.Fatalf("iteration %d nonterminal record: %#v", i, record)
		}
	}
}

func TestRepositoryConcurrentAppendAssignsMonotonicSequenceAndLimitsList(t *testing.T) {
	ctx := context.Background()
	repo, _ := openCallRepository(t)
	if err := repo.Create(ctx, submittedCall("call-events"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	const count = 100
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			envelope, _ := json.Marshal(map[string]any{"kind": "event", "value": i})
			_, err := repo.AppendEvent(ctx, "call-events", envelope)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := repo.ListEvents(ctx, "call-events", MaxEventList+100)
	if err != nil || len(events) != count {
		t.Fatalf("ListEvents() len=%d err=%v", len(events), err)
	}
	for i, event := range events {
		if event.Sequence != i+1 || event.CallID != "call-events" {
			t.Fatalf("event[%d]=%#v", i, event)
		}
	}
	if _, err := repo.ListEvents(ctx, "missing", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ListEvents() error=%v", err)
	}
}

func TestRepositoryEventCountAndPayloadBounds(t *testing.T) {
	ctx := context.Background()
	repo, _ := openCallRepository(t)
	if err := repo.Create(ctx, submittedCall("call-bounds"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, "call-bounds", make(json.RawMessage, MaxEventEnvelopeBytes+1)); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("oversized AppendEvent() error=%v", err)
	}
	for i := 1; i <= MaxEvents; i++ {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO events(call_id,sequence,data_json) VALUES(?,?,?)`, "call-bounds", i, []byte(`{"kind":"event"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE call_event_usage SET event_count=?,byte_count=? WHERE call_id=?`, MaxEvents, MaxEvents*len(`{"kind":"event"}`), "call-bounds"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, "call-bounds", json.RawMessage(`{"kind":"event"}`)); !errors.Is(err, ErrTooManyEvents) {
		t.Fatalf("event overflow error=%v", err)
	}
	events, err := repo.ListEvents(ctx, "call-bounds", MaxEventList+1)
	if err != nil || len(events) != MaxEventList {
		t.Fatalf("hard-limited ListEvents() len=%d err=%v", len(events), err)
	}
}

func TestRepositoryCreateRollsBackAndPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	repo, path := openCallRepository(t)
	if _, err := repo.db.ExecContext(ctx, `CREATE TRIGGER reject_call_input BEFORE INSERT ON call_inputs WHEN NEW.call_id='call-rollback' BEGIN SELECT RAISE(FAIL,'reject input'); END`); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, submittedCall("call-rollback"), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Create unexpectedly succeeded")
	}
	if _, err := repo.Get(ctx, "call-rollback"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back Get() error=%v", err)
	}
	if err := repo.Create(ctx, submittedCall("call-persisted"), json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	reopened := NewRepository(db)
	record, err := reopened.Get(ctx, "call-persisted")
	_ = db.Close()
	if err != nil || string(record.Input) != `{"x":1}` || record.Call.IdentityID != "agent" {
		t.Fatalf("reopened Get()=%#v, %v", record, err)
	}
}

func TestRepositoryRecoverInterruptedLeavesTerminalCallsUnchanged(t *testing.T) {
	ctx := context.Background()
	repo, _ := openCallRepository(t)
	states := []State{Created, Submitted, Working, InputRequired, AuthRequired, Completed, Failed, Canceled, Rejected}
	for _, state := range states {
		call := submittedCall("call-" + string(state))
		call.State = state
		call.CreatedAt = time.Now().UTC()
		call.UpdatedAt = call.CreatedAt
		if err := repo.Create(ctx, call, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	count, err := repo.RecoverInterrupted(ctx, "daemon_restarted", "daemon restarted")
	if err != nil || count != 5 {
		t.Fatalf("RecoverInterrupted() count=%d err=%v", count, err)
	}
	for _, state := range states {
		record, err := repo.Get(ctx, "call-"+string(state))
		if err != nil {
			t.Fatal(err)
		}
		if CanTransition(state, Failed) || state == Created {
			if record.Call.State != Failed || record.Call.Code != "daemon_restarted" {
				t.Fatalf("recovered %s=%#v", state, record.Call)
			}
		} else if record.Call.State != state {
			t.Fatalf("terminal %s changed to %s", state, record.Call.State)
		}
	}
}

func TestRepositoryRejectsUnsafeCallOwnershipAndMessages(t *testing.T) {
	repo, _ := openCallRepository(t)
	call := submittedCall("call-owner")
	call.IdentityID = ""
	if err := repo.Create(context.Background(), call, json.RawMessage(`{}`)); err == nil {
		t.Fatal("Create accepted missing owner")
	}
	if err := repo.Create(context.Background(), submittedCall("call-safe"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Transition(context.Background(), "call-safe", Failed, "x", fmt.Sprintf("%01025d", 1)); err == nil {
		t.Fatal("Transition accepted oversized message")
	}
	for name, values := range map[string][2]string{
		"unsafe code":  {"bad\ncode", "safe"},
		"control text": {"failed", "hidden\x00text"},
		"invalid utf8": {"failed", string([]byte{0xff})},
	} {
		if err := validateErrorMetadata(values[0], values[1]); err == nil {
			t.Errorf("error metadata validation accepted %s", name)
		}
	}
}

func TestRepositoryAggregateEventBudgetIsTransactionalAtBoundary(t *testing.T) {
	ctx := context.Background()
	repo, _ := openCallRepository(t)
	const aggregateLimit = 64 << 20
	if err := repo.Create(ctx, submittedCall("call-aggregate-bound"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	envelope := json.RawMessage(`{"kind":"event"}`)
	if _, err := repo.db.ExecContext(ctx, `UPDATE call_event_usage SET byte_count=? WHERE call_id=?`, aggregateLimit-len(envelope), "call-aggregate-bound"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, "call-aggregate-bound", envelope); err != nil {
		t.Fatalf("exact-limit AppendEvent error=%v", err)
	}
	var count, bytesBefore int64
	if err := repo.db.QueryRowContext(ctx, `SELECT event_count,byte_count FROM call_event_usage WHERE call_id=?`, "call-aggregate-bound").Scan(&count, &bytesBefore); err != nil {
		t.Fatal(err)
	}
	if count != 1 || bytesBefore != aggregateLimit {
		t.Fatalf("usage count=%d bytes=%d", count, bytesBefore)
	}
	if _, err := repo.AppendEvent(ctx, "call-aggregate-bound", json.RawMessage(`[]`)); err == nil {
		t.Fatal("AppendEvent accepted aggregate limit + 1")
	}
	var countAfter, bytesAfter int64
	if err := repo.db.QueryRowContext(ctx, `SELECT event_count,byte_count FROM call_event_usage WHERE call_id=?`, "call-aggregate-bound").Scan(&countAfter, &bytesAfter); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := repo.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE call_id=?`, "call-aggregate-bound").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if countAfter != count || bytesAfter != bytesBefore || events != 1 {
		t.Fatalf("rejected append mutated usage/events: count=%d bytes=%d events=%d", countAfter, bytesAfter, events)
	}
}

func TestRepositoryConcurrentAppendsCannotExceedAggregateBudget(t *testing.T) {
	ctx := context.Background()
	repo, _ := openCallRepository(t)
	const aggregateLimit = 64 << 20
	if err := repo.Create(ctx, submittedCall("call-aggregate-race"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	envelope := json.RawMessage(`{"value":1}`)
	const allowed = 50
	if _, err := repo.db.ExecContext(ctx, `UPDATE call_event_usage SET byte_count=? WHERE call_id=?`, aggregateLimit-allowed*len(envelope), "call-aggregate-race"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, allowed*2)
	var wg sync.WaitGroup
	for range allowed * 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := repo.AppendEvent(ctx, "call-aggregate-race", envelope)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != allowed {
		t.Fatalf("successful appends=%d want=%d", successes, allowed)
	}
	var count, bytes int64
	if err := repo.db.QueryRowContext(ctx, `SELECT event_count,byte_count FROM call_event_usage WHERE call_id=?`, "call-aggregate-race").Scan(&count, &bytes); err != nil {
		t.Fatal(err)
	}
	if count != allowed || bytes != aggregateLimit {
		t.Fatalf("usage count=%d bytes=%d", count, bytes)
	}
	events, err := repo.ListEvents(ctx, "call-aggregate-race", allowed*2)
	if err != nil || len(events) != allowed {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	for i, event := range events {
		if event.Sequence != i+1 {
			t.Fatalf("event[%d] sequence=%d", i, event.Sequence)
		}
	}
}

func TestRepositoryEventUsageRequiresInvariantAndRejectsIntegerOverflow(t *testing.T) {
	ctx := context.Background()
	repo, _ := openCallRepository(t)
	if err := repo.Create(ctx, submittedCall("call-usage-invariant"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM call_event_usage WHERE call_id=?`, "call-usage-invariant"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, "call-usage-invariant", json.RawMessage(`{"new":true}`)); !errors.Is(err, ErrStorageInvariant) {
		t.Fatalf("missing event usage error=%v", err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO call_event_usage(call_id,event_count,byte_count) VALUES(?,?,?)`, "call-usage-invariant", 3, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendEvent(ctx, "call-usage-invariant", json.RawMessage(`[]`)); err == nil {
		t.Fatal("AppendEvent accepted overflowing byte count")
	}
	var count, bytes int64
	if err := repo.db.QueryRowContext(ctx, `SELECT event_count,byte_count FROM call_event_usage WHERE call_id=?`, "call-usage-invariant").Scan(&count, &bytes); err != nil {
		t.Fatal(err)
	}
	if count != 3 || bytes != math.MaxInt64 {
		t.Fatalf("overflow rejection mutated usage count=%d bytes=%d", count, bytes)
	}
}

func TestRepositoryEventPagesBoundBytesAndReconstructOrder(t *testing.T) {
	ctx := context.Background()
	repo, _ := openCallRepository(t)
	if err := repo.Create(ctx, submittedCall("call-page-bytes"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	large := append([]byte{'"'}, make([]byte, (9<<20)-2)...)
	for i := 1; i < len(large); i++ {
		large[i] = 'x'
	}
	large = append(large, '"')
	for sequence := 1; sequence <= 3; sequence++ {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO events(call_id,sequence,data_json) VALUES(?,?,?)`, "call-page-bytes", sequence, large); err != nil {
			t.Fatal(err)
		}
	}
	var sequences []int
	after := 0
	for {
		page, err := repo.ListEventPage(ctx, "call-page-bytes", after, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) != 1 {
			t.Fatalf("large event page len=%d", len(page.Events))
		}
		sequences = append(sequences, page.Events[0].Sequence)
		if !page.HasMore {
			break
		}
		if page.NextAfter != page.Events[len(page.Events)-1].Sequence {
			t.Fatalf("next_after=%d events=%#v", page.NextAfter, page.Events)
		}
		after = page.NextAfter
	}
	if fmt.Sprint(sequences) != "[1 2 3]" {
		t.Fatalf("reconstructed sequences=%v", sequences)
	}
}

func TestRepositoryEventPagesCapRequestedCount(t *testing.T) {
	ctx := context.Background()
	repo, _ := openCallRepository(t)
	if err := repo.Create(ctx, submittedCall("call-page-count"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	for sequence := 1; sequence <= 150; sequence++ {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO events(call_id,sequence,data_json) VALUES(?,?,?)`, "call-page-count", sequence, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := repo.ListEventPage(ctx, "call-page-count", 0, math.MaxInt)
	if err != nil || len(first.Events) != 100 || !first.HasMore || first.NextAfter != 100 {
		t.Fatalf("first page=%#v err=%v", first, err)
	}
	second, err := repo.ListEventPage(ctx, "call-page-count", first.NextAfter, math.MaxInt)
	if err != nil || len(second.Events) != 50 || second.HasMore || second.NextAfter != 150 {
		t.Fatalf("second page=%#v err=%v", second, err)
	}
}
