package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	adapterreg "github.com/gopact-ai/9a/internal/adapter"
	"github.com/gopact-ai/9a/internal/app"
	callmodel "github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/search"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type Request struct {
	Action      string          `json:"action"`
	Protocol    string          `json:"protocol,omitempty"`
	Executable  string          `json:"executable,omitempty"`
	CallID      string          `json:"call_id,omitempty"`
	After       int             `json:"after,omitempty"`
	Limit       int             `json:"limit,omitempty"`
	Name        string          `json:"name,omitempty"`
	Endpoint    string          `json:"endpoint,omitempty"`
	Identity    string          `json:"identity,omitempty"`
	Capability  string          `json:"capability,omitempty"`
	Permissions []string        `json:"permissions,omitempty"`
	Query       string          `json:"query,omitempty"`
	Format      string          `json:"format,omitempty"`
	Root        string          `json:"root,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
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
		r.Body = http.MaxBytesReader(w, r.Body, callmodel.MaxPayloadBytes+(64<<10))
		var q Request
		if json.NewDecoder(r.Body).Decode(&q) != nil {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(Response{Error: "invalid_request"})
			return
		}
		ctx := r.Context()
		var data any
		var err error
		switch q.Action {
		case "adapter.add":
			if !a.IsAdmin(ctx, identity) {
				writeError(w, http.StatusForbidden, "permission_denied", errors.New("admin permission required"))
				return
			}
			err = a.AddAdapter(ctx, q.Protocol, q.Executable)
		case "provider.add":
			if !a.IsAdmin(ctx, identity) {
				writeError(w, http.StatusForbidden, "permission_denied", errors.New("admin permission required"))
				return
			}
			p := provider.Provider{ID: q.Protocol + "/" + q.Name, Protocol: q.Protocol, Name: q.Name, Endpoint: q.Endpoint}
			err = a.AddProvider(ctx, p)
		case "acl.grant":
			if !a.IsAdmin(ctx, identity) {
				writeError(w, http.StatusForbidden, "permission_denied", errors.New("admin permission required"))
				return
			}
			err = a.Grant(ctx, q.Identity, q.Capability, q.Permissions)
		case "token.create":
			if !a.IsAdmin(ctx, identity) {
				writeError(w, http.StatusForbidden, "permission_denied", errors.New("admin permission required"))
				return
			}
			data, err = a.CreateToken(ctx, q.Identity)
		case "search":
			data, err = a.Search(ctx, identity, search.Query{Text: q.Query, Limit: 20})
		case "project.add":
			err = a.Project(ctx, identity, q.Capability, q.Root)
		case "invoke":
			data, err = a.Invoke(ctx, identity, q.Capability, q.Input)
		case "call.start":
			data, err = a.StartCall(ctx, identity, q.Capability, q.Input)
		case "call.get":
			data, err = a.GetCall(ctx, identity, q.CallID)
		case "call.events":
			data, err = a.ListCallEventPage(ctx, identity, q.CallID, q.After, q.Limit)
		case "call.cancel":
			err = a.CancelCall(ctx, identity, q.CallID)
		default:
			err = &actionError{}
		}
		if err != nil {
			if q.Action == "adapter.add" {
				if errors.Is(err, adapterreg.ErrDuplicate) {
					writeError(w, http.StatusBadRequest, "adapter_exists", errors.New("adapter already registered"))
					return
				}
				if errors.Is(err, adapterreg.ErrInvalid) {
					writeError(w, http.StatusBadRequest, "invalid_adapter", errors.New("invalid adapter registration"))
					return
				}
				writeError(w, http.StatusBadRequest, "adapter_failed", errors.New("adapter registration failed"))
				return
			}
			if strings.HasPrefix(q.Action, "call.") {
				writeCallError(w, err)
				return
			}
			writeError(w, http.StatusBadRequest, "request_failed", err)
			return
		}
		_ = json.NewEncoder(w).Encode(Response{Data: data})
	})
	s := &Server{http: &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}, ln: ln}
	go s.http.Serve(ln)
	return s, nil
}

func writeCallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, callmodel.ErrQuotaExceeded):
		writeError(w, http.StatusBadRequest, "call_quota_exceeded", errors.New("call quota exceeded"))
	case errors.Is(err, app.ErrCallNotFound):
		writeError(w, http.StatusBadRequest, "call_not_found", errors.New("call not found"))
	case errors.Is(err, app.ErrCallNotCancelable):
		writeError(w, http.StatusBadRequest, "call_not_cancelable", errors.New("call is not cancelable"))
	case errors.Is(err, app.ErrCallNotActive):
		writeError(w, http.StatusBadRequest, "call_not_active", errors.New("call is not active"))
	default:
		writeError(w, http.StatusBadRequest, "request_failed", errors.New("call request failed"))
	}
}

func writeError(w http.ResponseWriter, status int, code string, err error) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Response{Error: err.Error(), Code: code})
}

type actionError struct{}

func (*actionError) Error() string                { return "unknown_action" }
func (s *Server) Close(ctx context.Context) error { return s.http.Shutdown(ctx) }
