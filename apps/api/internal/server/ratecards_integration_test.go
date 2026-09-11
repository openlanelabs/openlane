//go:build integration

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestRateCardsCRUD(t *testing.T) {
	_, _, h := filesTestStack(t)

	// create: needs at least one rate
	code, body := h.do("POST", "/v1/rate-cards", map[string]any{"name": "Default 2026"})
	if code != http.StatusBadRequest {
		t.Fatalf("no-rates = %d %s (want 400)", code, body)
	}
	// create: negative rate rejected (§328)
	code, body = h.do("POST", "/v1/rate-cards", map[string]any{
		"name": "Bad", "rates": []map[string]any{{"role": "pm", "hourly_rate": -5}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("negative rate = %d %s (want 400)", code, body)
	}

	// create default card (no customer) with two rates
	code, body = h.do("POST", "/v1/rate-cards", map[string]any{
		"name":     "Acme Default 2026",
		"currency": "USD",
		"rates": []map[string]any{
			{"role": "architect", "hourly_rate": 200},
			{"role": "pm", "hourly_rate": 150},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var card struct {
		ID       string `json:"id"`
		Currency string `json:"currency"`
		Rates    []struct {
			Role       string  `json:"role"`
			HourlyRate float64 `json:"hourly_rate"`
		} `json:"rates"`
	}
	json.Unmarshal(body, &card)
	if card.Currency != "USD" || len(card.Rates) != 2 || card.Rates[0].Role != "architect" || card.Rates[0].HourlyRate != 200 {
		t.Fatalf("card shape: %s", body)
	}

	// customer-scoped card (Adobe = seeded customer)
	code, body = h.do("POST", "/v1/rate-cards", map[string]any{
		"customer_id": custID, "name": "Adobe Premier 2026", "currency": "EUR",
		"rates": []map[string]any{{"role": "architect", "hourly_rate": 250}},
	})
	if code != http.StatusCreated {
		t.Fatalf("customer card = %d %s", code, body)
	}
	var adobe struct {
		ID         string  `json:"id"`
		CustomerID *string `json:"customer_id"`
	}
	json.Unmarshal(body, &adobe)
	if adobe.CustomerID == nil || *adobe.CustomerID != custID {
		t.Fatalf("customer card customer_id: %s", body)
	}

	// FK customer from another workspace → 400
	code, body = h.do("POST", "/v1/rate-cards", map[string]any{
		"customer_id": "22222222-2222-2222-2222-222222222222", "name": "X", // ws B id — not a customer
		"rates": []map[string]any{{"role": "pm", "hourly_rate": 1}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad customer = %d %s (want 400)", code, body)
	}

	// list: 2 cards, rates aggregated
	code, body = h.do("GET", "/v1/rate-cards", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d %s", code, body)
	}
	var list []map[string]any
	json.Unmarshal(body, &list)
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}

	// get single
	code, body = h.do("GET", "/v1/rate-cards/"+card.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("get = %d %s", code, body)
	}

	// patch: only is_active
	code, body = h.do("PATCH", "/v1/rate-cards/"+card.ID, map[string]any{"is_active": false})
	if code != http.StatusOK {
		t.Fatalf("patch = %d %s", code, body)
	}
	code, body = h.do("PATCH", "/v1/rate-cards/"+card.ID, map[string]any{})
	if code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d %s (want 400 — rates immutable)", code, body)
	}

	// delete → 204, then get 404, list drops to 1
	code, body = h.do("DELETE", "/v1/rate-cards/"+adobe.ID, nil)
	if code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", code, body)
	}
	code, _ = h.do("GET", "/v1/rate-cards/"+adobe.ID, nil)
	if code != http.StatusNotFound {
		t.Fatalf("get after delete = %d (want 404)", code)
	}
	code, body = h.do("GET", "/v1/rate-cards", nil)
	json.Unmarshal(body, &list)
	if len(list) != 1 {
		t.Fatalf("list after delete = %d, want 1", len(list))
	}
}

func TestRateCardsCrossWorkspace(t *testing.T) {
	_, _, h := filesTestStack(t)
	// ws A creates a card
	code, body := h.do("POST", "/v1/rate-cards", map[string]any{
		"name": "A only", "rates": []map[string]any{{"role": "dev", "hourly_rate": 100}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}

	// static token + X-Workspace-Id of an unseeded workspace → RLS hides all
	// of A's cards (tenant isolation rides the same middleware/RLS path).
	res, body := h.doWS("GET", "/v1/rate-cards", nil, "22222222-2222-2222-2222-222222222222")
	if res != http.StatusOK {
		t.Fatalf("beta list = %d %s", res, body)
	}
	var list []map[string]any
	json.Unmarshal(body, &list)
	if len(list) != 0 {
		t.Fatalf("beta sees %d cards (want 0)", len(list))
	}
}

// doWS: same as do() but with a different X-Workspace-Id (tenant switch).
func (h *httptestSrv) doWS(method, path string, body any, wsID string) (int, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+staffTok)
	req.Header.Set("X-Workspace-Id", wsID)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer r.Body.Close()
	out, _ := io.ReadAll(r.Body)
	return r.StatusCode, out
}
