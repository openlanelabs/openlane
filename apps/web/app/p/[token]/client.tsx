"use client";

import { useEffect, useRef, useState } from "react";

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

type Approval = {
  id: string;
  project_id: string;
  title: string;
  description: string;
};

type File = {
  id: string;
  name: string;
  content_type: string;
  size_bytes: number;
};

type Doc = {
  id: string;
  title: string;
  content_md: string;
  updated_at: string;
};

type Message = {
  id: string;
  task_id: string;
  author_type: "user" | "contact";
  author_name: string;
  body: string;
  created_at: string;
};

type Tab = "tasks" | "approvals" | "files" | "docs";

// api(): same-origin proxy under /api/portal/{token}/… (the customer
// never learns the API origin; the token lives in their URL anyway).
async function api<T>(
  token: string,
  method: string,
  subpath: string,
  body?: unknown,
): Promise<{ ok: boolean; status: number; data: T | null }> {
  const res = await fetch(`/api/portal/${token}/${subpath}`, {
    method,
    headers: body !== undefined ? { "content-type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  let data: T | null = null;
  try {
    data = (await res.json()) as T;
  } catch {
    /* empty 204 etc. */
  }
  return { ok: res.ok, status: res.status, data };
}

export default function PortalClient({
  token,
  session,
  initialTasks,
  initialApprovals,
  initialFiles,
  initialDocs,
}: {
  token: string;
  session: Session;
  initialTasks: Task[];
  initialApprovals: Approval[];
  initialFiles: File[];
  initialDocs: Doc[];
}) {
  const [tab, setTab] = useState<Tab>("tasks");
  const [tasks, setTasks] = useState<Task[]>(initialTasks);
  const [approvals, setApprovals] = useState<Approval[]>(initialApprovals);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState<string | null>(null);

  async function complete(task: Task) {
    setPending(task.id);
    setError(null);
    const prev = tasks;
    setTasks((t) => t.map((x) => (x.id === task.id ? { ...x, status: "done" } : x))); // optimistic
    const r = await api<Task>(token, "POST", `tasks/${task.id}/complete`);
    if (r.status === 410) {
      window.location.reload(); // link revoked mid-session → expired page
      return;
    }
    if (!r.ok) {
      setTasks(prev); // rollback (§20)
      setError("That didn't go through. Try again in a moment.");
    } else if (r.data) {
      setTasks((t) => t.map((x) => (x.id === r.data!.id ? r.data! : x)));
    }
    setPending(null);
  }

  const due = tasks.filter((t) => t.status !== "done");
  const done = tasks.filter((t) => t.status === "done");
  const pct = tasks.length ? Math.round((done.length / tasks.length) * 100) : 0;

  const tabs: { key: Tab; label: string; badge?: number }[] = [
    { key: "tasks", label: "Tasks", badge: due.length },
    { key: "approvals", label: "Approvals", badge: approvals.length },
    { key: "files", label: "Files", badge: initialFiles.length },
    { key: "docs", label: "Docs", badge: initialDocs.length },
  ];

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

      <nav className="mx-auto max-w-xl px-4 pt-4" aria-label="Portal sections">
        <div className="flex gap-1 overflow-x-auto" role="tablist">
          {tabs.map((t) => (
            <button
              key={t.key}
              role="tab"
              aria-selected={tab === t.key}
              onClick={() => setTab(t.key)}
              className={`rounded-[10px] px-3 py-2 text-sm font-medium ${
                tab === t.key ? "bg-primary text-white" : "bg-surface text-text border border-border"
              }`}
            >
              {t.label}
              {t.badge ? (
                <span
                  className={`ml-1.5 rounded-full px-1.5 text-xs ${
                    tab === t.key ? "bg-white/20" : "bg-border"
                  }`}
                >
                  {t.badge}
                </span>
              ) : null}
            </button>
          ))}
        </div>
      </nav>

      <main className="mx-auto max-w-xl space-y-3 p-4" aria-live="polite">
        {error && (
          <p className="text-sm text-danger" role="alert">
            {error}
          </p>
        )}

        {tab === "tasks" && (
          <>
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
                <div
                  className="h-full rounded-full bg-primary transition-all duration-150"
                  style={{ width: `${pct}%` }}
                />
              </div>
            </div>

            <section aria-label="Tasks due for you" className="space-y-3">
              {due.map((t) => (
                <TaskCard
                  key={t.id}
                  token={token}
                  task={t}
                  pending={pending === t.id}
                  onComplete={() => complete(t)}
                />
              ))}
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

            {tasks.length === 0 && <EmptyState />}
          </>
        )}

        {tab === "approvals" && (
          <section aria-label="Approvals" className="space-y-3">
            {approvals.length === 0 && <EmptyState text="Nothing needs your approval right now." />}
            {approvals.map((a) => (
              <ApprovalCard
                key={a.id}
                token={token}
                approval={a}
                onDecided={() => setApprovals((xs) => xs.filter((x) => x.id !== a.id))}
              />
            ))}
          </section>
        )}

        {tab === "files" && (
          <section aria-label="Files" className="space-y-3">
            {initialFiles.length === 0 && <EmptyState text="No files shared yet." />}
            {initialFiles.map((f) => (
              <FileRow key={f.id} token={token} file={f} />
            ))}
          </section>
        )}

        {tab === "docs" && (
          <section aria-label="Documents" className="space-y-3">
            {initialDocs.length === 0 && <EmptyState text="No documents shared yet." />}
            {initialDocs.map((d) => (
              <details key={d.id} className="rounded-[10px] border border-border bg-surface">
                <summary className="cursor-pointer p-4 text-sm font-medium text-text">{d.title}</summary>
                <pre className="whitespace-pre-wrap break-words px-4 pb-4 text-sm text-muted">
                  {d.content_md}
                </pre>
              </details>
            ))}
          </section>
        )}
      </main>
    </div>
  );
}

function EmptyState({ text }: { text?: string }) {
  return (
    <div className="rounded-[10px] border border-dashed border-border bg-surface p-8 text-center">
      <p className="text-sm text-muted">{text ?? "Nothing needs your attention right now."}</p>
      <p className="mt-1 text-xs text-muted">We&rsquo;ll email you the moment something does.</p>
    </div>
  );
}

function TaskCard({
  token,
  task,
  pending,
  onComplete,
}: {
  token: string;
  task: Task;
  pending: boolean;
  onComplete: () => void;
}) {
  const [open, setOpen] = useState(false);
  const d = fmtDue(task.due_at);
  const overdue = d?.startsWith("Overdue");
  return (
    <div className="rounded-[10px] border border-border bg-surface">
      <div className="flex items-center justify-between gap-3 p-4">
        <div className="min-w-0">
          <p className="truncate text-sm font-medium text-text">{task.title}</p>
          {d && <p className={`mt-0.5 text-xs ${overdue ? "text-danger" : "text-muted"}`}>{d}</p>}
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <button
            onClick={() => setOpen((v) => !v)}
            aria-expanded={open}
            className="rounded-[10px] border border-border px-3 py-2 text-sm"
            aria-label={`Messages: ${task.title}`}
            title="Messages"
          >
            💬
          </button>
          <button
            onClick={onComplete}
            disabled={pending}
            className="rounded-[10px] bg-primary px-4 py-2 text-sm font-medium text-white disabled:opacity-50"
            aria-label={`Complete: ${task.title}`}
          >
            {pending ? "…" : "Complete"}
          </button>
        </div>
      </div>
      {open && <ChatPanel token={token} taskId={task.id} />}
    </div>
  );
}

// ChatPanel: thread + post; polls after_id every 5s while open (§449
// polling fallback until the WS hub).
function ChatPanel({ token, taskId }: { token: string; taskId: string }) {
  const [messages, setMessages] = useState<Message[] | null>(null);
  const [body, setBody] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const cursor = useRef<string>("");
  const box = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    let stop = false;
    const load = async () => {
      const q = cursor.current ? `?after_id=${cursor.current}` : "";
      const r = await api<Message[]>(token, "GET", `tasks/${taskId}/messages${q}`);
      if (stop || !r.ok || !r.data) return;
      if (r.data.length > 0) {
        setMessages((m) => (m === null ? r.data! : [...m, ...r.data!]));
        cursor.current = r.data[r.data.length - 1].id;
      } else if (messages === null) {
        setMessages([]);
      }
    };
    load();
    const timer = setInterval(load, 5000);
    return () => {
      stop = true;
      clearInterval(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token, taskId]);

  async function send() {
    const text = body.trim();
    if (!text || busy) return;
    setBusy(true);
    setError(null);
    const r = await api<Message>(token, "POST", `tasks/${taskId}/messages`, { body: text });
    setBusy(false);
    if (!r.ok) {
      setError(r.status === 429 ? "You're going fast — try again in a minute." : "Couldn't send. Try again.");
      return;
    }
    setBody("");
    if (r.data) {
      setMessages((m) => (m === null ? [r.data!] : [...m, r.data!]));
      cursor.current = r.data!.id;
    }
  }

  return (
    <div className="border-t border-border p-4" aria-label="Task messages">
      <div ref={box} className="max-h-64 space-y-2 overflow-y-auto">
        {messages === null && <p className="text-xs text-muted">Loading…</p>}
        {messages?.length === 0 && <p className="text-xs text-muted">No messages yet — say hello.</p>}
        {messages?.map((m) => (
          <div key={m.id} className="text-sm">
            <span className={`font-medium ${m.author_type === "contact" ? "text-primary" : "text-text"}`}>
              {m.author_name || (m.author_type === "contact" ? "You" : "Team")}:
            </span>{" "}
            <span className="text-text">{m.body}</span>
          </div>
        ))}
      </div>
      <div className="mt-3 flex gap-2">
        <input
          value={body}
          onChange={(e) => setBody(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && !e.shiftKey && send()}
          placeholder="Write a message…"
          aria-label="Message"
          className="min-w-0 flex-1 rounded-[10px] border border-border bg-bg p-2 text-sm text-text"
          maxLength={2000}
        />
        <button
          onClick={send}
          disabled={busy || !body.trim()}
          className="rounded-[10px] bg-primary px-3 py-2 text-sm font-medium text-white disabled:opacity-50"
        >
          {busy ? "…" : "Send"}
        </button>
      </div>
      {error && <p className="mt-2 text-xs text-danger">{error}</p>}
    </div>
  );
}

// ApprovalCard: decide (approved / changes_requested) with optional
// comment, then the 1-click CSAT widget (§218) on the same card.
function ApprovalCard({
  token,
  approval,
  onDecided,
}: {
  token: string;
  approval: Approval;
  onDecided: () => void;
}) {
  const [comment, setComment] = useState("");
  const [busy, setBusy] = useState(false);
  const [phase, setPhase] = useState<"decide" | "csat" | "done">("decide");
  const [error, setError] = useState<string | null>(null);

  async function decide(status: "approved" | "changes_requested") {
    setBusy(true);
    setError(null);
    const r = await api<unknown>(token, "POST", `approvals/${approval.id}/decide`, {
      decision: status,
      comment,
    });
    setBusy(false);
    if (r.status === 410) {
      window.location.reload();
      return;
    }
    if (!r.ok) {
      setError("Couldn't record that — try again.");
      return;
    }
    setPhase("csat");
  }

  return (
    <div className="rounded-[10px] border border-border bg-surface p-4">
      {phase === "decide" && (
        <>
          <p className="text-sm font-medium text-text">{approval.title}</p>
          {approval.description && <p className="mt-1 text-sm text-muted">{approval.description}</p>}
          <label className="mt-3 block text-xs text-muted" htmlFor={`c-${approval.id}`}>
            Comment (optional)
          </label>
          <textarea
            id={`c-${approval.id}`}
            value={comment}
            onChange={(e) => setComment(e.target.value)}
            rows={2}
            maxLength={2000}
            className="mt-1 w-full rounded-[10px] border border-border bg-bg p-2 text-sm text-text"
            placeholder="Add context for your team…"
          />
          <div className="mt-3 flex gap-2">
            <button
              onClick={() => decide("approved")}
              disabled={busy}
              className="flex-1 rounded-[10px] bg-success px-4 py-2 text-sm font-medium text-white disabled:opacity-50"
            >
              {busy ? "…" : "✓ Approve"}
            </button>
            <button
              onClick={() => decide("changes_requested")}
              disabled={busy}
              className="flex-1 rounded-[10px] border border-danger px-4 py-2 text-sm font-medium text-danger disabled:opacity-50"
            >
              Request changes
            </button>
          </div>
          {error && <p className="mt-2 text-xs text-danger">{error}</p>}
        </>
      )}
      {phase === "csat" && (
        <CSATPanel
          token={token}
          approval={approval}
          onDone={() => {
            setPhase("done");
            onDecided();
          }}
        />
      )}
      {phase === "done" && <p className="text-sm text-muted">Thanks — that&rsquo;s noted.</p>}
    </div>
  );
}

// CSATPanel: 😞😐🙂😄 1-click + optional comment (§218).
function CSATPanel({
  token,
  approval,
  onDone,
}: {
  token: string;
  approval: Approval;
  onDone: () => void;
}) {
  const [score, setScore] = useState<number | null>(null);
  const [comment, setComment] = useState("");
  const [busy, setBusy] = useState(false);
  const choices: [number, string, string][] = [
    [1, "😞", "Very unhappy"],
    [3, "😐", "Okay"],
    [5, "😄", "Great"],
  ];

  async function rate(n: number) {
    setBusy(true);
    const r = await api<unknown>(token, "POST", "csat", {
      approval_id: approval.id,
      score: n,
      comment,
    });
    setBusy(false);
    if (!r.ok) return; // stay put; they can retry or move on
    onDone();
  }

  return (
    <div aria-label="How did that go?">
      <p className="text-sm font-medium text-text">How did that go?</p>
      <div className="mt-3 flex gap-3">
        {choices.map(([n, emoji, label]) => (
          <button
            key={n}
            onClick={() => {
              setScore(n);
              rate(n);
            }}
            disabled={busy}
            aria-label={label}
            className={`flex-1 rounded-[10px] border p-3 text-2xl transition-colors ${
              score === n ? "border-primary bg-primary/10" : "border-border bg-bg"
            } disabled:opacity-50`}
          >
            {emoji}
          </button>
        ))}
      </div>
      {score !== null && !busy && (
        <input
          value={comment}
          onChange={(e) => setComment(e.target.value)}
          placeholder="Anything to add? (optional)"
          aria-label="CSAT comment"
          className="mt-3 w-full rounded-[10px] border border-border bg-bg p-2 text-sm text-text"
          maxLength={2000}
        />
      )}
      <button onClick={onDone} className="mt-3 text-xs text-muted underline">
        Skip
      </button>
    </div>
  );
}

async function openFile(token: string, id: string) {
  const r = await api<{ url: string }>(token, "GET", `files/${id}/url`);
  if (r.ok && r.data?.url) window.open(r.data.url, "_blank", "noopener");
}

function FileRow({ token, file }: { token: string; file: File }) {
  return (
    <button
      onClick={() => openFile(token, file.id)}
      className="flex w-full items-center justify-between gap-3 rounded-[10px] border border-border bg-surface p-4 text-left hover:border-primary"
    >
      <div className="min-w-0">
        <p className="truncate text-sm font-medium text-text">{file.name}</p>
        <p className="text-xs text-muted">{fmtSize(file.size_bytes)}</p>
      </div>
      <span aria-hidden className="text-muted">↓</span>
    </button>
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

function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}
