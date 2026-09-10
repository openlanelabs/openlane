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
  const [newTitle, setNewTitle] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  const load = useCallback(async () => {
    const [pRes, tRes] = await Promise.all([api(`/projects/${id}`), api(`/projects/${id}/tasks`)]);
    if (pRes.status === 401) { router.push("/login"); return; }
    if (pRes.ok) setProject(await pRes.json());
    if (tRes.ok) setTasks(await tRes.json());
    else setErr("Couldn't load tasks.");
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
      </main>
    </div>
  );
}
