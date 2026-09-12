"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { api, logout } from "@/lib/auth";

type Project = {
  id: string;
  customer_id: string;
  name: string;
  status: string;
  health: string;
  progress_pct: number;
  target_go_live: string | null;
};

const FILTERS = ["all", "active", "draft", "completed"] as const;

type PendingTime = {
  id: string;
  project_id: string;
  minutes: number;
  started_at: string;
  note: string;
};

type PortfolioMargins = {
  projects: {
    project_id: string;
    logged_hours: number;
    cost: number;
    billed: number;
    margin: number | null;
  }[];
  totals: {
    logged_hours: number;
    cost: number;
    billed: number;
    margin: number | null;
  };
};

function healthDot(h: string) {
  const c = h === "green" ? "bg-success" : h === "amber" ? "bg-warn" : h === "red" ? "bg-danger" : "bg-muted";
  return <span className={`inline-block h-2 w-2 rounded-full ${c}`} aria-label={`health: ${h}`} />;
}

export default function Dashboard() {
  const router = useRouter();
  const [me, setMe] = useState<{ display_name: string; workspace_name: string; role: string } | null>(null);
  const [projects, setProjects] = useState<Project[] | null>(null);
  const [filter, setFilter] = useState<(typeof FILTERS)[number]>("all");
  const [err, setErr] = useState("");
  const [pending, setPending] = useState<PendingTime[] | null>(null);
  const [rejecting, setRejecting] = useState<string | null>(null); // entry id with open reason box
  const [reason, setReason] = useState("");
  const [margins, setMargins] = useState<PortfolioMargins | null>(null);

  useEffect(() => {
    (async () => {
      const meRes = await api("/me");
      if (meRes.status === 401) {
        router.push("/login");
        return;
      }
      if (meRes.ok) setMe(await meRes.json());
    })();
  }, [router]);

  useEffect(() => {
    if (!me) return;
    (async () => {
      const q = filter === "all" ? "" : `?status=${filter}`;
      const res = await api(`/projects${q}`);
      if (res.ok) setProjects(await res.json());
      else setErr("Couldn't load projects.");
    })();
  }, [me, filter]);

  const isManager = ["owner", "admin", "manager"].includes(me?.role ?? "");
  const loadPending = useCallback(async () => {
    const res = await api("/time/pending");
    if (res.ok) setPending(await res.json());
    else setPending(null); // 403 or failure: hide the panel
  }, []);
  useEffect(() => {
    if (!me) return;
    if (isManager) {
      void loadPending();
      void api("/margins").then(async (res) => {
        if (res.ok) setMargins(await res.json());
        else setMargins(null);
      });
    } else {
      setPending(null);
      setMargins(null);
    }
  }, [me, isManager, loadPending]);

  async function decideTime(id: string, status: "approved" | "rejected", why?: string) {
    setErr("");
    const prev = pending;
    if (status === "approved") {
      setPending((ps) => ps?.filter((x) => x.id !== id) ?? ps); // optimistic
    }
    const res = await api(`/time/${id}/${status === "approved" ? "approve" : "reject"}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ reason: why ?? "" }),
    });
    if (!res.ok) {
      if (prev && status === "approved") setPending(prev); // rollback
      const problem = await res.json().catch(() => ({ title: "" }));
      setErr(problem.title || "Couldn't update the entry.");
      return;
    }
    setRejecting(null);
    setReason("");
    await loadPending();
  }

  if (!me) {
    return (
      <main className="min-h-screen bg-bg p-6">
        <div className="mx-auto max-w-3xl space-y-3">
          {[0, 1, 2].map((i) => (
            <div key={i} className="h-16 animate-pulse rounded-[10px] border border-border bg-surface" />
          ))}
        </div>
      </main>
    );
  }

  return (
    <div className="min-h-screen bg-bg">
      <header className="border-b border-border bg-surface px-4 py-3">
        <div className="mx-auto flex max-w-3xl items-center justify-between">
          <div>
            <p className="text-sm font-semibold text-text">{me.workspace_name}</p>
            <p className="text-xs text-muted">{me.display_name}</p>
          </div>
          <div className="flex items-center gap-4">
            <a href="/capacity" className="text-xs text-muted hover:text-text">
              Capacity
            </a>
            {isManager && (
              <a href="/agents" className="text-xs text-muted hover:text-text">
                Agents
              </a>
            )}
            <button
              onClick={async () => { await logout(); router.push("/login"); }}
              className="text-xs text-muted hover:text-text"
            >
              Sign out
            </button>
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-3xl space-y-4 p-4">
        {margins !== null && margins.totals.billed > 0 && (
          <section aria-label="Portfolio margins" className="rounded-[10px] border border-border bg-surface p-4">
            <div className="flex flex-wrap items-center gap-x-6 gap-y-1 text-xs text-muted">
              <span>
                Billed <strong className="text-text">${Math.round(margins.totals.billed).toLocaleString()}</strong>
              </span>
              <span>
                Cost <strong className="text-text">${Math.round(margins.totals.cost).toLocaleString()}</strong>
              </span>
              <span>
                Logged <strong className="text-text">{Math.round(margins.totals.logged_hours)}h</strong>
              </span>
              <span
                className={`rounded-[10px] px-2 py-0.5 font-medium ${
                  margins.totals.margin !== null && margins.totals.margin >= 0.5
                    ? "bg-success/10 text-success"
                    : margins.totals.margin !== null && margins.totals.margin >= 0.2
                      ? "bg-warn/10 text-warn"
                      : "text-danger"
                }`}
              >
                Margin {margins.totals.margin !== null ? `${Math.round(margins.totals.margin * 100)}%` : "—"}
              </span>
            </div>
          </section>
        )}

        {pending !== null && pending.length > 0 && (
          <section aria-label="Pending time approvals" className="rounded-[10px] border border-border bg-surface p-4">
            <h2 className="text-sm font-semibold text-text">Pending time ({pending.length})</h2>
            <ul className="mt-2 space-y-2">
              {pending.map((e) => (
                <li key={e.id} className="rounded-[10px] border border-border bg-bg p-2">
                  <div className="flex items-center justify-between gap-2">
                    <div className="min-w-0">
                      <p className="truncate text-sm text-text">{Math.round(e.minutes / 60 * 10) / 10}h · {new Date(e.started_at).toLocaleDateString()}</p>
                      {e.note && <p className="truncate text-xs text-muted">{e.note}</p>}
                    </div>
                    {rejecting !== e.id ? (
                      <div className="flex shrink-0 gap-1">
                        <button
                          onClick={() => decideTime(e.id, "approved")}
                          className="rounded-[10px] border border-success px-2 py-1 text-xs font-medium text-success hover:bg-success hover:text-white"
                        >
                          Approve
                        </button>
                        <button
                          onClick={() => { setRejecting(e.id); setReason(""); }}
                          className="rounded-[10px] border border-border px-2 py-1 text-xs text-muted"
                        >
                          Reject
                        </button>
                      </div>
                    ) : (
                      <form
                        onSubmit={(ev) => { ev.preventDefault(); if (reason.trim()) void decideTime(e.id, "rejected", reason.trim()); }}
                        className="flex shrink-0 items-center gap-1"
                      >
                        <input
                          value={reason}
                          onChange={(ev) => setReason(ev.target.value)}
                          placeholder="Reason"
                          aria-label={`Reject reason for entry`}
                          className="w-36 rounded-[10px] border border-border bg-surface px-2 py-1 text-xs text-text"
                          autoFocus
                        />
                        <button disabled={!reason.trim()} className="rounded-[10px] bg-danger px-2 py-1 text-xs font-medium text-white disabled:opacity-50">
                          Reject
                        </button>
                        <button type="button" onClick={() => setRejecting(null)} className="rounded-[10px] px-1 py-1 text-xs text-muted">
                          ✕
                        </button>
                      </form>
                    )}
                  </div>
                </li>
              ))}
            </ul>
          </section>
        )}

        <div className="flex items-center gap-2" role="tablist" aria-label="Filter projects">
          {FILTERS.map((f) => (
            <button
              key={f}
              role="tab"
              aria-selected={filter === f}
              onClick={() => setFilter(f)}
              className={`rounded-[10px] px-3 py-1 text-xs font-medium capitalize ${
                filter === f ? "bg-primary text-white" : "border border-border bg-surface text-muted"
              }`}
            >
              {f}
            </button>
          ))}
        </div>

        {err && <p className="text-sm text-danger" role="alert">{err}</p>}

        {projects === null ? (
          <div className="space-y-3">
            {[0, 1, 2].map((i) => (
              <div key={i} className="h-20 animate-pulse rounded-[10px] border border-border bg-surface" />
            ))}
          </div>
        ) : projects.length === 0 ? (
          <div className="rounded-[10px] border border-dashed border-border bg-surface p-8 text-center">
            <p className="text-sm font-medium text-text">No projects yet</p>
            <p className="mt-1 text-xs text-muted">Create your first onboarding from a template — or import a CSV from your old tool.</p>
          </div>
        ) : (
          <ul className="space-y-3">
            {projects.map((p) => (
              <li key={p.id}>
                <Link
                  href={`/projects/${p.id}`}
                  className="block rounded-[10px] border border-border bg-surface p-4 hover:border-primary"
                >
                  <div className="flex items-center justify-between gap-3">
                    <div className="flex min-w-0 items-center gap-2">
                      {healthDot(p.health)}
                      <span className="truncate text-sm font-medium text-text">{p.name}</span>
                    </div>
                    <span className="shrink-0 rounded-[10px] border border-border px-2 py-0.5 text-xs text-muted capitalize">
                      {p.status}
                    </span>
                    {(() => {
                      const m = margins?.projects.find((x) => x.project_id === p.id);
                      if (!m || m.margin === null) return null;
                      const pct = Math.round(m.margin * 100);
                      return (
                        <span
                          className={`shrink-0 rounded-[10px] px-2 py-0.5 text-xs font-medium ${
                            m.margin >= 0.5 ? "bg-success/10 text-success" : m.margin >= 0.2 ? "bg-warn/10 text-warn" : "text-danger"
                          }`}
                          title={`billed $${Math.round(m.billed)} · cost $${Math.round(m.cost)}`}
                        >
                          {pct}% margin
                        </span>
                      );
                    })()}
                  </div>
                  <div className="mt-3 flex items-center gap-3">
                    <div className="h-2 flex-1 overflow-hidden rounded-full bg-border" role="progressbar" aria-valuenow={p.progress_pct} aria-valuemin={0} aria-valuemax={100}>
                      <div className="h-full rounded-full bg-primary transition-all duration-150" style={{ width: `${p.progress_pct}%` }} />
                    </div>
                    <span className="text-xs text-muted">{p.progress_pct}%</span>
                  </div>
                </Link>
              </li>
            ))}
          </ul>
        )}
      </main>
    </div>
  );
}
