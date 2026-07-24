// Package search finds capabilities a caller may read within a workspace,
// resolving exact and integration references and otherwise ranking results
// with SQLite full-text search, filtered by ACL grants and workspace root.
package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/9a/internal/authz"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/jsonvalue"
)

type Query struct {
	Text, Protocol, Provider, Kind string
	WorkspaceRoot                  string
	Limit                          int
}
type Result struct {
	Capability capability.Capability `json:"capability"`
	Score      float64               `json:"score"`
	Reason     string                `json:"reason"`
}
type Service struct {
	db    *sql.DB
	authz *authz.Service
}

func New(db *sql.DB, az *authz.Service) *Service { return &Service{db: db, authz: az} }

func (s *Service) Search(ctx context.Context, identity string, q Query) ([]Result, error) {
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 20
	}
	if integration, name, ok := shortReference(q.Text); ok {
		rows, err := s.db.QueryContext(ctx, `SELECT c.data_json,p.config_json FROM capabilities c JOIN providers p ON p.id=c.provider_id JOIN acl a ON a.capability_id=c.id WHERE c.provider_name=? AND a.identity_id=? AND a.permission='read' ORDER BY c.id`, integration, identity)
		if err != nil {
			return nil, err
		}
		var out []Result
		for rows.Next() {
			c, workspaceConfig, scanErr := scanCandidate(rows)
			if scanErr != nil {
				return nil, scanErr
			}
			if capability.Slug(c.Source.UpstreamName) != name || !matches(c, q) || !matchesWorkspace(workspaceConfig, q.WorkspaceRoot) {
				continue
			}
			out = append(out, Result{Capability: c, Score: 1, Reason: "exact_ref"})
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if len(out) > 1 {
			return nil, fmt.Errorf("capability reference %q is ambiguous", q.Text)
		}
		return out, nil
	}
	if integration, ok := integrationReference(q.Text); ok {
		out, found, err := s.searchIntegration(ctx, identity, integration, q)
		if err != nil {
			return nil, err
		}
		if found {
			return out, nil
		}
	}
	query := ftsQuery(q.Text)
	var rows *sql.Rows
	var err error
	if query == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT c.data_json,p.config_json,0.0 FROM capabilities c JOIN providers p ON p.id=c.provider_id JOIN acl a ON a.capability_id=c.id WHERE a.identity_id=? AND a.permission='read' ORDER BY c.id`, identity)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT c.data_json,p.config_json,bm25(capability_fts) FROM capability_fts JOIN capabilities c ON c.id=capability_fts.id JOIN providers p ON p.id=c.provider_id JOIN acl a ON a.capability_id=c.id WHERE capability_fts MATCH ? AND a.identity_id=? AND a.permission='read' ORDER BY bm25(capability_fts),c.id`, query, identity)
	}
	if err != nil {
		return nil, fmt.Errorf("search catalog: %w", err)
	}
	out := make([]Result, 0, q.Limit)
	for rows.Next() {
		var data, workspaceConfig []byte
		var rank float64
		if err := rows.Scan(&data, &workspaceConfig, &rank); err != nil {
			return nil, err
		}
		var c capability.Capability
		if err := jsonvalue.Decode(data, &c); err != nil {
			return nil, err
		}
		if !matches(c, q) || !matchesWorkspace(workspaceConfig, q.WorkspaceRoot) {
			continue
		}
		out = append(out, Result{Capability: c, Score: -rank, Reason: "full_text"})
		if len(out) == q.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) searchIntegration(ctx context.Context, identity, integration string, q Query) ([]Result, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.data_json,p.config_json FROM capabilities c JOIN providers p ON p.id=c.provider_id JOIN acl a ON a.capability_id=c.id WHERE c.provider_name=? AND a.identity_id=? AND a.permission='read' ORDER BY c.id`, integration, identity)
	if err != nil {
		return nil, false, err
	}
	var out []Result
	found := false
	for rows.Next() {
		c, workspaceConfig, scanErr := scanCandidate(rows)
		if scanErr != nil {
			return nil, false, scanErr
		}
		if !matchesWorkspace(workspaceConfig, q.WorkspaceRoot) {
			continue
		}
		found = true
		if !matches(c, q) {
			continue
		}
		out = append(out, Result{Capability: c, Score: 1, Reason: "integration_ref"})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	return out, found, nil
}

func shortReference(text string) (string, string, bool) {
	if strings.TrimSpace(text) != text || len(strings.Fields(text)) != 1 {
		return "", "", false
	}
	parts := strings.Split(text, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || capability.Slug(parts[0]) != parts[0] || capability.Slug(parts[1]) != parts[1] {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func integrationReference(text string) (string, bool) {
	if text == "" || strings.TrimSpace(text) != text || len(strings.Fields(text)) != 1 || strings.Contains(text, "/") || capability.Slug(text) != text {
		return "", false
	}
	return text, true
}

func scanCandidate(row interface{ Scan(...any) error }) (capability.Capability, []byte, error) {
	var data, workspaceConfig []byte
	if err := row.Scan(&data, &workspaceConfig); err != nil {
		return capability.Capability{}, nil, err
	}
	var c capability.Capability
	if err := jsonvalue.Decode(data, &c); err != nil {
		return capability.Capability{}, nil, err
	}
	return c, workspaceConfig, nil
}

func matchesWorkspace(raw []byte, root string) bool {
	if root == "" {
		return true
	}
	var config map[string]string
	if json.Unmarshal(raw, &config) != nil {
		return false
	}
	stored := config["workspace_root"]
	return filepath.IsAbs(stored) && filepath.Clean(stored) == filepath.Clean(root)
}

func matches(c capability.Capability, q Query) bool {
	return (q.Protocol == "" || c.Source.Protocol == q.Protocol) && (q.Provider == "" || c.Source.Provider == q.Provider) && (q.Kind == "" || c.Kind == q.Kind)
}
func ftsQuery(text string) string {
	parts := strings.Fields(text)
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, " AND ")
}
