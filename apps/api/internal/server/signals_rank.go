package server

import (
	"encoding/json"
	"sort"
	"time"
)

// ML risk ranking (§357 'rules + ML') — honest ML: a logistic model
// with hand-set, auditable weights. The linear form IS the point:
// every score decomposes into factors a delivery lead can read, and
// the weights cite their rationale inline.
//
// ponytail: hand-set weights now; when outcome labels exist (CSAT +
// churn linked to signal history), fit them by logistic regression —
// same feature vector, same factors output, weights become data.

// riskWeights: rationale per weight (delivery-ops base rates):
// silent-portal outranks overdue-cluster for churn correlation;
// margin is the slowest-burning risk (least actionable per day).
var riskWeights = struct {
	severityRed, severityWarn  float64
	overduePerTask, overdueCap float64
	silencePerDay, silenceCap  float64
	burnOverRatio              float64
	marginPerPctBelow          float64
	stalePerDay, staleCap      float64
	healthAtRisk               float64
}{
	severityRed:    1.0,  // red = the action contract baseline
	severityWarn:   0.35, // warns act, red acts first
	overduePerTask: 0.09, overdueCap: 0.9,
	silencePerDay: 0.05, silenceCap: 0.7,
	burnOverRatio:     0.8,        // logged past budget hours
	marginPerPctBelow: 0.6 / 20.0, // per pct-point under the 20% floor
	stalePerDay:       0.035, staleCap: 0.5,
	healthAtRisk: 0.4,
}

// riskFactors: the decomposition — evidence-linked (§357: 'every
// signal has evidence links, not just score').
type riskFactors struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
}

// rankSignals: score in-place. Additive payload only — severity (the
// automation/nudge contract) is untouched.
func rankSignals(out []signalOut) {
	scores := make([]float64, len(out))
	factors := make([]riskFactors, 0, 4)
	for i := range out {
		sig := &out[i]
		var ev map[string]any
		_ = json.Unmarshal(sig.Evidence, &ev)

		var z float64
		factors = factors[:0]
		add := func(name string, w float64) {
			if w <= 0 {
				return
			}
			z += w
			factors = append(factors, riskFactors{Name: name, Weight: round2(w)})
		}

		if sig.Severity == "red" {
			add("severity: red", riskWeights.severityRed)
		} else {
			add("severity: warn", riskWeights.severityWarn)
		}
		if ids, _ := ev["overdue_task_ids"].([]any); len(ids) > 0 {
			w := float64(len(ids)) * riskWeights.overduePerTask
			if w > riskWeights.overdueCap {
				w = riskWeights.overdueCap
			}
			add("overdue tasks", w)
		}
		if used, ok := ev["last_used_at"].(string); ok && used != "" {
			if days := daysSince(used); days > 0 {
				w := float64(days) * riskWeights.silencePerDay
				if w > riskWeights.silenceCap {
					w = riskWeights.silenceCap
				}
				add("portal silence (days)", w)
			}
		} else if sig.Kind == "portal_silent" {
			add("portal never used", riskWeights.silenceCap)
		}
		if logged, budget := num(ev["logged_minutes"]), num(ev["budget_hours"]); budget > 0 && logged > 0 {
			burn := logged / (budget * 60)
			if burn > 1 {
				add("budget overage", riskWeights.burnOverRatio*(burn-1))
			}
		}
		if pct, ok := ev["margin_pct"].(float64); ok && pct < 20 {
			add("margin under floor", (20-pct)*riskWeights.marginPerPctBelow)
		}
		if created, ok := ev["created_at"].(string); ok && created != "" {
			if days := daysSince(created); days > 7 {
				w := float64(days) * riskWeights.stalePerDay
				if w > riskWeights.staleCap {
					w = riskWeights.staleCap
				}
				add("approval age (days)", w)
			}
		}
		if sig.Kind == "margin_dip" || sig.Kind == "budget_burn" {
			add("financial health", riskWeights.healthAtRisk)
		}

		// logistic squash → 0..1
		score := 1 / (1 + expNeg(z))
		scores[i] = round2(score)
		sig.RiskScore = scores[i]
		if len(factors) > 0 {
			sig.Factors = mustJSON(factors)
		}
	}
	// stable sort desc by score (severity remains the tiebreaker —
	// ordering shifts only when the model says so)
	sort.SliceStable(out, func(i, j int) bool {
		if scores[i] != scores[j] {
			return scores[i] > scores[j]
		}
		return out[i].Severity == "red" && out[j].Severity != "red"
	})
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

func expNeg(z float64) float64 {
	// e^-z without importing math for one call — two-term Padé is
	// plenty for a 0..1 ranking squash
	if z < 0 {
		return 1 + (-z) + (-z)*(-z)/2
	}
	d := 1 + z + z*z/2
	return 1 / d
}

// num: json numbers arrive as float64.
func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// daysSince: timestamptz rendered to text by Postgres; day granularity
// is all the ranking needs.
func daysSince(ts string) int {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		// Postgres may render without timezone — try local
		if t, err = time.Parse("2006-01-02 15:04:05.999999-07", ts); err != nil {
			return 0
		}
	}
	d := time.Since(t).Hours() / 24
	if d < 0 {
		return 0
	}
	return int(d)
}
