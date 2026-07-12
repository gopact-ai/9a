package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gopact-ai/9a/internal/authz"
	"github.com/gopact-ai/9a/internal/capability"
)

type Query struct {
	Text, Protocol, Provider, Kind string
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
	if strings.Contains(q.Text, "/") {
		var data []byte
		err := s.db.QueryRowContext(ctx, `SELECT c.data_json FROM capabilities c JOIN acl a ON a.capability_id=c.id WHERE c.id=? AND a.identity_id=? AND a.permission='read'`, q.Text, identity).Scan(&data)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var c capability.Capability
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, err
		}
		if !matches(c, q) {
			return nil, nil
		}
		return []Result{{Capability: c, Score: 1, Reason: "exact_id"}}, nil
	}
	query := ftsQuery(q.Text)
	var rows *sql.Rows
	var err error
	if query == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT c.data_json,0.0 FROM capabilities c JOIN acl a ON a.capability_id=c.id WHERE a.identity_id=? AND a.permission='read' ORDER BY c.id LIMIT ?`, identity, q.Limit*4)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT c.data_json,bm25(capability_fts) FROM capability_fts JOIN capabilities c ON c.id=capability_fts.id JOIN acl a ON a.capability_id=c.id WHERE capability_fts MATCH ? AND a.identity_id=? AND a.permission='read' ORDER BY bm25(capability_fts),c.id LIMIT ?`, query, identity, q.Limit*4)
	}
	if err != nil {
		return nil, fmt.Errorf("search catalog: %w", err)
	}
	defer rows.Close()
	out := make([]Result, 0, q.Limit)
	for rows.Next() {
		var data []byte
		var rank float64
		if err := rows.Scan(&data, &rank); err != nil {
			return nil, err
		}
		var c capability.Capability
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, err
		}
		if !matches(c, q) {
			continue
		}
		out = append(out, Result{Capability: c, Score: -rank, Reason: "full_text"})
		if len(out) == q.Limit {
			break
		}
	}
	return out, rows.Err()
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
