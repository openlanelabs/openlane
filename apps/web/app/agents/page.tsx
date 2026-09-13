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

// Migration Agent (§15.2) types + helpers
type MigrRow = { row: Record<string, string>; errors?: string[] };
type Migr = {
  id: string;
  name: string;
  dest: string;
  status: string;
  stats: { total?: number; ok?: number; quarantined?: number } | null;
};
type MigrDetail = Migr & {
  mapping: { columns?: Record<string, string>; transforms?: string[] } | null;
  plain_english: string | null;
  preview: { headers?: string[]; rows?: MigrRow[] } | null;
  quarantine: { line: number; reasons: string[] }[] | null;
  result: Record<string, string>[] | null;
};

export default function AgentsPage() {
  const router = useRouter();
  const [me, setMe] = useState<{ display_name: string; role: string } | null>(null);
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [enabled, setEnabled] = useState(true);
  const [isAdmin, setIsAdmin] = useState(false);
  const [err, setErr] = useState("");
  // Migration Agent state (§15.2)
  const [migrName, setMigrName] = useState("");
  const [migrDest, setMigrDest] = useState("salesforce_accounts");
  const [migrCSV, setMigrCSV] = useState("");
  const [migrBusy, setMigrBusy] = useState(false);
  const [migrSuggest, setMigrSuggest] = useState<
    { name: string; columns: Record<string, string>; transforms: string[]; plain_english: string; preview: { headers?: string[]; rows?: MigrRow[] } | null; model: string; cost_cents: number; id: string } | null
  >(null);
  const [migrDetail, setMigrDetail] = useState<MigrDetail | null>(null);
  const [migrList, setMigrList] = useState<Migr[] | null>(null);
  const [migrErr, setMigrErr] = useState("");
  const [migrPicked, setMigrPicked] = useState("");

  const loadMigrList = async () => {
    const res = await api("/api/agents/migrations");
    if (res.ok) setMigrList(await res.json());
  };

  // Custom domains state (P1 §199)
  const [domains, setDomains] = useState<{ id: string; domain: string; verified: boolean; challenge: string }[] | null>(null);
  const [newDomain, setNewDomain] = useState("");
  const [domMsg, setDomMsg] = useState("");
  const [domBusy, setDomBusy] = useState(false);

  const loadDomains = async () => {
    const res = await api("/api/domains");
    if (res.ok) setDomains(await res.json());
  };

  const claimDomain = async (e: React.FormEvent) => {
    e.preventDefault();
    setDomBusy(true);
    setDomMsg("");
    const res = await api("/api/domains", {
      method: "PUT",
      body: JSON.stringify({ domain: newDomain }),
    });
    setDomBusy(false);
    if (res.ok) {
      setNewDomain("");
      loadDomains();
    } else {
      const p = await res.json().catch(() => null);
      setDomMsg(p?.title ?? "Claim failed");
    }
  };

  const verifyDomain = async (id: string) => {
    setDomMsg("Verifying via DNS…");
    const res = await api(`/api/domains/${id}/verify`, { method: "POST" });
    if (res.ok) {
      setDomMsg("Verified ✓");
      loadDomains();
    } else {
      const p = await res.json().catch(() => null);
      setDomMsg(p?.title ?? "Verification failed");
    }
  };

  const removeDomain = async (id: string) => {
    await api(`/api/domains/${id}`, { method: "DELETE" });
    loadDomains();
  };

  // Jira settings state (integrations)
  const [jira, setJira] = useState<{ configured: boolean; instance_url?: string } | null>(null);
  const [jiraURL, setJiraURL] = useState("");
  const [jiraPAT, setJiraPAT] = useState("");
  const [jiraSecret, setJiraSecret] = useState("");
  const [jiraMsg, setJiraMsg] = useState("");

  const loadJira = async () => {
    const res = await api("/api/integrations/jira");
    if (res.ok) {
      const c = await res.json();
      setJira(c);
      if (c.configured) setJiraURL(c.instance_url ?? "");
    }
  };

  const saveJira = async (e: React.FormEvent) => {
    e.preventDefault();
    setJiraMsg("");
    const res = await api("/api/integrations/jira", {
      method: "PUT",
      body: JSON.stringify({ instance_url: jiraURL, pat: jiraPAT, webhook_secret: jiraSecret }),
    });
    if (res.ok) {
      setJiraMsg("Saved. Status sync + task linking active for this workspace.");
      setJiraPAT("");
      setJiraSecret("");
      loadJira();
    } else {
      const p = await res.json().catch(() => null);
      setJiraMsg(p?.title ?? "Save failed");
    }
  };

  // Finance Guardian state (§15.5)
  const [fgCfg, setFgCfg] = useState<{ margin_warn_pct: number; margin_red_pct: number; min_billed: number; enabled: boolean } | null>(null);
  const [fgFlags, setFgFlags] = useState<
    | { enabled: boolean; flags: { project_id: string; name: string; billed: number; cost: number; margin_pct: number | null; severity: string; threshold: number; suggestion: string }[] }
    | null
  >(null);
  const [fgWarn, setFgWarn] = useState("");
  const [fgRed, setFgRed] = useState("");
  const [fgMsg, setFgMsg] = useState("");

  const loadFinance = async () => {
    const [cRes, gRes] = await Promise.all([
      api("/api/agents/finance/config"),
      api("/api/agents/finance/guardian"),
    ]);
    if (cRes.ok) setFgCfg(await cRes.json());
    if (gRes.ok) setFgFlags(await gRes.json());
  };

  const saveFinance = async (e: React.FormEvent) => {
    e.preventDefault();
    setFgMsg("");
    const body: Record<string, unknown> = {};
    if (fgWarn !== "") body.margin_warn_pct = Number(fgWarn);
    if (fgRed !== "") body.margin_red_pct = Number(fgRed);
    const res = await api("/api/agents/finance/config", {
      method: "PUT",
      body: JSON.stringify(body),
    });
    if (res.ok) {
      setFgCfg(await res.json());
      setFgMsg("Thresholds saved.");
      loadFinance();
    } else {
      const p = await res.json().catch(() => null);
      setFgMsg(p?.title ?? "Save failed");
    }
  };

  const runSuggest = async (e: React.FormEvent) => {
    e.preventDefault();
    setMigrBusy(true);
    setMigrErr("");
    setMigrSuggest(null);
    setMigrDetail(null);
    const res = await api("/api/agents/migrations/suggest", {
      method: "POST",
      body: JSON.stringify({ name: migrName, dest: migrDest, csv_text: migrCSV }),
    });
    setMigrBusy(false);
    if (res.ok) {
      setMigrSuggest(await res.json());
      loadMigrList();
    } else {
      const p = await res.json().catch(() => null);
      setMigrErr(p?.title ?? "Suggest failed");
    }
  };

  const approveMigr = async () => {
    if (!migrSuggest) return;
    setMigrBusy(true);
    const res = await api(`/api/agents/migrations/${migrSuggest.id}/approve`, { method: "POST" });
    setMigrBusy(false);
    if (res.ok) {
      const out = await res.json();
      setMigrPicked(
        `done — ${out.stats.ok} imported, ${out.stats.quarantined} quarantined of ${out.stats.total}`
      );
      loadMigrList();
    } else {
      const p = await res.json().catch(() => null);
      setMigrErr(p?.title ?? "Approve failed");
    }
  };
  const [llm, setLlm] = useState<{
    configured: boolean;
    provider?: string;
    base_url?: string;
    cheap_model?: string;
    smart_model?: string;
  } | null>(null);
  const [fProvider, setFProvider] = useState("openai");
  const [fBase, setFBase] = useState("https://api.openai.com");
  const [fCheap, setFCheap] = useState("");
  const [fSmart, setFSmart] = useState("");
  const [fKey, setFKey] = useState("");
  const [llmMsg, setLlmMsg] = useState("");
  const [llmBusy, setLlmBusy] = useState(false);

  useEffect(() => {
    (async () => {
      const meRes = await fetch("/api/auth/me");
      if (meRes.status === 401) return router.push("/login");
      if (meRes.ok) {
        const m = await meRes.json();
        setMe(m);
        setIsAdmin(m.role === "owner" || m.role === "admin");
        if (!["owner", "admin", "manager"].includes(m.role)) return router.push("/");
        const [runsRes, stRes, llmRes] = await Promise.all([
          api("/api/agents/runs"),
          api("/api/agents/status"),
          api("/api/agents/llm"),
        ]);
        if (runsRes.ok) setRuns((await runsRes.json()).runs ?? []);
        if (stRes.ok) setEnabled((await stRes.json()).enabled ?? true);
        if (llmRes.ok) {
          const c = await llmRes.json();
          setLlm(c);
          if (c.configured) {
            setFProvider(c.provider ?? "openai");
            setFBase(c.base_url ?? "");
            setFCheap(c.cheap_model ?? "");
            setFSmart(c.smart_model ?? "");
          }
        }
        loadMigrList();
        loadFinance();
        loadJira();
        loadDomains();
      }
    })();
  }, [router]);

  const saveLLM = async (e: React.FormEvent) => {
    e.preventDefault();
    setLlmBusy(true);
    setLlmMsg("");
    const res = await api("/api/agents/llm", {
      method: "PUT",
      body: JSON.stringify({
        provider: fProvider,
        base_url: fBase,
        cheap_model: fCheap,
        smart_model: fSmart,
        api_key: fKey,
      }),
    });
    setLlmBusy(false);
    if (res.ok) {
      setLlmMsg("Saved. Key sealed — narration is live.");
      setFKey("");
      const c = await (await api("/api/agents/llm")).json();
      setLlm(c);
    } else {
      const p = await res.json().catch(() => null);
      setLlmMsg(p?.title ?? "Save failed");
    }
  };

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

        <section aria-label="BYO-LLM provider" className="rounded-xl border border-border bg-surface p-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div>
              <p className="text-sm font-medium text-text">BYO-LLM provider</p>
              <p className="text-xs text-muted">
                OpenAI / Anthropic / Ollama (§15). Model router — cheap for nudges, smart for drafts. Key sealed AES-256-GCM, never returned.
              </p>
            </div>
            {llm && (
              <span className={`rounded-full px-2 py-0.5 text-xs ${llm.configured ? "bg-success/10 text-success" : "bg-warn/10 text-warn"}`}>
                {llm.configured ? `${llm.provider} — ${llm.cheap_model} / ${llm.smart_model}` : "not configured"}
              </span>
            )}
          </div>
          {isAdmin && (
            <form onSubmit={saveLLM} className="mt-3 grid gap-2 sm:grid-cols-2">
              <label className="text-xs text-muted">
                Provider
                <select
                  value={fProvider}
                  onChange={(e) => setFProvider(e.target.value)}
                  className="mt-1 w-full rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text"
                >
                  <option value="openai">OpenAI / Azure (openai-compatible)</option>
                  <option value="anthropic">Anthropic</option>
                  <option value="ollama">Ollama (self-hosted)</option>
                </select>
              </label>
              <label className="text-xs text-muted">
                Base URL
                <input
                  value={fBase}
                  onChange={(e) => setFBase(e.target.value)}
                  placeholder="https://api.openai.com"
                  className="mt-1 w-full rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text"
                />
              </label>
              <label className="text-xs text-muted">
                Cheap model (nudges, summaries)
                <input
                  value={fCheap}
                  onChange={(e) => setFCheap(e.target.value)}
                  placeholder="gpt-4o-mini"
                  className="mt-1 w-full rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text"
                />
              </label>
              <label className="text-xs text-muted">
                Smart model (docs, migration)
                <input
                  value={fSmart}
                  onChange={(e) => setFSmart(e.target.value)}
                  placeholder="gpt-4o"
                  className="mt-1 w-full rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text"
                />
              </label>
              <label className="text-xs text-muted">
                API key {llm?.configured ? "(leave blank to keep)" : ""}
                <input
                  type="password"
                  value={fKey}
                  onChange={(e) => setFKey(e.target.value)}
                  placeholder={fProvider === "ollama" ? "not needed for ollama" : "sk-…"}
                  className="mt-1 w-full rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text"
                />
              </label>
              <button
                type="submit"
                disabled={llmBusy}
                className="mt-1 self-end rounded-[10px] bg-primary px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
              >
                {llmBusy ? "Saving…" : "Save"}
              </button>
              {llmMsg && <p className="col-span-full text-xs text-muted">{llmMsg}</p>}
            </form>
          )}
        </section>

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

        <section aria-label="Migration Agent" className="rounded-xl border border-border bg-surface p-4">
          <div>
            <p className="text-sm font-medium text-text">Migration Agent</p>
            <p className="text-xs text-muted">
              CSV in → the agent suggests a column mapping + transforms in plain English (§15.2) → you review the preview + errors → Approve runs the full migration deterministically, quarantining bad rows. The LLM never touches your data — it only picks the mapping.
            </p>
          </div>
          <form onSubmit={runSuggest} className="mt-3 space-y-2">
            <div className="flex flex-wrap gap-2">
              <input
                value={migrName}
                onChange={(e) => setMigrName(e.target.value)}
                placeholder="Migration name (e.g. legacy accounts)"
                aria-label="Migration name"
                className="w-56 rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted"
              />
              <select
                value={migrDest}
                onChange={(e) => setMigrDest(e.target.value)}
                aria-label="Destination"
                className="rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text"
              >
                <option value="salesforce_accounts">Salesforce Accounts</option>
                <option value="hubspot_contacts">HubSpot Contacts</option>
                <option value="generic">Generic</option>
              </select>
            </div>
            <textarea
              value={migrCSV}
              onChange={(e) => setMigrCSV(e.target.value)}
              placeholder={"Paste CSV (header row + up to 2000 rows)…\ncompany,phone,signup_date\nAcme,+1 415 555 0100,03/15/2026"}
              aria-label="CSV data"
              rows={5}
              className="w-full rounded-[10px] border border-border bg-bg px-3 py-2 font-mono text-xs text-text placeholder:text-muted"
            />
            <button
              disabled={migrBusy || !migrName || !migrCSV}
              className="rounded-[10px] bg-primary px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
            >
              {migrBusy ? "Suggesting…" : "Suggest mapping"}
            </button>
          </form>
          {migrErr && (
            <p className="mt-2 text-sm text-danger" role="alert">
              {migrErr}
            </p>
          )}
          {migrSuggest && (
            <div className="mt-3 space-y-3 rounded-[10px] border border-border bg-bg p-3" role="status">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <p className="text-sm text-text">
                  {migrSuggest.name} · <span className="text-xs text-muted">{migrSuggest.model} · {migrSuggest.cost_cents}¢</span>
                </p>
                <button
                  onClick={approveMigr}
                  disabled={migrBusy}
                  className="rounded-[10px] bg-primary px-4 py-1.5 text-xs font-medium text-white hover:bg-primary-600 disabled:opacity-50"
                >
                  Approve + run full migration
                </button>
              </div>
              {migrSuggest.plain_english && (
                <p className="text-sm text-text">{migrSuggest.plain_english}</p>
              )}
              <div className="flex flex-wrap gap-1.5">
                {Object.entries(migrSuggest.columns).map(([csvCol, field]) => (
                  <span key={csvCol} className="rounded-full bg-surface px-2 py-0.5 text-xs text-text">
                    {csvCol} → <span className="font-medium">{field}</span>
                  </span>
                ))}
                {migrSuggest.transforms.length > 0 && (
                  <span className="rounded-full bg-primary/10 px-2 py-0.5 text-xs text-primary">
                    transforms: {migrSuggest.transforms.join(", ")}
                  </span>
                )}
              </div>
              {migrSuggest.preview?.rows && migrSuggest.preview.rows.some((r) => (r.errors?.length ?? 0) > 0) && (
                <div className="rounded-[10px] border border-warn/40 bg-warn/10 p-2">
                  <p className="text-xs font-semibold text-warn">Rows with validation errors (will be quarantined on run):</p>
                  <ul className="mt-1 list-disc pl-4 text-xs text-text">
                    {migrSuggest.preview.rows
                      .filter((r) => (r.errors?.length ?? 0) > 0)
                      .map((r, i) => (
                        <li key={i}>{r.errors?.join("; ")}</li>
                      ))}
                  </ul>
                </div>
              )}
              {migrPicked && (
                <p className="text-sm text-success">{migrPicked}</p>
              )}
            </div>
          )}
          {migrList && migrList.length > 0 && (
            <ul className="mt-3 space-y-1">
              {migrList.map((m) => (
                <li key={m.id} className="flex items-center justify-between gap-2 text-sm">
                  <span className="text-text">{m.name}</span>
                  <span className="text-xs text-muted">
                    {m.status}
                    {m.stats && m.status === "done" && m.stats && m.stats.ok != null && (
                      ` · ${m.stats.ok} ok, ${m.stats.quarantined} quarantined`
                    )}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </section>

        <section aria-label="Finance Guardian" className="rounded-xl border border-border bg-surface p-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div>
              <p className="text-sm font-medium text-text">Finance Guardian</p>
              <p className="text-xs text-muted">
                Margin thresholds — it flags projects drifting underwater, it never touches money without you (§15.5).
              </p>
            </div>
            {fgCfg && (
              <span className={`rounded-full px-2 py-0.5 text-xs ${fgCfg.enabled ? "bg-success/10 text-success" : "bg-warn/10 text-warn"}`}>
                {fgCfg.enabled ? `warn ${fgCfg.margin_warn_pct}% / red ${fgCfg.margin_red_pct}%` : "disabled"}
              </span>
            )}
          </div>
          {fgFlags && fgFlags.flags.length > 0 && (
            <ul className="mt-3 space-y-2">
              {fgFlags.flags.map((f) => (
                <li key={f.project_id} className="rounded-[10px] border border-border bg-bg px-3 py-2">
                  <div className="flex items-center justify-between gap-2">
                    <p className="text-sm text-text">
                      {f.name}{" "}
                      <span className={`rounded-full px-2 py-0.5 text-xs ${f.severity === "red" ? "bg-danger/10 text-danger" : "bg-warn/10 text-warn"}`}>
                        {f.severity} · {f.margin_pct}% margin
                      </span>
                    </p>
                    <span className="text-xs text-muted">billed {f.billed} / cost {f.cost}</span>
                  </div>
                  <p className="mt-1 text-xs text-muted">{f.suggestion}</p>
                </li>
              ))}
            </ul>
          )}
          {fgFlags && fgFlags.flags.length === 0 && fgCfg?.enabled && (
            <p className="mt-2 text-xs text-success">No projects under the thresholds.</p>
          )}
          {isAdmin && fgCfg && (
            <form onSubmit={saveFinance} className="mt-3 flex flex-wrap items-center gap-2">
              <input
                value={fgWarn}
                onChange={(e) => setFgWarn(e.target.value)}
                placeholder={`warn % (now ${fgCfg.margin_warn_pct})`}
                aria-label="Warn threshold percent"
                inputMode="numeric"
                className="w-40 rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted"
              />
              <input
                value={fgRed}
                onChange={(e) => setFgRed(e.target.value)}
                placeholder={`red % (now ${fgCfg.margin_red_pct})`}
                aria-label="Red threshold percent"
                inputMode="numeric"
                className="w-40 rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted"
              />
              <button
                disabled={fgWarn === "" && fgRed === ""}
                className="rounded-[10px] bg-primary px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
              >
                Save thresholds
              </button>
              {fgMsg && <span className="text-xs text-muted">{fgMsg}</span>}
            </form>
          )}

        {isAdmin && (
          <section aria-label="Jira" className="rounded-xl border border-border bg-surface p-4">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div>
                <p className="text-sm font-medium text-text">Jira</p>
                <p className="text-xs text-muted">
                  Status sync (Jira wins v1) + task linking. PAT sealed AES-256-GCM server-side, never returned.
                </p>
              </div>
              {jira && (
                <span className={`rounded-full px-2 py-0.5 text-xs ${jira.configured ? "bg-success/10 text-success" : "bg-warn/10 text-warn"}`}>
                  {jira.configured ? jira.instance_url : "not configured"}
                </span>
              )}
            </div>
            <form onSubmit={saveJira} className="mt-3 space-y-2">
              <input
                value={jiraURL}
                onChange={(e) => setJiraURL(e.target.value)}
                placeholder="https://yourcompany.atlassian.net"
                aria-label="Jira instance URL"
                className="w-full rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted"
              />
              <div className="flex flex-wrap gap-2">
                <input
                  type="password"
                  value={jiraPAT}
                  onChange={(e) => setJiraPAT(e.target.value)}
                  placeholder={jira?.configured ? "PAT (leave blank to keep)" : "Personal access token"}
                  aria-label="Jira PAT"
                  className="min-w-0 flex-1 rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted"
                />
                <input
                  type="password"
                  value={jiraSecret}
                  onChange={(e) => setJiraSecret(e.target.value)}
                  placeholder={jira?.configured ? "Webhook secret (blank keeps)" : "Webhook secret (min 16 chars)"}
                  aria-label="Webhook secret"
                  className="min-w-0 flex-1 rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted"
                />
                <button
                  disabled={!jiraURL || (!jiraPAT && !jiraSecret && !jira?.configured)}
                  className="self-stretch rounded-[10px] bg-primary px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
                >
                  Save
                </button>
              </div>
              {jiraMsg && <p className="text-xs text-muted">{jiraMsg}</p>}
            </form>
          </section>
        )}

        {isAdmin && (
          <section aria-label="Custom domains" className="rounded-xl border border-border bg-surface p-4">
            <div>
              <p className="text-sm font-medium text-text">Custom domains</p>
              <p className="text-xs text-muted">
                Branded portal hosts (onboarding.acme.com). Add the TXT record we show you, verify, then point DNS at your OpenLane host — TLS is handled by your reverse proxy (§199/§229).
              </p>
            </div>
            <form onSubmit={claimDomain} className="mt-3 flex gap-2">
              <input
                value={newDomain}
                onChange={(e) => setNewDomain(e.target.value)}
                placeholder="onboarding.acme.com"
                aria-label="New domain"
                className="min-w-0 flex-1 rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted"
              />
              <button
                disabled={domBusy || !newDomain.trim()}
                className="rounded-[10px] bg-primary px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
              >
                {domBusy ? "Claiming…" : "Claim"}
              </button>
            </form>
            {domMsg && <p className="mt-1 text-xs text-muted">{domMsg}</p>}
            {domains && domains.length > 0 && (
              <ul className="mt-3 space-y-2">
                {domains.map((d) => (
                  <li key={d.id} className="rounded-[10px] border border-border bg-bg px-3 py-2">
                    <div className="flex items-center justify-between gap-2">
                      <p className="text-sm text-text">
                        {d.domain}{" "}
                        <span className={`rounded-full px-2 py-0.5 text-xs ${d.verified ? "bg-success/10 text-success" : "bg-warn/10 text-warn"}`}>
                          {d.verified ? "verified" : "unverified"}
                        </span>
                      </p>
                      <span className="flex gap-2">
                        {!d.verified && (
                          <button
                            onClick={() => verifyDomain(d.id)}
                            className="rounded-[10px] bg-primary px-3 py-1 text-xs font-medium text-white hover:bg-primary-600"
                          >
                            Verify
                          </button>
                        )}
                        <button
                          onClick={() => removeDomain(d.id)}
                          className="rounded-[10px] border border-border px-3 py-1 text-xs text-muted hover:text-text"
                        >
                          Remove
                        </button>
                      </span>
                    </div>
                    {!d.verified && (
                      <p className="mt-1 break-all font-mono text-xs text-muted">
                        TXT _openlane-challenge.{d.domain} → {d.challenge}
                      </p>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </section>
        )}
        </section>
      </div>
    </main>
  );
}
