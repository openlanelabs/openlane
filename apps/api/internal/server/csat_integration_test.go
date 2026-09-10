//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const csatPortalToken = "csat-portal-token-1234567890abcdefghijkLMNOP"

func csatTestStack(t *testing.T) *httptestSrv {
	t.Helper()
	_, _, h := filesTestStack(t)
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })
	if _, err := ap.Exec(context.Background(),
		`INSERT INTO portal_links (workspace_id, project_id, contact_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, encode(sha256($4::bytea),'hex'), now() + interval '7 days')`,
		wsA, projA, contactID, csatPortalToken); err != nil {
		t.Fatalf("link: %v", err)
	}
	return h
}

func TestCSATFlow(t *testing.T) {
	h := csatTestStack(t)
	admin := adminDSN(t)
	ap, _ := pgxpool.New(context.Background(), admin)
	defer ap.Close()

	// staff creates + customer decides one approval
	decide := func(title string) string {
		code, body := h.do("POST", "/v1/projects/"+projA+"/approvals", map[string]any{"title": title})
		if code != http.StatusCreated {
			t.Fatalf("create %q = %d %s", title, code, body)
		}
		var a approvalOut
		json.Unmarshal(body, &a)
		if code, body = h.doPortalJSON("POST", "/p/"+csatPortalToken+"/approvals/"+a.ID+"/decide",
			map[string]string{"decision": "approved"}); code != 200 {
			t.Fatalf("decide %q = %d %s", title, code, body)
		}
		return a.ID
	}
	highID := decide("Kickoff scope sign-off")
	lowID := decide("Data migration sign-off")
	thirdID := decide("Go-live readiness check")

	pURL := "/p/" + csatPortalToken
	csat := func(approval string, score int, comment string) (int, []byte) {
		return h.doPortalJSON("POST", pURL+"/csat",
			map[string]any{"approval_id": approval, "score": score, "comment": comment})
	}

	// rating a PENDING approval → 404. Make one pending first.
	code, body := h.do("POST", "/v1/projects/"+projA+"/approvals", map[string]any{"title": "pending one"})
	var pending approvalOut
	json.Unmarshal(body, &pending)
	if code, _ := csat(pending.ID, 5, ""); code != 404 {
		t.Fatalf("pending rating = %d, want 404", code)
	}

	// high score: no escalation
	if code, body = csat(highID, 5, "amazing"); code != http.StatusCreated || !strings.Contains(string(body), "😄") {
		t.Fatalf("high = %d %s", code, body)
	}
	// low score: escalation task auto-created
	if code, body = csat(lowID, 2, "too slow"); code != http.StatusCreated || !strings.Contains(string(body), `"escalated":true`) {
		t.Fatalf("low = %d %s", code, body)
	}
	// second low score on a different approval, same project → deduped (no new task)
	if code, body = csat(thirdID, 1, "still slow"); code != http.StatusCreated || strings.Contains(string(body), `"escalated":true`) {
		t.Fatalf("low2 = %d %s", code, body)
	}
	// re-rate same approval → 404 (unique, no oracle)
	if code, _ := csat(highID, 4, ""); code != 404 {
		t.Fatalf("re-rate = %d, want 404", code)
	}
	// bad scores → 400
	if code, _ := csat(pending.ID, 0, ""); code != 400 {
		t.Fatal("score 0 should 400")
	}
	if code, _ := csat(pending.ID, 6, ""); code != 400 {
		t.Fatal("score 6 should 400")
	}

	// staff analytics: 3 responses / 3 decided → rate 1.0, avg 8/3
	code, body = h.do("GET", "/v1/projects/"+projA+"/csat", nil)
	if code != 200 {
		t.Fatalf("csat list = %d %s", code, body)
	}
	var stats struct {
		Responses    []csatOut `json:"responses"`
		AverageScore *float64  `json:"average_score"`
		ResponseRate float64   `json:"response_rate"`
	}
	json.Unmarshal(body, &stats)
	if len(stats.Responses) != 3 || stats.ResponseRate != 1.0 {
		t.Fatalf("stats: %d responses rate %v", len(stats.Responses), stats.ResponseRate)
	}
	if stats.AverageScore == nil || *stats.AverageScore < 2.6 || *stats.AverageScore > 2.7 {
		t.Fatalf("avg = %v", stats.AverageScore)
	}
	// trend ascending, capped at 5, includes comment
	if stats.Responses[0].Score != 5 || stats.Responses[0].Comment != "amazing" {
		t.Fatalf("trend order: %+v", stats.Responses)
	}

	// escalation task exists once, internal-only, not customer-visible
	var tasks int
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM tasks WHERE title LIKE 'CSAT escalation:%'`).Scan(&tasks)
	if tasks != 1 {
		t.Fatalf("escalation tasks = %d, want 1", tasks)
	}

	// audit: 3 csat.submitted rows, contact actor
	var audit int
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action='csat.submitted' AND actor_type='contact'`).Scan(&audit)
	if audit != 3 {
		t.Fatalf("csat audits = %d", audit)
	}
}
