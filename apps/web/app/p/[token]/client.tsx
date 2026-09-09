"use client";

import { useState } from "react";

type Task = {
  id: string;
  title: string;
  status: string;
  due_at: string | null;
  completed_at: string | null;
};

type Session = {
  project_id: string;
  project_name: string;
  customer_name: string;
  contact_name: string;
  expires_at: string;
};

export default function PortalClient({
  token,
  session,
  initialTasks,
}: {
  token: string;
  session: Session;
  initialTasks: Task[];
}) {
  const [tasks, setTasks] = useState<Task[]>(initialTasks);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState<string | null>(null);

  async function complete(task: Task) {
    setPending(task.id);
    setError(null);
    const prev = tasks;
    setTasks((t) => t.map((x) => (x.id === task.id ? { ...x, status: "done" } : x))); // optimistic
    try {
      const res = await fetch(`/api/portal/${token}/tasks/${task.id}/complete`, { method: "POST" });
      if (res.status === 410) {
        window.location.reload(); // link revoked mid-session → expired page
        return;
      }
      if (!res.ok) throw new Error();
      const done: Task = await res.json();
      setTasks((t) => t.map((x) => (x.id === done.id ? done : x)));
    } catch {
      setTasks(prev); // rollback (§20: optimistic updates with rollback)
      setError("That didn't go through. Try again in a moment.");
    } finally {
      setPending(null);
    }
  }

  const due = tasks.filter((t) => t.status !== "done");
  const done = tasks.filter((t) => t.status === "done");
  const pct = tasks.length ? Math.round((done.length / tasks.length) * 100) : 0;

  return (
    <div className="min-h-screen bg-bg">
      <header className="border-b border-border bg-surface px-4 py-4">
        <div className="mx-auto flex max-w-xl items-center justify-between">
          <div>
            <p className="text-sm font-semibold text-text">{session.customer_name}</p>
            <p className="text-xs text-muted">{session.project_name}</p>
          </div>
          <p className="text-xs text-muted">Hi, {session.contact_name.split(" ")[0]}</p>
        </div>
      </header>

      <main className="mx-auto max-w-xl space-y-3 p-4" aria-live="polite">
        <div className="rounded-[10px] border border-border bg-surface p-4">
          <div className="flex items-baseline justify-between">
            <span className="text-sm font-medium text-text">
              {due.length > 0 ? `Due for you (${due.length})` : "All done — nice work!"}
            </span>
            <span className="text-xs text-muted">
              {done.length}/{tasks.length} complete
            </span>
          </div>
          <div
            className="mt-3 h-2 overflow-hidden rounded-full bg-border"
            role="progressbar"
            aria-valuenow={pct}
            aria-valuemin={0}
            aria-valuemax={100}
          >
            <div className="h-full rounded-full bg-primary transition-all duration-150" style={{ width: `${pct}%` }} />
          </div>
        </div>

        {error && (
          <p className="text-sm text-danger" role="alert">
            {error}
          </p>
        )}

        <section aria-label="Tasks due for you" className="space-y-3">
          {due.map((t) => {
            const d = fmtDue(t.due_at);
            const overdue = d?.startsWith("Overdue");
            return (
              <div
                key={t.id}
                className="flex items-center justify-between gap-3 rounded-[10px] border border-border bg-surface p-4"
              >
                <div className="min-w-0">
                  <p className="truncate text-sm font-medium text-text">{t.title}</p>
                  {d && (
                    <p className={`mt-0.5 text-xs ${overdue ? "text-danger" : "text-muted"}`}>{d}</p>
                  )}
                </div>
                <button
                  onClick={() => complete(t)}
                  disabled={pending === t.id}
                  className="shrink-0 rounded-[10px] bg-primary px-4 py-2 text-sm font-medium text-white disabled:opacity-50"
                  aria-label={`Complete: ${t.title}`}
                >
                  {pending === t.id ? "…" : "Complete"}
                </button>
              </div>
            );
          })}
        </section>

        {done.length > 0 && (
          <details className="rounded-[10px] border border-border bg-surface">
            <summary className="cursor-pointer p-4 text-sm text-muted">
              Completed ({done.length})
            </summary>
            <ul className="space-y-2 px-4 pb-4">
              {done.map((t) => (
                <li key={t.id} className="flex items-center gap-2 text-sm text-muted">
                  <span aria-hidden className="text-success">✓</span>
                  <span className="line-through">{t.title}</span>
                </li>
              ))}
            </ul>
          </details>
        )}

        {tasks.length === 0 && (
          <div className="rounded-[10px] border border-dashed border-border bg-surface p-8 text-center">
            <p className="text-sm text-muted">Nothing needs your attention right now.</p>
            <p className="mt-1 text-xs text-muted">We&rsquo;ll email you the moment something does.</p>
          </div>
        )}
      </main>
    </div>
  );
}

function fmtDue(due: string | null): string | null {
  if (!due) return null;
  const d = new Date(due);
  const days = Math.ceil((d.getTime() - Date.now()) / 86400000);
  if (days < 0) return `Overdue by ${-days}d`;
  if (days === 0) return "Due today";
  if (days <= 7) return `Due in ${days}d`;
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}
