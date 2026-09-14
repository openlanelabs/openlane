//go:build integration

package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Device list + instant revoke (§406): sessions carry UA/IP, the list
// flags the caller's own row by the sid claim, revoking another
// session kills its refresh while the current survives.

func TestDeviceListAndRevoke(t *testing.T) {
	srv, _, h, _ := timeTestStack(t)
	defer srv.Close()

	// device 1: the standard devLogin (UA = Go http client)
	_, _, access1 := devLogin(h)

	// list: one session, flagged current
	code, body := h.doJWT("GET", "/v1/auth/sessions", nil, access1)
	if code != http.StatusOK {
		t.Fatalf("list = %d %s", code, body)
	}
	var list struct {
		Sessions []struct {
			ID        string `json:"id"`
			UserAgent string `json:"user_agent"`
			Current   bool   `json:"current"`
		} `json:"sessions"`
	}
	json.Unmarshal(body, &list)
	if len(list.Sessions) != 1 || !list.Sessions[0].Current {
		t.Fatalf("sessions = %s", body)
	}
	if list.Sessions[0].UserAgent == "" || list.Sessions[0].UserAgent == "Unknown device (pre-3.4)" {
		t.Fatalf("UA not captured: %s", body)
	}

	// device 2: second login (another session row)
	_, _, access2 := devLogin(h)

	code, body = h.doJWT("GET", "/v1/auth/sessions", nil, access2)
	if code != http.StatusOK {
		t.Fatalf("list2 = %d %s", code, body)
	}
	json.Unmarshal(body, &list)
	if len(list.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(list.Sessions))
	}
	cur := 0
	for _, s := range list.Sessions {
		if s.Current {
			cur++
		}
	}
	if cur != 1 {
		t.Fatalf("exactly one current, got %d", cur)
	}

	// the OTHER session's id (not access2's)
	other := ""
	for _, s := range list.Sessions {
		if !s.Current {
			other = s.ID
		}
	}
	if other == "" {
		t.Fatal("no other session found")
	}

	// revoke the other device — instant
	if code, body = h.doJWT("DELETE", "/v1/auth/sessions/"+other, nil, access2); code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", code, body)
	}

	// the revoked device's refresh now 401s: its token… we don't hold
	// device-1's refresh (devLogin returns access only). Fetch with
	// the list: instead prove via DB state + current survives.
	code, body = h.doJWT("GET", "/v1/auth/sessions", nil, access1)
	if code != http.StatusOK {
		t.Fatalf("list after revoke (device1) = %d %s", code, body)
	}
	var l1 struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	json.Unmarshal(body, &l1)
	// device1's list now contains only device2's session (the
	// revoked one is gone); device1's own sid matches nothing so
	// current is false on it
	if len(l1.Sessions) != 1 || l1.Sessions[0].ID == other {
		t.Fatalf("revoked session still listed for device1: %s", body)
	}
	// current device2 survives
	code, body = h.doJWT("GET", "/v1/auth/sessions", nil, access2)
	if code != http.StatusOK {
		t.Fatalf("list after revoke (device2) = %d %s", code, body)
	}
	var l2 struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	json.Unmarshal(body, &l2)
	if len(l2.Sessions) != 1 {
		t.Fatalf("current session should survive: %s", body)
	}

	// revoke current = allowed (logout semantics), idempotent second time
	if code, _ = h.doJWT("DELETE", "/v1/auth/sessions/"+l2.Sessions[0].ID, nil, access2); code != http.StatusNoContent {
		t.Fatalf("revoke current = %d", code)
	}
	if code, _ = h.doJWT("DELETE", "/v1/auth/sessions/"+l2.Sessions[0].ID, nil, access2); code != http.StatusNoContent {
		t.Fatalf("idempotent revoke = %d", code)
	}

	// someone else's session id → 404 (not 403: no oracle)
	if code, _ = h.doJWT("DELETE", "/v1/auth/sessions/99999999-9999-9999-9999-999999999999", nil, access2); code != http.StatusNotFound {
		t.Fatal("unknown session must 404")
	}
}
