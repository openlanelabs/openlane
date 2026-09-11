package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// chat.go — §217/§274 chat-lite: per-task threads, staff (user) and
// customers (portal contact) both post. Mentions are extracted
// server-side and stored on the row (email push = P1 SMTP). ?after=
// polling until the WS hub lands (§449).

type messageOut struct {
	ID         string          `json:"id"`
	TaskID     string          `json:"task_id"`
	AuthorType string          `json:"author_type"`
	AuthorID   string          `json:"author_id,omitempty"`
	AuthorName string          `json:"author_name"`
	Body       string          `json:"body"`
	Mentions   json.RawMessage `json:"mentions"`
	CreatedAt  string          `json:"created_at"`
}

var mentionRe = regexp.MustCompile(`@([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// extractMentions: @email or @uuid tokens in the body → json array.
// ponytail: regex extraction only — NLP mention resolution when the
// web UI ships a picker.
func extractMentions(body string) []byte {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range mentionRe.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	if out == nil {
		return []byte("[]")
	}
	b, _ := json.Marshal(out)
	return b
}

func validateMessage(body string) string {
	body = trimAll(body)
	if len(body) == 0 || len(body) > 2000 {
		return "message body must be 1-2000 chars"
	}
	return ""
}

func trimAll(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// POST /v1/tasks/{id}/messages — staff (user actor)
func (s *Server) createTaskMessage(w http.ResponseWriter, r *http.Request) {
	s.postMessage(w, r, "user")
}

// postMessage: shared by staff + portal (author type differs; author
// id resolves from ctx — user_id for staff, the link's contact for
// portal). Spam guard (§276): 30 msgs/min/author.
func (s *Server) postMessage(w http.ResponseWriter, r *http.Request, authorType string) {
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if msg := validateMessage(req.Body); msg != "" {
		problem(w, http.StatusBadRequest, msg)
		return
	}
	// trim for storage
	req.Body = trimAll(req.Body)

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var authorID string
	if authorType == "user" {
		// staff path: staffTx sets ws+user ctx
		if _, err := tx.Exec(ctx,
			"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
			workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		authorID = userFromCtx(ctx)
		if authorID == "" {
			problem(w, http.StatusBadRequest, "no user identity for chat")
			return
		}
	} else {
		// portal wrapper already opened tx; this is NOT re-opened —
		// unreachable via the portal route (see portalPostMessage)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	if ok := throttleMessages(ctx, tx, authorType, authorID); !ok {
		w.Header().Set("Retry-After", "60")
		problem(w, http.StatusTooManyRequests, "too many messages — slow down")
		return
	}

	var m messageOut
	var created time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO task_messages (workspace_id, task_id, author_type, author_id, body, mentions)
		SELECT t.workspace_id, t.id, 'user', $2::uuid, $3, $4::jsonb
		FROM tasks t
		WHERE t.id = $1::uuid AND t.deleted_at IS NULL
		RETURNING id, task_id, author_type, COALESCE(author_id::text,''), body, mentions, created_at`,
		r.PathValue("id"), authorID, req.Body, extractMentions(req.Body)).
		Scan(&m.ID, &m.TaskID, &m.AuthorType, &m.AuthorID, &m.Body, &m.Mentions, &created)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "task not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		SELECT t.workspace_id, 'task_message', $1, 'user', $2::uuid, 'message.created', 'api'
		FROM tasks t WHERE t.id = $3::uuid`,
		m.ID, authorID, r.PathValue("id")); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	m.AuthorName = userNameFromID(ctx, s.pool, authorID)
	m.CreatedAt = created.UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusCreated, m)
}

// portalPostMessage: runs inside the portal() wrapper's tx.
func (s *Server) portalPostMessage(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, r *http.Request) (int, error) {
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return 0, nil
	}
	if msg := validateMessage(req.Body); msg != "" {
		problem(w, http.StatusBadRequest, msg)
		return 0, nil
	}
	req.Body = trimAll(req.Body)
	sess := sessionFromCtx(ctx)

	if ok := throttleMessages(ctx, tx, "contact", sess.ContactID); !ok {
		w.Header().Set("Retry-After", "60")
		problem(w, http.StatusTooManyRequests, "too many messages — slow down")
		return 0, nil
	}

	var m messageOut
	var created time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO task_messages (workspace_id, task_id, author_type, author_id, body, mentions)
		SELECT t.workspace_id, t.id, 'contact', $2::uuid, $3, $4::jsonb
		FROM tasks t
		WHERE t.id = $1::uuid AND t.customer_visible AND t.deleted_at IS NULL
		RETURNING id, task_id, author_type, COALESCE(author_id::text,''), body, mentions, created_at`,
		r.PathValue("task_id"), sess.ContactID, req.Body, extractMentions(req.Body)).
		Scan(&m.ID, &m.TaskID, &m.AuthorType, &m.AuthorID, &m.Body, &m.Mentions, &created)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "task not found")
			return 0, nil
		}
		return http.StatusInternalServerError, errQuiet
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		SELECT t.workspace_id, 'task_message', $1, 'contact', $2::uuid, 'message.created', 'portal'
		FROM tasks t WHERE t.id = $3::uuid`,
		m.ID, sess.ContactID, r.PathValue("task_id")); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	if err := tx.Commit(ctx); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	m.AuthorName = sess.ContactName
	m.CreatedAt = created.UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusCreated, m)
	return 0, nil
}

// throttleMessages: 30/min/author (§276 chat spam edge).
func throttleMessages(ctx context.Context, tx pgx.Tx, authorType, authorID string) bool {
	var n int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM task_messages
		WHERE author_type = $1 AND author_id = $2::uuid
		  AND created_at > now() - interval '1 minute'`,
		authorType, authorID).Scan(&n); err != nil {
		return true // fail open on the counter: posting matters more
	}
	return n < 30
}

func userNameFromID(ctx context.Context, pool *pgxpool.Pool, id string) string {
	var name string
	if err := pool.QueryRow(ctx, `SELECT display_name FROM users WHERE id = $1::uuid`, id).Scan(&name); err != nil {
		return "staff"
	}
	return name
}

// GET /v1/tasks/{id}/messages — staff thread, ?after= cursor
func (s *Server) listTaskMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	hits, status := scanMessages(ctx, tx, r.PathValue("id"), r.URL.Query().Get("after"), r.URL.Query().Get("after_id"))
	if status != 0 {
		problem(w, status, http.StatusText(status))
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

// portalListMessages: portal thread (only messages on tasks the link
// can see — enforced by the RLS policies on the join).
func (s *Server) portalListMessages(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, r *http.Request) (int, error) {
	hits, status := scanMessages(ctx, tx, r.PathValue("task_id"), r.URL.Query().Get("after"), r.URL.Query().Get("after_id"))
	if status != 0 {
		problem(w, status, http.StatusText(status))
		return 0, nil
	}
	writeJSON(w, http.StatusOK, hits)
	return 0, nil
}

func scanMessages(ctx context.Context, tx pgx.Tx, taskID, after, afterID string) ([]messageOut, int) {
	// existence check first: the task itself (404 before empty thread)
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM tasks WHERE id = $1::uuid AND deleted_at IS NULL`, taskID).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, http.StatusNotFound
		}
		return nil, http.StatusInternalServerError
	}
	rows, err := tx.Query(ctx, `
		SELECT m.id, m.task_id, m.author_type, COALESCE(m.author_id::text,''),
		       COALESCE(u.display_name, c.display_name, ''),
		       m.body, m.mentions, m.created_at
		FROM task_messages m
		LEFT JOIN users u ON u.id = m.author_id AND m.author_type = 'user'
		LEFT JOIN contacts c ON c.id = m.author_id AND m.author_type = 'contact'
		WHERE m.task_id = $1::uuid
		  AND ($2 = '' OR (m.created_at, m.id) > (
		      SELECT c2.created_at, c2.id FROM task_messages c2 WHERE c2.id = $2::uuid))
		ORDER BY m.created_at ASC, m.id ASC
		LIMIT 200`,
		taskID, afterID)
	if err != nil {
		return nil, http.StatusInternalServerError
	}
	defer rows.Close()
	out := []messageOut{}
	for rows.Next() {
		var m messageOut
		var created time.Time
		if err := rows.Scan(&m.ID, &m.TaskID, &m.AuthorType, &m.AuthorID, &m.AuthorName, &m.Body, &m.Mentions, &created); err != nil {
			return nil, http.StatusInternalServerError
		}
		m.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, m)
	}
	return out, 0
}
