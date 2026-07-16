package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gopact-ai/9a/internal/authz"
	"github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/jsoncontract"
	"github.com/gopact-ai/9a/internal/secret"
)

type RunError struct {
	CallID     string
	Code       string
	Message    string
	Credential string
	SideEffect string
	err        error
}

func (e *RunError) Error() string {
	code := e.Code
	if code == "" {
		code = "run_failed"
	}
	message := e.Message
	if message == "" {
		message = "call did not complete"
	}
	if e.CallID == "" {
		return fmt.Sprintf("run failed (%s): %s", code, message)
	}
	return fmt.Sprintf("call %s failed (%s): %s", e.CallID, code, message)
}

func (e *RunError) Unwrap() error { return e.err }

type ApprovalRequiredError struct {
	Capability string
	Token      string
}

func (e *ApprovalRequiredError) Error() string {
	return fmt.Sprintf("approval required to run %s", e.Capability)
}

type CapabilityChangedError struct {
	Capability string
}

func (e *CapabilityChangedError) Error() string {
	return fmt.Sprintf("capability %s changed before the run started", e.Capability)
}

type ApprovalMismatchError struct {
	Capability string
}

func (e *ApprovalMismatchError) Error() string {
	return fmt.Sprintf("approval does not match the current %s capability and input", e.Capability)
}

const (
	approvalTTL         = 10 * time.Minute
	maxPendingApprovals = 1024
)

type approvalChallenge struct {
	identity     string
	capabilityID string
	revision     int64
	inputDigest  [sha256.Size]byte
	expiresAt    time.Time
}

func (a *App) issueApproval(identity string, resolved capability.Capability, input json.RawMessage) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create approval challenge: %w", err)
	}
	token := fmt.Sprintf("v1.%d.%s", resolved.Revision, hex.EncodeToString(random[:]))
	challenge := approvalChallenge{
		identity: identity, capabilityID: resolved.ID, revision: resolved.Revision,
		inputDigest: sha256.Sum256(input), expiresAt: time.Now().Add(approvalTTL),
	}

	a.approvalMu.Lock()
	defer a.approvalMu.Unlock()
	now := time.Now()
	kept := a.approvalOrder[:0]
	for _, pendingToken := range a.approvalOrder {
		pending, ok := a.approvals[pendingToken]
		if !ok || !pending.expiresAt.After(now) || pending.identity == challenge.identity && pending.capabilityID == challenge.capabilityID && pending.revision == challenge.revision && pending.inputDigest == challenge.inputDigest {
			delete(a.approvals, pendingToken)
			continue
		}
		kept = append(kept, pendingToken)
	}
	a.approvalOrder = kept
	if len(a.approvals) >= maxPendingApprovals && len(a.approvalOrder) > 0 {
		oldest := a.approvalOrder[0]
		delete(a.approvals, oldest)
		a.approvalOrder = a.approvalOrder[1:]
	}
	a.approvals[token] = challenge
	a.approvalOrder = append(a.approvalOrder, token)
	return token, nil
}

func (a *App) consumeApproval(identity, token string, resolved capability.Capability, input json.RawMessage) error {
	if _, ok := approvalTokenRevision(token); !ok {
		return &ApprovalMismatchError{Capability: publicCapabilityRef(resolved)}
	}
	a.approvalMu.Lock()
	challenge, ok := a.approvals[token]
	delete(a.approvals, token)
	for index, pendingToken := range a.approvalOrder {
		if pendingToken == token {
			a.approvalOrder = append(a.approvalOrder[:index], a.approvalOrder[index+1:]...)
			break
		}
	}
	a.approvalMu.Unlock()
	if !ok || !challenge.expiresAt.After(time.Now()) {
		return &ApprovalMismatchError{Capability: publicCapabilityRef(resolved)}
	}
	if challenge.revision != resolved.Revision {
		return &CapabilityChangedError{Capability: publicCapabilityRef(resolved)}
	}
	if challenge.identity != identity || challenge.capabilityID != resolved.ID || challenge.inputDigest != sha256.Sum256(input) {
		return &ApprovalMismatchError{Capability: publicCapabilityRef(resolved)}
	}
	return nil
}

func approvalTokenRevision(token string) (int64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" || len(parts[2]) != 32 {
		return 0, false
	}
	for _, r := range parts[2] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return 0, false
		}
	}
	revision, err := strconv.ParseInt(parts[1], 10, 64)
	return revision, err == nil && revision > 0 && parts[1] == strconv.FormatInt(revision, 10)
}

func (a *App) RunInWorkspace(ctx context.Context, identity, root, ref string, input json.RawMessage, approval string) (json.RawMessage, error) {
	canonical, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	resolved, err := a.cat.ResolveWorkspaceCapability(ctx, canonical, ref)
	if err != nil {
		return nil, err
	}
	return a.runResolved(ctx, identity, ref, input, approval, resolved)
}

func (a *App) runResolved(ctx context.Context, identity, ref string, input json.RawMessage, approval string, resolved capability.Capability) (json.RawMessage, error) {
	if len(input) > call.MaxPayloadBytes {
		return nil, call.ErrPayloadTooLarge
	}
	if !json.Valid(input) {
		return nil, errors.New("call input is not valid JSON")
	}
	if !a.az.Allowed(ctx, identity, resolved.ID, authz.Invoke) {
		return nil, errors.New("permission_denied")
	}
	if err := jsoncontract.Validate(resolved.Input.JSONSchema, input); err != nil {
		return nil, fmt.Errorf("validate capability input: %w", err)
	}
	if approval != "" {
		if err := a.consumeApproval(identity, approval, resolved, input); err != nil {
			var mismatch *ApprovalMismatchError
			if errors.As(err, &mismatch) {
				mismatch.Capability = ref
			}
			var changed *CapabilityChangedError
			if errors.As(err, &changed) {
				changed.Capability = ref
			}
			return nil, err
		}
	}
	if resolved.Security.RequiresApproval == "always" && approval == "" {
		token, err := a.issueApproval(identity, resolved, input)
		if err != nil {
			return nil, err
		}
		return nil, &ApprovalRequiredError{Capability: ref, Token: token}
	}
	if err := a.preflightA2ACredential(ctx, resolved); err != nil {
		return nil, err
	}
	id, err := a.startCallAtRevision(ctx, identity, resolved.ID, input, &resolved.Revision)
	if err != nil {
		if errors.Is(err, ErrCapabilityChanged) {
			return nil, &CapabilityChangedError{Capability: ref}
		}
		return nil, err
	}

	a.mu.Lock()
	runtime := a.activeCalls[id]
	a.mu.Unlock()
	if runtime != nil {
		select {
		case <-runtime.done:
		case <-ctx.Done():
			return nil, &RunError{CallID: id, Code: "wait_canceled", Message: "stopped waiting for call completion", SideEffect: "possible", err: ctx.Err()}
		}
	}
	record, err := a.getCall(context.WithoutCancel(ctx), identity, id)
	if err != nil {
		return nil, &RunError{CallID: id, Code: "internal_error", Message: "call state unavailable", SideEffect: "possible", err: err}
	}
	if record.Call.State == call.Completed {
		return record.Result, nil
	}
	code := record.Call.Code
	if code == "" {
		code = string(record.Call.State)
	}
	message := record.Call.Message
	if message == "" {
		message = "call ended in " + string(record.Call.State)
	}
	credential := ""
	if code == "missing_credential" {
		const prefix, suffix = "credential ", " is missing"
		if strings.HasPrefix(message, prefix) && strings.HasSuffix(message, suffix) {
			credential = strings.TrimSuffix(strings.TrimPrefix(message, prefix), suffix)
		}
	}
	sideEffect := "possible"
	if code == "missing_credential" && resolved.Kind != "workflow" {
		sideEffect = "none"
	}
	return nil, &RunError{CallID: id, Code: code, Message: message, Credential: credential, SideEffect: sideEffect}
}

func (a *App) preflightA2ACredential(ctx context.Context, resolved capability.Capability) error {
	if resolved.Source.Protocol != "a2a" || resolved.Security.UpstreamAuth != "secret" {
		return nil
	}
	a.mu.RLock()
	p, ok := a.providers[capabilityProviderID(resolved)]
	a.mu.RUnlock()
	if !ok {
		return nil // The normal call path reports a concurrently removed provider.
	}
	reference := p.Config["credential_reference"]
	if reference == "" {
		return &RunError{Code: "credential_unavailable", Message: "integration credential declaration is missing", SideEffect: "none"}
	}
	if _, err := a.secrets.Resolve(secret.WithWorkspace(ctx, p.Config["workspace_root"]), reference); err != nil {
		if errors.Is(err, secret.ErrMissing) {
			return &RunError{Code: "missing_credential", Message: "credential " + reference + " is missing", Credential: reference, SideEffect: "none", err: err}
		}
		return &RunError{Code: "credential_unavailable", Message: "system credential store is unavailable", SideEffect: "none", err: err}
	}
	return nil
}
