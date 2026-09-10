"use client";

import { useEffect, useState } from "react";
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

function healthDot(h: string) {
  const c = h === "green" ? "bg-success" : h === "amber" ? "bg-warn" : h === "red" ? "bg-danger" : "bg-muted";
  return <span className={`inline-block h-2 w-2 rounded-full ${c}`} aria-label={`health: ${h}`} />;
}

export default function Dashboard() {
  const router = useRouter();
  const [me, setMe] = useState<{ display_name: string; workspace_name: string } | null>(null);
  const [projects, setProjects] = useState<Project[] | null>(null);
  const [filter, setFilter] = useState<(typeof FILTERS)[number]>("all");
  const [err, setErr] = useState("");

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
          <button
            onClick={async () => { await logout(); router.push("/login"); }}
            className="text-xs text-muted hover:text-text"
          >
            Sign out
          </button>
        </div>
      </header>

      <main className="mx-auto max-w-3xl space-y-4 p-4">
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
