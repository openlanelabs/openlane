"use client";

import { useEffect, useState } from "react";
import { api, logout } from "@/lib/auth";
import { useRouter } from "next/navigation";

type Run = {
  id: string;
  agent: string;
  status: string;
  model: string;
  cost_cents: number;
  input_ref: string;
  output_ref: string;
  error: string;
  approved_by: string | null;
  started_at: string;
  finished_at: string | null;
};

const statusColor = (s: string) => {
  if (s === "succeeded") return "bg-success/10 text-success";
  if (s === "running") return "bg-warn/10 text-warn";
  return "bg-danger/10 text-danger";
};

export default function AgentsPage() {
  const router = useRouter();
  const [me, setMe] = useState<{ display_name: string; role: string } | null>(null);
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [enabled, setEnabled] = useState(true);
  const [isAdmin, setIsAdmin] = useState(false);
  const [err, setErr] = useState("");

  useEffect(() => {
    (async () => {
      const meRes = await fetch("/api/auth/me");
      if (meRes.status === 401) return router.push("/login");
      if (meRes.ok) {
        const m = await meRes.json();
        setMe(m);
        setIsAdmin(m.role === "owner" || m.role === "admin");
        if (!["owner", "admin", "manager"].includes(m.role)) return router.push("/");
        const [runsRes, stRes] = await Promise.all([api("/api/agents/runs"), api("/api/agents/status")]);
        if (runsRes.ok) setRuns((await runsRes.json()).runs ?? []);
        if (stRes.ok) setEnabled((await stRes.json()).enabled ?? true);
      }
    })();
  }, [router]);

  const toggle = async () => {
    const next = !enabled;
    setEnabled(next); // optimistic
    const res = await api("/api/agents/status", {
      method: "PUT",
      body: JSON.stringify({ enabled: next }),
    });
    if (!res.ok) {
      setEnabled(!next); // rollback
      setErr("Could not update the kill switch");
    }
  };

  if (!me) return null;

  return (
    <main className="min-h-screen bg-bg p-6">
      <div className="mx-auto max-w-5xl space-y-6">
        <div className="flex items-center justify-between">
          <div>
            <h1 className="text-xl font-semibold text-text">Agents</h1>
            <p className="text-sm text-muted">Open Nitro — every run audited. We show cost, unlike Nitro black-box.</p>
          </div>
          <div className="flex items-center gap-3">
            <a href="/" className="text-xs text-muted hover:text-text">
              Dashboard
            </a>
            <button onClick={logout} className="text-xs text-muted hover:text-text">
              Sign out
            </button>
          </div>
        </div>

        <div className="flex items-center justify-between rounded-xl border border-border bg-surface p-4">
          <div>
            <p className="text-sm font-medium text-text">Agents {enabled ? "enabled" : "disabled"}</p>
            <p className="text-xs text-muted">The §15 kill switch — per workspace. Toggling off stops Guardian scans + MCP tool calls.</p>
          </div>
          {isAdmin && (
            <button
              onClick={toggle}
              className={`rounded-[10px] px-4 py-2 text-sm font-medium ${
                enabled ? "bg-danger text-white hover:opacity-90" : "bg-primary text-white hover:bg-primary-600"
              }`}
            >
              {enabled ? "Disable agents" : "Enable agents"}
            </button>
          )}
        </div>

        {err && (
          <p className="text-sm text-danger" role="alert">
            {err}
          </p>
        )}

        <div className="overflow-hidden rounded-xl border border-border">
          <table className="w-full text-sm">
            <thead className="bg-surface text-left text-xs text-muted">
              <tr>
                <th className="px-4 py-2">Agent</th>
                <th className="px-4 py-2">Status</th>
                <th className="px-4 py-2">Cost</th>
                <th className="px-4 py-2">Input → Output</th>
                <th className="px-4 py-2">When</th>
              </tr>
            </thead>
            <tbody>
              {(runs ?? []).map((r) => (
                <tr key={r.id} className="border-t border-border">
                  <td className="px-4 py-2 font-medium text-text">{r.agent}</td>
                  <td className="px-4 py-2">
                    <span className={`rounded-full px-2 py-0.5 text-xs ${statusColor(r.status)}`}>{r.status}</span>
                    {r.error && <p className="mt-1 max-w-48 truncate text-xs text-danger">{r.error}</p>}
                  </td>
                  <td className="px-4 py-2 text-muted">${(r.cost_cents / 100).toFixed(2)}</td>
                  <td className="px-4 py-2">
                    <p className="max-w-64 truncate text-xs text-text">{r.input_ref || "—"}</p>
                    <p className="max-w-64 truncate text-xs text-muted">{r.output_ref || "—"}</p>
                  </td>
                  <td className="px-4 py-2 text-xs text-muted">{r.started_at.replace("T", " ").slice(0, 19)}</td>
                </tr>
              ))}
              {runs && runs.length === 0 && (
                <tr>
                  <td colSpan={5} className="px-4 py-8 text-center text-sm text-muted">
                    No agent runs yet. The Time Guardian scans every morning (09:00 UTC).
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </div>
    </main>
  );
}
