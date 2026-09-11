package server

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// metrics v1 (§588 acceptance): TTV rollup, portal open rate, API latency
// percentiles. All data already exists — this is the measurement surface.

// rec is a low-cardinality latency record: route pattern (Go 1.22 mux
// r.Pattern), status class (2xx/4xx/5xx), duration.
type rec struct {
	route string
	class string
	d     time.Duration
}

type metrics struct {
	mu      sync.Mutex
	records []rec
	// separate counters for totals without scanning
	total  uint64
	errors uint64
}

var met = &metrics{}

// recordMetrics wraps the entire mux. r.Pattern is the registered route
// pattern ("GET /v1/projects/{id}") — low cardinality by construction, so
// no label explosion. Static/healthz included; that's fine (they're part
// of p95 reality).
func recordMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		class := "2xx"
		switch {
		case sw.status >= 500:
			class = "5xx"
		case sw.status >= 400:
			class = "4xx"
		}
		pattern := r.Pattern
		if pattern == "" { // unmatched → 404 from the mux itself
			pattern = "unmatched"
		}
		met.mu.Lock()
		met.records = append(met.records, rec{pattern, class, time.Since(start)})
		met.total++
		if sw.status >= 500 {
			met.errors++
		}
		// ponytail: unbounded ring — trim to last 50k; P0 single-instance,
		// restart clears; move to a proper histogram when multi-instance
		if len(met.records) > 50000 {
			met.records = met.records[len(met.records)/2:]
		}
		met.mu.Unlock()
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// percentiles returns p50/p95/p99 in ms over collected records (global —
// route-level percentiles aren't in the acceptance line, totals are).
func (m *metrics) percentiles() (p50, p95, p99 float64, total uint64) {
	m.mu.Lock()
	ns := make([]float64, 0, len(m.records))
	for _, r := range m.records {
		ns = append(ns, float64(r.d.Microseconds())/1000.0)
	}
	total, errs := m.total, m.errors
	m.mu.Unlock()
	_ = errs
	n := len(ns)
	if n == 0 {
		return 0, 0, 0, 0
	}
	sort.Float64s(ns)
	pct := func(p float64) float64 {
		idx := int(math.Ceil(p/100*float64(n))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= n {
			idx = n - 1
		}
		return ns[idx]
	}
	return pct(50), pct(95), pct(99), total
}

// ttvOut is the workspace rollup: completed projects with actual_go_live,
// days from created_at → actual_go_live (the §588 "customer gets value").
type ttvOut struct {
	Count int     `json:"count"`
	Avg   float64 `json:"avg_days"`
	Min   float64 `json:"min_days"`
	Max   float64 `json:"max_days"`
}

type openRateOut struct {
	ActiveLinks int     `json:"active_links"`
	Opened      int     `json:"opened"`
	Rate        float64 `json:"rate"`
}

type metricsOut struct {
	TTV        ttvOut      `json:"ttv"`
	PortalOpen openRateOut `json:"portal_open_rate"`
	APILatency latencyOut  `json:"api_latency"`
}

type latencyOut struct {
	P50MS    float64 `json:"p50_ms"`
	P95MS    float64 `json:"p95_ms"`
	P99MS    float64 `json:"p99_ms"`
	Requests uint64  `json:"requests"`
	Errors   uint64  `json:"errors"`
}

// getMetrics: staff JSON under the caller's workspace ctx (RLS decides).
func (s *Server) getMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	var out metricsOut
	// TTV: completed projects that actually went live.
	if err := tx.QueryRow(ctx, `
		SELECT count(*), COALESCE(avg(EXTRACT(EPOCH FROM (actual_go_live::timestamptz - created_at))/86400.0), 0),
		       COALESCE(min(EXTRACT(EPOCH FROM (actual_go_live::timestamptz - created_at))/86400.0), 0),
		       COALESCE(max(EXTRACT(EPOCH FROM (actual_go_live::timestamptz - created_at))/86400.0), 0)
		FROM projects
		WHERE status = 'completed' AND actual_go_live IS NOT NULL AND deleted_at IS NULL`).Scan(
		&out.TTV.Count, &out.TTV.Avg, &out.TTV.Min, &out.TTV.Max); err != nil && err != pgx.ErrNoRows {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	out.TTV.Avg = math.Round(out.TTV.Avg*10) / 10
	out.TTV.Min = math.Round(out.TTV.Min*10) / 10
	out.TTV.Max = math.Round(out.TTV.Max*10) / 10

	// Portal open rate: active links ever used / active links.
	if err := tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE last_used_at IS NOT NULL)
		FROM portal_links WHERE status = 'active' AND deleted_at IS NULL`).Scan(
		&out.PortalOpen.ActiveLinks, &out.PortalOpen.Opened); err != nil && err != pgx.ErrNoRows {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if out.PortalOpen.ActiveLinks > 0 {
		out.PortalOpen.Rate = float64(out.PortalOpen.Opened) / float64(out.PortalOpen.ActiveLinks)
	}

	p50, p95, p99, _ := met.percentiles()
	out.APILatency = latencyOut{P50MS: round1(p50), P95MS: round1(p95), P99MS: round1(p99), Requests: met.total, Errors: met.errors}

	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// prometheus: text exposition of the same data, scrapable at /metrics.
// ponytail: hand-rolled text format; swap for prometheus/client_golang if
// we ever need histograms/buckets or multi-process merging.
func (s *Server) prometheus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var ttv ttvOut
	if err := tx.QueryRow(ctx, `
		SELECT count(*), COALESCE(avg(EXTRACT(EPOCH FROM (actual_go_live::timestamptz - created_at))/86400.0), 0),
		       COALESCE(min(EXTRACT(EPOCH FROM (actual_go_live::timestamptz - created_at))/86400.0), 0),
		       COALESCE(max(EXTRACT(EPOCH FROM (actual_go_live::timestamptz - created_at))/86400.0), 0)
		FROM projects
		WHERE status = 'completed' AND actual_go_live IS NOT NULL AND deleted_at IS NULL`).Scan(
		&ttv.Count, &ttv.Avg, &ttv.Min, &ttv.Max); err != nil && err != pgx.ErrNoRows {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var po openRateOut
	if err := tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE last_used_at IS NOT NULL)
		FROM portal_links WHERE status = 'active' AND deleted_at IS NULL`).Scan(
		&po.ActiveLinks, &po.Opened); err != nil && err != pgx.ErrNoRows {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p50, p95, p99, total := met.percentiles()

	var b strings.Builder
	fmt.Fprintf(&b, "# HELP openlane_ttv_days Time-to-value: created→go-live days for completed projects.\n")
	fmt.Fprintf(&b, "# TYPE openlane_ttv_days gauge\n")
	fmt.Fprintf(&b, "openlane_ttv_days{stat=\"avg\"} %.1f\n", ttv.Avg)
	fmt.Fprintf(&b, "openlane_ttv_days{stat=\"min\"} %.1f\n", ttv.Min)
	fmt.Fprintf(&b, "openlane_ttv_days{stat=\"max\"} %.1f\n", ttv.Max)
	fmt.Fprintf(&b, "openlane_ttv_days_completed_projects %d\n", ttv.Count)
	fmt.Fprintf(&b, "# HELP openlane_portal_open_rate Active portal links opened at least once / active links.\n")
	fmt.Fprintf(&b, "# TYPE openlane_portal_open_rate gauge\n")
	fmt.Fprintf(&b, "openlane_portal_open_rate %.4f\n", po.Rate)
	fmt.Fprintf(&b, "openlane_portal_links_active %d\n", po.ActiveLinks)
	fmt.Fprintf(&b, "openlane_portal_links_opened %d\n", po.Opened)
	fmt.Fprintf(&b, "# HELP openlane_api_latency_ms Request latency percentiles in milliseconds.\n")
	fmt.Fprintf(&b, "# TYPE openlane_api_latency_ms gauge\n")
	fmt.Fprintf(&b, "openlane_api_latency_ms{p=\"50\"} %.1f\n", p50)
	fmt.Fprintf(&b, "openlane_api_latency_ms{p=\"95\"} %.1f\n", p95)
	fmt.Fprintf(&b, "openlane_api_latency_ms{p=\"99\"} %.1f\n", p99)
	fmt.Fprintf(&b, "# HELP openlane_api_requests_total Requests served since boot.\n")
	fmt.Fprintf(&b, "# TYPE openlane_api_requests_total counter\n")
	fmt.Fprintf(&b, "openlane_api_requests_total %d\n", total)
	fmt.Fprintf(&b, "openlane_api_errors_total %d\n", met.errors)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
