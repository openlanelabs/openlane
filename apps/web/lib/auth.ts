"use client";

// P0 token storage: localStorage + Bearer header. httpOnly cookie lands with
// the CSRF/security hardening pass (§17-6) — the refresh rotation already
// bounds blast radius.
const ACCESS = "openlane_access";
const REFRESH = "openlane_refresh";

export function getAccess(): string | null {
  if (typeof window === "undefined") return null;
  return localStorage.getItem(ACCESS);
}

export function setSession(access: string, refresh: string) {
  localStorage.setItem(ACCESS, access);
  localStorage.setItem(REFRESH, refresh);
}

export function clearSession() {
  localStorage.removeItem(ACCESS);
  localStorage.removeItem(REFRESH);
}

export async function logout() {
  const refresh = localStorage.getItem(REFRESH);
  clearSession();
  if (refresh) {
    void fetch("/api/auth/logout", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: refresh }),
    }).catch(() => {});
  }
}

// api(): same-origin proxy fetch with Bearer + one auto-refresh retry.
export async function api(path: string, init?: RequestInit): Promise<Response> {
  const doFetch = () =>
    fetch(`/api${path}`, {
      ...init,
      headers: {
        ...init?.headers,
        ...(getAccess() ? { Authorization: `Bearer ${getAccess()}` } : {}),
      },
    });
  let res = await doFetch();
  if (res.status === 401 && localStorage.getItem(REFRESH)) {
    const ok = await tryRefresh();
    if (ok) res = await doFetch();
  }
  return res;
}

async function tryRefresh(): Promise<boolean> {
  const refresh = localStorage.getItem(REFRESH);
  if (!refresh) return false;
  const res = await fetch("/api/auth/refresh", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ refresh_token: refresh }),
  });
  if (!res.ok) {
    clearSession();
    return false;
  }
  const data = await res.json();
  setSession(data.access_token, data.refresh_token);
  return true;
}
