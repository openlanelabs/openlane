"use client";

import { useCallback, useEffect, useState } from "react";
import { useParams, useRouter } from "next/navigation";
import { api } from "@/lib/auth";

type Task = {
  id: string;
  title: string;
  status: string;
  owner_type: string;
  customer_visible: boolean;
  required: boolean;
  due_at: string | null;
};

type Approval = {
  id: string;
  title: string;
  status: string;
  decision_comment: string;
};

type Doc = {
  id: string;
  title: string;
  customer_visible: boolean;
  latest_version: number;
  updated_at: string;
};

type CSAT = {
  responses: { score: number; comment: string }[];
  average_score: number | null;
  response_rate: number;
};

type Budget = {
  budget_hours: number | null;
  billing_type: string;
  logged_minutes: number;
  logged_hours: number;
  pct: number | null;
  status: "unbudgeted" | "ok" | "warn50" | "warn80" | "over";
};

type Invoice = {
  id: string;
  status: "draft" | "sent" | "paid" | "void";
  period_start: string;
  period_end: string;
  subtotal: string;
};

const NEXT: Record<string, string> = {
  todo: "in_progress",
  in_progress: "review",
  review: "done",
};

export default function ProjectDetail() {
  const router = useRouter();
  const { id } = useParams<{ id: string }>();
  const [project, setProject] = useState<{ name: string; status: string; health: string; progress_pct: number } | null>(null);
  const [tasks, setTasks] = useState<Task[] | null>(null);
  const [approvals, setApprovals] = useState<Approval[] | null>(null);
  const [docs, setDocs] = useState<Doc[] | null>(null);
  const [csat, setCSAT] = useState<CSAT | null>(null);
  const [budget, setBudget] = useState<Budget | null>(null);
  const [invoices, setInvoices] = useState<Invoice[] | null>(null);
  const [budgetHours, setBudgetHours] = useState("");
  const [billingType, setBillingType] = useState("");
  const [approvalTitle, setApprovalTitle] = useState("");
  const [docTitle, setDocTitle] = useState("");
  const [docBody, setDocBody] = useState("");
  const [newTitle, setNewTitle] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  const load = useCallback(async () => {
    const results = await Promise.allSettled([
      api(`/projects/${id}`),
      api(`/projects/${id}/tasks`),
      api(`/projects/${id}/approvals`),
      api(`/projects/${id}/docs`),
      api(`/projects/${id}/csat`),
      api(`/projects/${id}/budget`),
      api(`/invoices`),
    ]);
    if (results[0].status === "fulfilled" && results[0].value.status === 401) { router.push("/login"); return; }
    if (results[0].status === "fulfilled" && results[0].value.ok) setProject(await results[0].value.json());
    if (results[1].status === "fulfilled" && results[1].value.ok) setTasks(await results[1].value.json());
    else setErr("Couldn't load tasks.");
    if (results[2].status === "fulfilled" && results[2].value.ok) setApprovals(await results[2].value.json());
    if (results[3].status === "fulfilled" && results[3].value.ok) setDocs(await results[3].value.json());
    if (results[4].status === "fulfilled" && results[4].value.ok) setCSAT(await results[4].value.json());
    if (results[5].status === "fulfilled" && results[5].value.ok) {
      const b = (await results[5].value.json()) as Budget;
      setBudget(b);
      if (b.budget_hours !== null) setBudgetHours(String(b.budget_hours));
      setBillingType(b.billing_type);
    } else setBudget(null);
    if (results[6].status === "fulfilled" && results[6].value.ok) setInvoices(await results[6].value.json());
    else setInvoices([]);
  }, [id, router]);

  useEffect(() => { void load(); }, [load]);

  async function addTask(e: React.FormEvent) {
    e.preventDefault();
    const title = newTitle.trim();
    if (!title || busy) return;
    setBusy(true); setErr("");
    const res = await api(`/projects/${id}/tasks`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ title }),
    });
    setBusy(false);
    if (!res.ok) { setErr("Couldn't add the task."); return; }
    setNewTitle("");
    await load();
  }

  async function addApproval(e: React.FormEvent) {
    e.preventDefault();
    const title = approvalTitle.trim();
    if (!title) return;
    setApprovalTitle("");
    const res = await api(`/projects/${id}/approvals`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ title }),
    });
    if (res.ok) await load();
  }

  async function reopen(a: Approval) {
    const res = await api(`/approvals/${a.id}/reopen`, { method: "POST" });
    if (res.ok) await load();
  }

  async function addDoc(e: React.FormEvent) {
    e.preventDefault();
    const title = docTitle.trim();
    if (!title) return;
    setDocTitle(""); setDocBody("");
    const res = await api(`/projects/${id}/docs`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ title, content_md: docBody }),
    });
    if (res.ok) await load();
  }


  async function saveBudget(e: React.FormEvent) {
    e.preventDefault();
    setErr("");
    const hours = budgetHours.trim();
    const body: Record<string, string> = {};
    if (hours) body.budget_hours = hours;
    if (billingType) body.billing_type = billingType;
    if (Object.keys(body).length === 0) return;
    const res = await api(`/projects/${id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const problem = await res.json().catch(() => ({ title: "" }));
      setErr(problem.title || "Couldn't save the budget.");
      return;
    }
    await load();
  }

  async function moveInvoice(inv: Invoice, next: string) {
    setErr("");
    const res = await api(`/invoices/${inv.id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ status: next }),
    });
    if (!res.ok) {
      const problem = await res.json().catch(() => ({ title: "" }));
      setErr(problem.title || "That transition isn't allowed.");
      return;
    }
    await load();
  }

  async function advance(t: Task) {
    const next = NEXT[t.status];
    if (!next) return;
    setErr("");
    const prev = tasks;
    setTasks((ts) => ts?.map((x) => (x.id === t.id ? { ...x, status: next } : x)) ?? ts); // optimistic
    const res = await api(`/tasks/${t.id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ status: next }),
    });
    if (!res.ok) {
      if (prev) setTasks(prev); // rollback
      const problem = await res.json().catch(() => ({ title: "" }));
      setErr(problem.title || "That transition isn't allowed.");
    }
  }

  const due = tasks?.filter((t) => !["done", "waived"].includes(t.status)) ?? [];
  const done = tasks?.filter((t) => t.status === "done" || t.status === "waived") ?? [];

  return (
    <div className="min-h-screen bg-bg">
      <header className="border-b border-border bg-surface px-4 py-3">
        <div className="mx-auto flex max-w-3xl items-center justify-between">
          <button onClick={() => router.push("/")} className="text-xs text-muted hover:text-text">← Projects</button>
          {project && <span className="rounded-[10px] border border-border px-2 py-0.5 text-xs text-muted capitalize">{project.status}</span>}
        </div>
        {project && (
          <div className="mx-auto mt-2 max-w-3xl">
            <h1 className="text-sm font-semibold text-text">{project.name}</h1>
            <div className="mt-2 flex items-center gap-3">
              <div className="h-2 flex-1 overflow-hidden rounded-full bg-border" role="progressbar" aria-valuenow={project.progress_pct} aria-valuemin={0} aria-valuemax={100}>
                <div className="h-full rounded-full bg-primary transition-all duration-150" style={{ width: `${project.progress_pct}%` }} />
              </div>
              <span className="text-xs text-muted">{project.progress_pct}%</span>
            </div>
          </div>
        )}
      </header>

      <main className="mx-auto max-w-3xl space-y-4 p-4" aria-live="polite">
        <form onSubmit={addTask} className="flex gap-2">
          <input
            value={newTitle}
            onChange={(e) => setNewTitle(e.target.value)}
            placeholder="Add a task…"
            className="flex-1 rounded-[10px] border border-border bg-surface px-3 py-2 text-sm text-text placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-primary"
          />
          <button disabled={busy || !newTitle.trim()} className="rounded-[10px] bg-primary px-4 py-2 text-sm font-medium text-white disabled:opacity-50">
            Add
          </button>
        </form>

        {err && <p className="text-sm text-danger" role="alert">{err}</p>}

        {tasks === null ? (
          <div className="space-y-3">
            {[0, 1, 2].map((i) => <div key={i} className="h-14 animate-pulse rounded-[10px] border border-border bg-surface" />)}
          </div>
        ) : (
          <>
            <section aria-label="Open tasks" className="space-y-2">
              {due.length === 0 && done.length > 0 && (
                <div className="rounded-[10px] border border-dashed border-border bg-surface p-4 text-center text-sm text-muted">
                  All done — nice work! 🎉
                </div>
              )}
              {due.map((t) => (
                <div key={t.id} className="flex items-center justify-between gap-3 rounded-[10px] border border-border bg-surface p-3">
                  <div className="min-w-0">
                    <p className="truncate text-sm text-text">{t.title}</p>
                    <div className="mt-0.5 flex items-center gap-2">
                      <span className="text-xs capitalize text-muted">{t.status.replace("_", " ")}</span>
                      {!t.customer_visible && (
                        <span className="rounded bg-border px-1.5 py-0.5 text-[10px] font-medium text-muted">Internal</span>
                      )}
                    </div>
                  </div>
                  {NEXT[t.status] && (
                    <button
                      onClick={() => advance(t)}
                      className="shrink-0 rounded-[10px] border border-primary px-3 py-1.5 text-xs font-medium text-primary hover:bg-primary hover:text-white"
                    >
                      → {NEXT[t.status].replace("_", " ")}
                    </button>
                  )}
                </div>
              ))}
            </section>

            {done.length > 0 && (
              <details className="rounded-[10px] border border-border bg-surface">
                <summary className="cursor-pointer p-3 text-sm text-muted">Completed ({done.length})</summary>
                <ul className="space-y-1 px-3 pb-3">
                  {done.map((t) => (
                    <li key={t.id} className="flex items-center gap-2 text-sm text-muted">
                      <span aria-hidden className="text-success">✓</span>
                      <span className="line-through">{t.title}</span>
                    </li>
                  ))}
                </ul>
              </details>
            )}
          </>
        )}

        <section aria-label="Approvals" className="rounded-[10px] border border-border bg-surface p-4">
          <h2 className="text-sm font-semibold text-text">Approvals</h2>
          <form onSubmit={addApproval} className="mt-2 flex gap-2">
            <input
              value={approvalTitle}
              onChange={(e) => setApprovalTitle(e.target.value)}
              placeholder="Request an approval…"
              aria-label="Approval title"
              className="flex-1 rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted"
            />
            <button className="rounded-[10px] bg-primary px-4 py-2 text-sm font-medium text-white">Ask</button>
          </form>
          <ul className="mt-3 space-y-2">
            {approvals === null && <li className="text-xs text-muted">Loading…</li>}
            {approvals?.length === 0 && <li className="text-xs text-muted">None yet.</li>}
            {approvals?.map((a) => (
              <li key={a.id} className="flex items-center justify-between gap-2 rounded-[10px] border border-border bg-bg p-2">
                <div className="min-w-0">
                  <p className="truncate text-sm text-text">{a.title}</p>
                  <span
                    className={`text-xs ${
                      a.status === "approved"
                        ? "text-success"
                        : a.status === "changes_requested"
                          ? "text-danger"
                          : "text-muted"
                    }`}
                  >
                    {a.status.replace("_", " ")}
                  </span>
                  {a.decision_comment && <p className="truncate text-xs text-muted">“{a.decision_comment}”</p>}
                </div>
                {a.status !== "pending" && (
                  <button
                    onClick={() => reopen(a)}
                    className="shrink-0 rounded-[10px] border border-border px-2 py-1 text-xs text-muted"
                  >
                    Reopen
                  </button>
                )}
              </li>
            ))}
          </ul>
        </section>

        <section aria-label="Documents" className="rounded-[10px] border border-border bg-surface p-4">
          <h2 className="text-sm font-semibold text-text">Docs</h2>
          <form onSubmit={addDoc} className="mt-2 space-y-2">
            <input
              value={docTitle}
              onChange={(e) => setDocTitle(e.target.value)}
              placeholder="New doc title…"
              aria-label="Doc title"
              className="w-full rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted"
            />
            <textarea
              value={docBody}
              onChange={(e) => setDocBody(e.target.value)}
              placeholder="Markdown content…"
              aria-label="Doc content"
              rows={3}
              className="w-full rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted"
            />
            <button className="rounded-[10px] bg-primary px-4 py-2 text-sm font-medium text-white">Create</button>
          </form>
          <ul className="mt-3 space-y-2">
            {docs === null && <li className="text-xs text-muted">Loading…</li>}
            {docs?.length === 0 && <li className="text-xs text-muted">None yet.</li>}
            {docs?.map((d) => (
              <li key={d.id} className="flex items-center justify-between gap-2 text-sm">
                <span className="truncate text-text">{d.title}</span>
                <span className="shrink-0 text-xs text-muted">
                  v{d.latest_version}
                  {d.customer_visible ? " · shared" : " · internal"}
                </span>
              </li>
            ))}
          </ul>
        </section>

        {csat && csat.responses.length > 0 && (
          <section aria-label="Customer satisfaction" className="rounded-[10px] border border-border bg-surface p-4">
            <h2 className="text-sm font-semibold text-text">CSAT</h2>
            <p className="mt-1 text-xs text-muted">
              avg {csat.average_score !== null ? csat.average_score.toFixed(1) : "—"}/5 · response rate{" "}
              {Math.round(csat.response_rate * 100)}%
            </p>
            <ul className="mt-2 space-y-1">
              {csat.responses.map((r, i) => (
                <li key={i} className="text-sm text-text">
                  {r.score >= 4 ? "😄" : r.score === 3 ? "😐" : "😞"} {r.score}/5
                  {r.comment && <span className="text-muted"> — “{r.comment}”</span>}
                </li>
              ))}
            </ul>
          </section>
        )}

        {budget && (
          <section aria-label="Budget" className="rounded-[10px] border border-border bg-surface p-4">
            <h2 className="text-sm font-semibold text-text">Budget</h2>
            <div className="mt-2 flex items-center gap-3">
              <span
                className={`rounded-[10px] px-2 py-0.5 text-xs font-medium ${
                  budget.status === "over"
                    ? "bg-danger/10 text-danger"
                    : budget.status.startsWith("warn")
                      ? "bg-warn/10 text-warn"
                      : "bg-border text-muted"
                }`}
              >
                {budget.status === "unbudgeted" ? "No budget" : `${budget.pct}% · ${budget.status}`}
              </span>
              <span className="text-xs text-muted">
                {budget.logged_hours}h logged
                {budget.budget_hours !== null ? ` of ${budget.budget_hours}h` : ""}
              </span>
            </div>
            <form onSubmit={saveBudget} className="mt-3 flex flex-wrap items-center gap-2">
              <input
                value={budgetHours}
                onChange={(e) => setBudgetHours(e.target.value)}
                placeholder="Budget hours"
                aria-label="Budget hours"
                inputMode="numeric"
                className="w-28 rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted"
              />
              <select
                value={billingType}
                onChange={(e) => setBillingType(e.target.value)}
                aria-label="Billing type"
                className="rounded-[10px] border border-border bg-bg px-2 py-2 text-sm text-text"
              >
                <option value="tm">Time &amp; materials</option>
                <option value="fixed">Fixed</option>
                <option value="retainer">Retainer</option>
              </select>
              <button
                disabled={project?.status === "completed"}
                className="rounded-[10px] border border-border px-3 py-2 text-xs font-medium text-text disabled:opacity-50"
              >
                Save
              </button>
            </form>
          </section>
        )}

        {invoices !== null && (
          <section aria-label="Invoices" className="rounded-[10px] border border-border bg-surface p-4">
            <h2 className="text-sm font-semibold text-text">Invoices</h2>
            <ul className="mt-3 space-y-2">
              {invoices.length === 0 && <li className="text-xs text-muted">None yet.</li>}
              {invoices.map((inv) => (
                <li key={inv.id} className="flex items-center justify-between gap-2 rounded-[10px] border border-border bg-bg p-2">
                  <div className="min-w-0">
                    <p className="truncate text-sm text-text">
                      {inv.period_start} → {inv.period_end}
                    </p>
                    <span
                      className={`text-xs ${
                        inv.status === "paid"
                          ? "text-success"
                          : inv.status === "void"
                            ? "text-muted line-through"
                            : "text-warning"
                      }`}
                    >
                      {inv.status} · ${inv.subtotal}
                    </span>
                  </div>
                  <div className="flex shrink-0 gap-1">
                    {inv.status === "draft" && (
                      <button onClick={() => moveInvoice(inv, "sent")} className="rounded-[10px] border border-primary px-2 py-1 text-xs font-medium text-primary hover:bg-primary hover:text-white">
                        Mark sent
                      </button>
                    )}
                    {inv.status === "sent" && (
                      <button onClick={() => moveInvoice(inv, "paid")} className="rounded-[10px] border border-success px-2 py-1 text-xs font-medium text-success hover:bg-success hover:text-white">
                        Mark paid
                      </button>
                    )}
                    {inv.status !== "void" && inv.status !== "paid" && (
                      <button onClick={() => moveInvoice(inv, "void")} className="rounded-[10px] border border-border px-2 py-1 text-xs text-muted">
                        Void
                      </button>
                    )}
                  </div>
                </li>
              ))}
            </ul>
          </section>
        )}
      </main>
    </div>
  );
}
