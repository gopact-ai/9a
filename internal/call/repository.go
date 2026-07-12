package call

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	MaxPayloadBytes                = 8 << 20
	MaxEventEnvelopeBytes          = ((MaxPayloadBytes + 2) / 3 * 4) + 2048
	MaxEventAggregateBytes   int64 = 64 << 20
	MaxEvents                      = 10_000
	MaxEventPageSize               = 100
	MaxEventPageBytes        int64 = 16 << 20
	MaxEventList                   = MaxEventPageSize
	MaxIdentityActiveCalls         = 8
	MaxGlobalActiveCalls           = 64
	MaxIdentityRetainedCalls       = 1_000
	MaxGlobalRetainedCalls         = 10_000
	MaxIdentityRetainedBytes       = int64(256 << 20)
	MaxGlobalRetainedBytes         = int64(2 << 30)
)

var (
	ErrNotFound            = errors.New("call not found")
	ErrInvalidTransition   = errors.New("invalid call state transition")
	ErrPayloadTooLarge     = errors.New("call payload exceeds limit")
	ErrTooManyEvents       = errors.New("call event limit exceeded")
	ErrEventBudgetExceeded = errors.New("call event byte budget exceeded")
	ErrQuotaExceeded       = errors.New("call quota exceeded")

	ErrIdentityActiveQuota   = errors.New("identity active call quota exceeded")
	ErrGlobalActiveQuota     = errors.New("global active call quota exceeded")
	ErrIdentityRetainedQuota = errors.New("identity retained call quota exceeded")
	ErrGlobalRetainedQuota   = errors.New("global retained call quota exceeded")
	ErrIdentityByteQuota     = errors.New("identity retained byte quota exceeded")
	ErrGlobalByteQuota       = errors.New("global retained byte quota exceeded")
)

type quotaError struct{ limit error }

func (e *quotaError) Error() string { return ErrQuotaExceeded.Error() }

func (e *quotaError) Is(target error) bool {
	return target == ErrQuotaExceeded || target == e.limit
}

type Record struct {
	Call   Call            `json:"call"`
	Input  json.RawMessage `json:"-"`
	Result json.RawMessage `json:"result,omitempty"`
}

type Event struct {
	CallID   string          `json:"call_id,omitempty"`
	Sequence int             `json:"sequence"`
	Envelope json.RawMessage `json:"envelope"`
}

type EventPage struct {
	Events    []Event `json:"events"`
	NextAfter int     `json:"next_after,omitempty"`
	HasMore   bool    `json:"has_more"`
}

type Repository struct{ db *sql.DB }

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

func validatePayload(data json.RawMessage, limit int) error {
	if len(data) > limit {
		return ErrPayloadTooLarge
	}
	if !json.Valid(data) {
		return errors.New("call payload is not valid JSON")
	}
	return nil
}

type quotaUsage struct {
	globalRetained   int64
	identityRetained int64
	globalActive     int64
	identityActive   int64
	globalBytes      int64
	identityBytes    int64
}

func checkCreateQuota(ctx context.Context, tx *immediateTx, identity string, inputBytes int64) error {
	var usage quotaUsage
	err := tx.QueryRowContext(ctx, `
SELECT count(*),
       coalesce(sum(CASE WHEN c.identity_id=? THEN 1 ELSE 0 END),0),
       coalesce(sum(CASE WHEN c.state NOT IN ('completed','failed','canceled','rejected') THEN 1 ELSE 0 END),0),
       coalesce(sum(CASE WHEN c.identity_id=? AND c.state NOT IN ('completed','failed','canceled','rejected') THEN 1 ELSE 0 END),0),
       coalesce(sum(u.byte_count),0),
       coalesce(sum(CASE WHEN c.identity_id=? THEN u.byte_count ELSE 0 END),0)
FROM calls c
LEFT JOIN call_storage_usage u ON u.call_id=c.id`, identity, identity, identity).Scan(
		&usage.globalRetained,
		&usage.identityRetained,
		&usage.globalActive,
		&usage.identityActive,
		&usage.globalBytes,
		&usage.identityBytes,
	)
	if err != nil {
		return err
	}
	switch {
	case usage.identityActive >= MaxIdentityActiveCalls:
		return &quotaError{limit: ErrIdentityActiveQuota}
	case usage.globalActive >= MaxGlobalActiveCalls:
		return &quotaError{limit: ErrGlobalActiveQuota}
	case usage.identityRetained >= MaxIdentityRetainedCalls:
		return &quotaError{limit: ErrIdentityRetainedQuota}
	case usage.globalRetained >= MaxGlobalRetainedCalls:
		return &quotaError{limit: ErrGlobalRetainedQuota}
	case inputBytes > MaxIdentityRetainedBytes-usage.identityBytes:
		return &quotaError{limit: ErrIdentityByteQuota}
	case inputBytes > MaxGlobalRetainedBytes-usage.globalBytes:
		return &quotaError{limit: ErrGlobalByteQuota}
	default:
		return nil
	}
}

func addRetainedBytes(ctx context.Context, tx *immediateTx, id string, additional int64) error {
	if additional < 0 {
		return errors.New("invalid retained byte increment")
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO call_storage_usage(call_id,byte_count)
SELECT c.id,
       coalesce(length(i.data_json),0) +
       coalesce(length(r.data_json),0) +
       coalesce((SELECT sum(length(e.data_json)) FROM events e WHERE e.call_id=c.id),0)
FROM calls c
LEFT JOIN call_inputs i ON i.call_id=c.id
LEFT JOIN call_results r ON r.call_id=c.id
WHERE c.id=?
ON CONFLICT(call_id) DO NOTHING`, id); err != nil {
		return err
	}
	var identity string
	if err := tx.QueryRowContext(ctx, `SELECT identity_id FROM calls WHERE id=?`, id).Scan(&identity); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var identityBytes, globalBytes int64
	if err := tx.QueryRowContext(ctx, `
SELECT coalesce(sum(CASE WHEN c.identity_id=? THEN u.byte_count ELSE 0 END),0),
       coalesce(sum(u.byte_count),0)
FROM calls c
JOIN call_storage_usage u ON u.call_id=c.id`, identity).Scan(&identityBytes, &globalBytes); err != nil {
		return err
	}
	switch {
	case identityBytes > MaxIdentityRetainedBytes || additional > MaxIdentityRetainedBytes-identityBytes:
		return &quotaError{limit: ErrIdentityByteQuota}
	case globalBytes > MaxGlobalRetainedBytes || additional > MaxGlobalRetainedBytes-globalBytes:
		return &quotaError{limit: ErrGlobalByteQuota}
	}
	result, err := tx.ExecContext(ctx, `UPDATE call_storage_usage SET byte_count=byte_count+? WHERE call_id=?`, additional, id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) Create(ctx context.Context, c Call, input json.RawMessage) (err error) {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := validatePayload(input, MaxPayloadBytes); err != nil {
		return err
	}
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.CreatedAt
	}
	tx, err := beginImmediate(ctx, r.db)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = checkCreateQuota(ctx, tx, c.IdentityID, int64(len(input))); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO calls(id,capability_id,identity_id,state,code,message,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, c.ID, c.CapabilityID, c.IdentityID, c.State, c.Code, c.Message, c.CreatedAt.Format(time.RFC3339Nano), c.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO call_inputs(call_id,data_json) VALUES(?,?)`, c.ID, []byte(input)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO call_event_usage(call_id,event_count,byte_count) VALUES(?,0,0)`, c.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO call_storage_usage(call_id,byte_count) VALUES(?,?)`, c.ID, len(input)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repository) Get(ctx context.Context, id string) (Record, error) {
	var record Record
	var state, created, updated string
	var input []byte
	var result []byte
	err := r.db.QueryRowContext(ctx, `SELECT c.id,c.capability_id,c.identity_id,c.state,c.code,c.message,c.created_at,c.updated_at,i.data_json,r.data_json FROM calls c JOIN call_inputs i ON i.call_id=c.id LEFT JOIN call_results r ON r.call_id=c.id WHERE c.id=?`, id).Scan(
		&record.Call.ID, &record.Call.CapabilityID, &record.Call.IdentityID, &state, &record.Call.Code, &record.Call.Message, &created, &updated, &input, &result,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	record.Call.State = State(state)
	record.Call.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Record{}, err
	}
	record.Call.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return Record{}, err
	}
	record.Input = append(json.RawMessage(nil), input...)
	if result != nil {
		record.Result = append(json.RawMessage(nil), result...)
	}
	return record, nil
}

func (r *Repository) Transition(ctx context.Context, id string, to State, code, message string) (err error) {
	if err := validateErrorMetadata(code, message); err != nil {
		return err
	}
	tx, err := beginImmediate(ctx, r.db)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var from string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM calls WHERE id=?`, id).Scan(&from); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if !CanTransition(State(from), to) {
		return ErrInvalidTransition
	}
	result, err := tx.ExecContext(ctx, `UPDATE calls SET state=?,code=?,message=?,updated_at=? WHERE id=? AND state=?`, to, code, message, time.Now().UTC().Format(time.RFC3339Nano), id, from)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrInvalidTransition
	}
	return tx.Commit(ctx)
}

func (r *Repository) Complete(ctx context.Context, id string, result json.RawMessage) (err error) {
	if err := validatePayload(result, MaxPayloadBytes); err != nil {
		return err
	}
	tx, err := beginImmediate(ctx, r.db)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM calls WHERE id=?`, id).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if State(state) == Completed {
		var existing []byte
		if err = tx.QueryRowContext(ctx, `SELECT data_json FROM call_results WHERE call_id=?`, id).Scan(&existing); err != nil {
			return err
		}
		if !bytes.Equal(existing, result) {
			return ErrInvalidTransition
		}
		return tx.Commit(ctx)
	}
	if !CanTransition(State(state), Completed) {
		return ErrInvalidTransition
	}
	if err = addRetainedBytes(ctx, tx, id, int64(len(result))); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO call_results(call_id,data_json) VALUES(?,?) ON CONFLICT(call_id) DO UPDATE SET data_json=excluded.data_json`, id, []byte(result)); err != nil {
		return err
	}
	updateResult, err := tx.ExecContext(ctx, `UPDATE calls SET state=?,code='',message='',updated_at=? WHERE id=? AND state=?`, Completed, time.Now().UTC().Format(time.RFC3339Nano), id, state)
	if err != nil {
		return err
	}
	changed, err := updateResult.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrInvalidTransition
	}
	return tx.Commit(ctx)
}

func (r *Repository) AppendEvent(ctx context.Context, id string, envelope json.RawMessage) (event Event, err error) {
	if err := validatePayload(envelope, MaxEventEnvelopeBytes); err != nil {
		return Event{}, err
	}
	tx, err := beginImmediate(ctx, r.db)
	if err != nil {
		return Event{}, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM calls WHERE id=?`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	} else if err != nil {
		return Event{}, err
	}
	var usageExists int
	backfilledUsage := false
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM call_event_usage WHERE call_id=?`, id).Scan(&usageExists); errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO call_event_usage(call_id,event_count,byte_count) SELECT ?,count(*),coalesce(sum(length(data_json)),0) FROM events WHERE call_id=? ON CONFLICT(call_id) DO NOTHING`, id, id); err != nil {
			return Event{}, err
		}
		backfilledUsage = true
	} else if err != nil {
		return Event{}, err
	}
	envelopeBytes := int64(len(envelope))
	usageResult, err := tx.ExecContext(ctx, `UPDATE call_event_usage SET event_count=event_count+1,byte_count=byte_count+? WHERE call_id=? AND event_count>=0 AND event_count<? AND byte_count>=0 AND byte_count<=?`, envelopeBytes, id, MaxEvents, MaxEventAggregateBytes-envelopeBytes)
	if err != nil {
		return Event{}, err
	}
	changed, err := usageResult.RowsAffected()
	if err != nil {
		return Event{}, err
	}
	if changed != 1 {
		var count, byteCount int64
		if err = tx.QueryRowContext(ctx, `SELECT event_count,byte_count FROM call_event_usage WHERE call_id=?`, id).Scan(&count, &byteCount); err != nil {
			return Event{}, err
		}
		limitErr := ErrEventBudgetExceeded
		if count >= MaxEvents {
			limitErr = ErrTooManyEvents
		}
		if backfilledUsage {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return Event{}, commitErr
			}
		}
		return Event{}, limitErr
	}
	var maxSequence int64
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM events WHERE call_id=?`, id).Scan(&maxSequence); err != nil {
		return Event{}, err
	}
	if maxSequence < 0 || maxSequence >= int64(^uint(0)>>1) {
		return Event{}, ErrTooManyEvents
	}
	event = Event{CallID: id, Sequence: int(maxSequence + 1), Envelope: append(json.RawMessage(nil), envelope...)}
	if err = addRetainedBytes(ctx, tx, id, envelopeBytes); err != nil {
		return Event{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(call_id,sequence,data_json) VALUES(?,?,?)`, id, event.Sequence, []byte(envelope)); err != nil {
		return Event{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (r *Repository) ListEvents(ctx context.Context, id string, limit int) ([]Event, error) {
	page, err := r.ListEventPage(ctx, id, 0, limit)
	return page.Events, err
}

func (r *Repository) ListEventPage(ctx context.Context, id string, after, limit int) (EventPage, error) {
	var exists int
	if err := r.db.QueryRowContext(ctx, `SELECT 1 FROM calls WHERE id=?`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return EventPage{}, ErrNotFound
	} else if err != nil {
		return EventPage{}, err
	}
	if after < 0 {
		return EventPage{}, errors.New("invalid event cursor")
	}
	if limit <= 0 || limit > MaxEventPageSize {
		limit = MaxEventPageSize
	}
	rows, err := r.db.QueryContext(ctx, `SELECT sequence,data_json FROM events WHERE call_id=? AND sequence>? ORDER BY sequence LIMIT ?`, id, after, limit+1)
	if err != nil {
		return EventPage{}, err
	}
	defer rows.Close()
	page := EventPage{Events: make([]Event, 0, limit), NextAfter: after}
	var pageBytes int64
	for rows.Next() {
		var event Event
		var envelope []byte
		if err := rows.Scan(&event.Sequence, &envelope); err != nil {
			return EventPage{}, err
		}
		if len(page.Events) >= limit {
			page.HasMore = true
			break
		}
		envelopeBytes := int64(len(envelope))
		if envelopeBytes > MaxEventPageBytes-pageBytes {
			if len(page.Events) == 0 {
				return EventPage{}, ErrPayloadTooLarge
			}
			page.HasMore = true
			break
		}
		event.CallID = id
		event.Envelope = append(json.RawMessage(nil), envelope...)
		page.Events = append(page.Events, event)
		page.NextAfter = event.Sequence
		pageBytes += envelopeBytes
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, err
	}
	return page, nil
}

func (r *Repository) RecoverInterrupted(ctx context.Context, code, message string) (int64, error) {
	if err := validateErrorMetadata(code, message); err != nil {
		return 0, err
	}
	result, err := r.db.ExecContext(ctx, `UPDATE calls SET state=?,code=?,message=?,updated_at=? WHERE state IN (?,?,?,?,?)`, Failed, code, message, time.Now().UTC().Format(time.RFC3339Nano), Created, Submitted, Working, InputRequired, AuthRequired)
	if err != nil {
		return 0, fmt.Errorf("recover interrupted calls: %w", err)
	}
	return result.RowsAffected()
}
