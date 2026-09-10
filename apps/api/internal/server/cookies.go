package server

// Cookie transport for API sessions (issue #56, spec §17-6): the refresh
// token is ALSO set in an httpOnly cookie with a double-submit CSRF token
// in a JS-readable sibling. Body-token clients keep the JSON field — both
// paths coexist and are tested.
//
// CSRF model: cookie-authenticated state changes (refresh, logout) require
// the X-OpenLane-CSRF header to match the openlane_csrf cookie. Bearer or
// body-token requests are structurally immune (no ambient cookie involved)
// and skip the check.
//
// The bundled web app still refreshes via its same-origin proxy with the
// body field (proxy server-to-server fetch carries no browser cookies —
// CSRF-immune by construction). Switching the web to cookie transport
// means proxying set-cookie through; deliberately P1.

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
)

const (
	refreshCookie = "openlane_refresh"
	csrfCookie    = "openlane_csrf"
	csrfHeader    = "X-OpenLane-CSRF"
)

// Cookies are always Secure: browsers treat http://localhost as a secure
// context, so dev keeps working; any other plain-http dev host loses only
// the COOKIE transport (body tokens still authenticate) — never a prod
// footgun. G124-proof by construction, no dynamic flags.
func setAuthCookies(w http.ResponseWriter, refresh, csrf string) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookie,
		Value:    refresh,
		Path:     "/v1/auth",
		MaxAge:   30 * 24 * 3600,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	// JS must read this for the double-submit header — HttpOnly here would
	// defeat CSRF protection entirely. The paired header check (checkCSRF)
	// is what makes the value safe to expose: it is not a credential,
	// only a per-session nonce.
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- deliberate: double-submit token
		Name:     csrfCookie,
		Value:    csrf,
		Path:     "/",
		MaxAge:   30 * 24 * 3600,
		HttpOnly: false,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearAuthCookies(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: refreshCookie, Path: "/v1/auth", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	// #nosec G124 -- csrf token is not a credential (see setAuthCookies)
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Path: "/", MaxAge: -1, Secure: true, SameSite: http.SameSiteLaxMode})
}

func newCSRFToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// csrfFromRequest: the double-submit pair — header vs cookie.
func csrfFromRequest(r *http.Request) (header, cookieV string) {
	c, err := r.Cookie(csrfCookie)
	if err == nil {
		cookieV = c.Value
	}
	return r.Header.Get(csrfHeader), cookieV
}

// checkCSRF: for cookie-authenticated flows only. Returns true when this
// request is NOT cookie-authenticated (bearer/body — no CSRF surface).
func checkCSRF(r *http.Request, viaCookie bool) bool {
	if !viaCookie {
		return true
	}
	h, c := csrfFromRequest(r)
	return h != "" && h == c
}
