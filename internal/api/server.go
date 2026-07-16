package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gopact-ai/9a/internal/app"
	"github.com/gopact-ai/9a/internal/declarative"
	"github.com/gopact-ai/9a/internal/jsoncontract"
	"github.com/gopact-ai/9a/internal/search"
)

type Request struct {
	Action     string          `json:"action"`
	Name       string          `json:"name,omitempty"`
	Capability string          `json:"capability,omitempty"`
	Query      string          `json:"query,omitempty"`
	Root       string          `json:"root,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Source     string          `json:"source,omitempty"`
	Approval   string          `json:"approval,omitempty"`
	Fix        bool            `json:"fix,omitempty"`
	Value      string          `json:"value,omitempty"`
}
type Response struct {
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}
type Server struct {
	http *http.Server
	ln   net.Listener
}

func Listen(socket string, a *app.App) (*Server, error) {
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("socket path exists and is not a socket: %s", socket)
		}
		conn, dialErr := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("socket already in use: %s", socket)
		}
		if err := os.Remove(socket); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	ln, e := net.Listen("unix", socket)
	if e != nil {
		return nil, e
	}
	if err := os.Chmod(socket, 0600); err != nil {
		_ = ln.Close()
		_ = os.Remove(socket)
		return nil, fmt.Errorf("secure socket permissions: %w", err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		identity, authErr := a.Authenticate(r.Context(), got)
		if authErr != nil {
			w.WriteHeader(401)
			_ = json.NewEncoder(w).Encode(Response{Error: "authentication failed", Code: "unauthorized"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, declarative.MaxSourceBytes*2+(64<<10))
		q, decodeErr := decodeRequest(r.Body)
		if decodeErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", errors.New("invalid request body"))
			return
		}
		ctx := r.Context()
		var data any
		var err error
		switch q.Action {
		case "status":
			data, err = a.Status(ctx, q.Root, q.Name)
		case "doctor":
			data, err = a.Doctor(ctx, identity, q.Root, q.Fix)
		case "secret.set":
			if !a.IsAdmin(ctx, identity) {
				writeError(w, http.StatusForbidden, "permission_denied", errors.New("admin permission required"))
				return
			}
			err = a.SetSecret(ctx, identity, q.Root, q.Name, q.Value)
		case "secret.list":
			data, err = a.ListSecrets(ctx, q.Root, q.Name)
		case "secret.unset":
			if !a.IsAdmin(ctx, identity) {
				writeError(w, http.StatusForbidden, "permission_denied", errors.New("admin permission required"))
				return
			}
			err = a.DeleteSecret(ctx, identity, q.Root, q.Name)
		case "connect":
			if !a.IsAdmin(ctx, identity) {
				writeError(w, http.StatusForbidden, "permission_denied", errors.New("admin permission required"))
				return
			}
			data, err = a.Connect(ctx, identity, []byte(q.Source), q.Root)
		case "disconnect":
			if !a.IsAdmin(ctx, identity) {
				writeError(w, http.StatusForbidden, "permission_denied", errors.New("admin permission required"))
				return
			}
			err = a.DisconnectFromWorkspace(ctx, identity, q.Root, q.Name)
		case "search":
			data, err = a.Search(ctx, identity, q.Root, search.Query{Text: q.Query, Limit: 20})
		case "run":
			runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
			data, err = a.RunInWorkspace(runCtx, identity, q.Root, q.Capability, q.Input, q.Approval)
			cancel()
		default:
			err = &actionError{}
		}
		if err != nil {
			if q.Action == "run" {
				writeRunError(w, err)
				return
			}
			writeError(w, http.StatusBadRequest, "request_failed", err)
			return
		}
		_ = json.NewEncoder(w).Encode(Response{Data: data})
	})
	s := &Server{http: &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}, ln: ln}
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = ln.Close()
		}
	}()
	return s, nil
}

func decodeRequest(body io.Reader) (Request, error) {
	var request Request
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return Request{}, err
	}
	if request.Action == "" {
		return Request{}, errors.New("action is required")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Request{}, errors.New("request must contain exactly one JSON value")
		}
		return Request{}, err
	}
	return request, nil
}

func writeRunError(w http.ResponseWriter, err error) {
	var approval *app.ApprovalRequiredError
	if errors.As(err, &approval) {
		integration, capabilityName, _ := strings.Cut(approval.Capability, "/")
		writeErrorData(w, http.StatusConflict, "approval_required", err, map[string]any{
			"integration":   integration,
			"capability":    capabilityName,
			"approvalToken": approval.Token,
			"sideEffect":    "none",
			"retryable":     true,
			"nextAction": map[string]string{
				"instruction": "Obtain explicit approval, then retry the exact same input with --approve " + approval.Token,
			},
		})
		return
	}
	var mismatch *app.ApprovalMismatchError
	if errors.As(err, &mismatch) {
		writeErrorData(w, http.StatusConflict, "approval_mismatch", err, map[string]any{
			"sideEffect": "none",
			"retryable":  true,
			"nextAction": map[string]string{
				"instruction": "Run the command without --approve to obtain a token for the current capability and input",
			},
		})
		return
	}
	var changed *app.CapabilityChangedError
	if errors.As(err, &changed) {
		writeErrorData(w, http.StatusConflict, "capability_changed", err, map[string]any{
			"sideEffect": "none",
			"retryable":  true,
			"nextAction": map[string]string{
				"command": "9a search " + changed.Capability + " --json",
			},
		})
		return
	}
	if errors.Is(err, jsoncontract.ErrInvalidValue) {
		writeErrorData(w, http.StatusBadRequest, "invalid_input", err, map[string]any{"sideEffect": "none", "retryable": false})
		return
	}
	var runErr *app.RunError
	if !errors.As(err, &runErr) || runErr == nil {
		writeErrorData(w, http.StatusBadRequest, "request_failed", err, map[string]any{"sideEffect": "none", "retryable": false})
		return
	}
	if runErr.Code == "missing_credential" && runErr.Credential != "" {
		integration, credentialName, _ := strings.Cut(runErr.Credential, ".")
		data := map[string]any{
			"integration": integration,
			"credential":  credentialName,
			"sideEffect":  runErr.SideEffect,
			"retryable":   false,
			"nextAction": map[string]string{
				"command": "9a secret set " + runErr.Credential,
			},
		}
		if runErr.CallID != "" {
			data["call_id"] = runErr.CallID
		}
		writeErrorData(w, http.StatusConflict, runErr.Code, err, data)
		return
	}
	code := runErr.Code
	if code == "" {
		code = "run_failed"
	}
	sideEffect := runErr.SideEffect
	if sideEffect == "" {
		sideEffect = "possible"
	}
	data := map[string]any{
		"sideEffect": sideEffect,
		"retryable":  false,
	}
	if runErr.CallID != "" {
		data["call_id"] = runErr.CallID
	}
	writeErrorData(w, http.StatusBadRequest, code, err, data)
}

func writeErrorData(w http.ResponseWriter, status int, code string, err error, data any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Response{Data: data, Error: err.Error(), Code: code})
}

func writeError(w http.ResponseWriter, status int, code string, err error) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Response{Error: err.Error(), Code: code})
}

type actionError struct{}

func (*actionError) Error() string                { return "unknown_action" }
func (s *Server) Close(ctx context.Context) error { return s.http.Shutdown(ctx) }
