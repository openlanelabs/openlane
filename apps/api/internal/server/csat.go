package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// csat.go — §7.4/§14: 1-click 😞😐😊 at approval time. One response per
// approval (UNIQUE); low scores (<3) auto-open an escalation task via the
// csat_escalate SECURITY DEFINER fn (portal ctx can't INSERT tasks by
// design); staff get trend + response rate.

type csatOut struct {
	ID         string `json:"id"`
	ApprovalID string `json:"approval_id"`
	ProjectID  string `json:"project_id"`
	Score      int16  `json:"score"`
	Comment    string `json:"comment,omitempty"`
	Emoji      string `json:"emoji"`
	CreatedAt  string `json:"created_at"`
}

func emoji(score int16) string {
	switch {
	case score >= 5:
		return "😄"
	case score == 4:
		return "🙂"
	case score == 3:
		return "😐"
	default:
		return "😞"
	}
}

// POST /v1/portal/{token}/csat — submit one response for a decided
// approval in link scope. Undecided/foreign/already-rated all 404.
func (s *Server) submitPortalCSAT(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, r *http.Request) (int, error) {
	var req struct {
		ApprovalID string `json:"approval_id"`
		Score      int16  `json:"score"`
		Comment    string `json:"comment"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil ||
		req.ApprovalID == "" || req.Score < 1 || req.Score > 5 {
		problem(w, http.StatusBadRequest, "score must be 1-5 with an approval_id")
		return 0, nil
	}
	sess := sessionFromCtx(ctx)

	// Step 1: fetch the approval under portal RLS scope (decided, not
	// deleted, in the link's projects — else plain 404, no oracle).
	var ws, projID string
	err := tx.QueryRow(ctx, `
		SELECT a.workspace_id::text, a.project_id::text
		FROM approvals a
		WHERE a.id = $1::uuid AND a.deleted_at IS NULL
		  AND a.status IN ('approved','changes_requested')`,
		req.ApprovalID).Scan(&ws, &projID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "no rating pending for that approval")
			return 0, nil
		}
		return http.StatusInternalServerError, errQuiet
	}

	// Step 2: insert — UNIQUE(approval_id) makes re-rating 404 too.
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO csat_responses (workspace_id, approval_id, project_id, contact_id, score, comment)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, NULLIF($6, ''))
		RETURNING id`,
		ws, req.ApprovalID, projID, sess.ContactID, req.Score, req.Comment).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.Is(err, pgx.ErrNoRows) ||
			(errors.As(err, &pgErr) && pgErr.Code == "23505") { // unique: already rated — same 404, no oracle
			problem(w, http.StatusNotFound, "no rating pending for that approval")
			return 0, nil
		}
		return http.StatusInternalServerError, errQuiet
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, new_value, source)
		VALUES ($1::uuid, 'csat', $2::uuid, 'contact', $3::uuid, 'csat.submitted',
		        jsonb_build_object('score', $4::smallint), 'portal')`,
		ws, id, sess.ContactID, req.Score); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	var escalated bool
	if req.Score < 3 {
		// ponytail: row-by-row escalation; a batched signal sweep replaces this when P1 signals land
		var escID *string
		if err := tx.QueryRow(ctx, `SELECT csat_escalate($1::uuid, $2, $3)`, projID, req.Score, req.Comment).Scan(&escID); err != nil {
			return http.StatusInternalServerError, errQuiet
		}
		escalated = escID != nil
	}
	if err := tx.Commit(ctx); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	text := emoji(req.Score) + " CSAT " + strconv.Itoa(int(req.Score)) + "/5 on " + sess.ProjectName
	if escalated {
		text += " — escalation task opened"
	}
	s.notifyEvent(context.WithoutCancel(ctx), sess.WorkspaceID, "csat.submitted", text, req.ApprovalID)
	out := map[string]any{"id": id, "score": req.Score, "emoji": emoji(req.Score), "escalated": escalated}
	writeJSON(w, http.StatusCreated, out)
	return 0, nil
}

// GET /v1/projects/{id}/csat — staff analytics: responses, avg score,
// response rate (responses / decided approvals) and last-5 trend (§14).
func (s *Server) listProjectCSAT(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT c.id, c.approval_id, c.project_id, c.score, COALESCE(c.comment,''), c.created_at
		FROM csat_responses c
		WHERE c.project_id = $1::uuid
		ORDER BY c.created_at DESC`,
		r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []csatOut{}
	for rows.Next() {
		var c csatOut
		var created time.Time
		if err := rows.Scan(&c.ID, &c.ApprovalID, &c.ProjectID, &c.Score, &c.Comment, &created); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		c.Emoji = emoji(c.Score)
		c.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, c)
	}
	rows.Close()

	var avg *float64
	var decided, responded int
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT avg(score)::float8 FROM csat_responses WHERE project_id = $1::uuid),
		       (SELECT count(*) FROM approvals
		         WHERE project_id = $1::uuid AND status IN ('approved','changes_requested')),
		       (SELECT count(*) FROM csat_responses WHERE project_id = $1::uuid)`,
		r.PathValue("id")).Scan(&avg, &decided, &responded); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	rate := 0.0
	if decided > 0 {
		rate = float64(responded) / float64(decided)
	}
	var avgAny any
	if avg != nil {
		avgAny = *avg
	}
	// trend: chronological, capped at last 5
	n := len(out)
	if n > 5 {
		out = out[n-5:]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // reversed to ascending
		out[i], out[j] = out[j], out[i]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"responses":     out,
		"average_score": avgAny,
		"response_rate": rate,
	})
}
