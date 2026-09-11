package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// search.go — §410: global search across projects/tasks/docs/files.
// One UNION through each table's RLS — the DB enforces tenancy +
// visibility, the handler cannot leak (ADR-0002). Trigram similarity
// for typo tolerance, recency as tiebreak.

type searchHit struct {
	Type      string  `json:"type"` // project | task | doc | file
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	ProjectID string  `json:"project_id,omitempty"`
	Score     float64 `json:"score"`
}

// staffSearchSQL: one query per table (RLS-filtered), similarity-ranked.
// Written as a UNION ALL of four lateral selects; pgx runs it as one
// round trip. deleted_at IS NULL everywhere.
const staffSearchSQL = `
SELECT * FROM (
	SELECT 'project' AS type, p.id::text, p.name AS title, '' AS project_id,
	       similarity(p.name, $1) AS score, p.updated_at AS ts
	FROM projects p
	WHERE p.deleted_at IS NULL AND (p.name % $1 OR p.name ILIKE '%' || $1 || '%')
	UNION ALL
	SELECT 'task', t.id::text, t.title, t.project_id::text,
	       similarity(t.title, $1), t.updated_at
	FROM tasks t
	WHERE t.deleted_at IS NULL AND (t.title % $1 OR t.title ILIKE '%' || $1 || '%')
	UNION ALL
	SELECT 'doc', d.id::text, d.title, d.project_id::text,
	       similarity(d.title, $1), d.updated_at
	FROM docs d
	WHERE d.deleted_at IS NULL AND (d.title % $1 OR d.title ILIKE '%' || $1 || '%')
	UNION ALL
	SELECT 'file', f.id::text, f.name, f.project_id::text,
	       similarity(f.name, $1), f.updated_at
	FROM files f
	WHERE f.deleted_at IS NULL AND (f.name % $1 OR f.name ILIKE '%' || $1 || '%')
) hits
ORDER BY score DESC, ts DESC
LIMIT 60`

func (s *Server) staffSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len(q) > 200 {
		problem(w, http.StatusBadRequest, "q required (1-200 chars)")
		return
	}
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	hits, err := scanSearch(ctx, tx, staffSearchSQL, q)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

// portalSearchSQL: the customer sees ONLY what staff shared — linked
// projects, customer_visible tasks/docs/files (§207 never leak
// internal). Same UNION shape; every branch carries its portal policy.
const portalSearchSQL = `
SELECT * FROM (
	SELECT 'project' AS type, p.id::text, p.name AS title, '' AS project_id,
	       similarity(p.name, $1) AS score, p.updated_at AS ts
	FROM projects p
	WHERE p.deleted_at IS NULL AND (p.name % $1 OR p.name ILIKE '%' || $1 || '%')
	UNION ALL
	SELECT 'task', t.id::text, t.title, t.project_id::text,
	       similarity(t.title, $1), t.updated_at
	FROM tasks t
	WHERE t.deleted_at IS NULL AND t.customer_visible AND (t.title % $1 OR t.title ILIKE '%' || $1 || '%')
	UNION ALL
	SELECT 'doc', d.id::text, d.title, d.project_id::text,
	       similarity(d.title, $1), d.updated_at
	FROM docs d
	WHERE d.deleted_at IS NULL AND d.customer_visible AND (d.title % $1 OR d.title ILIKE '%' || $1 || '%')
	UNION ALL
	SELECT 'file', f.id::text, f.name, f.project_id::text,
	       similarity(f.name, $1), f.updated_at
	FROM files f
	WHERE f.deleted_at IS NULL AND f.customer_visible AND (f.name % $1 OR f.name ILIKE '%' || $1 || '%')
) hits
ORDER BY score DESC, ts DESC
LIMIT 60`

func (s *Server) portalSearch(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, r *http.Request) (int, error) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len(q) > 200 {
		problem(w, http.StatusBadRequest, "q required (1-200 chars)")
		return 0, nil
	}
	hits, err := scanSearch(ctx, tx, portalSearchSQL, q)
	if err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	writeJSON(w, http.StatusOK, hits)
	return 0, nil
}

func scanSearch(ctx context.Context, tx pgx.Tx, sql, q string) ([]searchHit, error) {
	rows, err := tx.Query(ctx, sql, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hits := []searchHit{}
	for rows.Next() {
		var h searchHit
		var ts time.Time
		if err := rows.Scan(&h.Type, &h.ID, &h.Title, &h.ProjectID, &h.Score, &ts); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, nil
}
