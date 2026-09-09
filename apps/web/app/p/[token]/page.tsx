import { notFound } from "next/navigation";
import type { Metadata } from "next";
import PortalClient from "./client";

export const dynamic = "force-dynamic"; // token in URL — never cache

export const metadata: Metadata = {
  robots: { index: false }, // §7.4-E9: portal PII must not be indexed
};

type Session = {
  project_id: string;
  project_name: string;
  customer_name: string;
  contact_name: string;
  expires_at: string;
};

type Task = {
  id: string;
  title: string;
  status: string;
  due_at: string | null;
  completed_at: string | null;
};

const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

async function fetchSession(token: string) {
  const res = await fetch(`${API}/v1/portal/${token}/session`, { cache: "no-store" });
  if (res.status === 200) return { kind: "ok" as const, data: await res.json() };
  if (res.status === 410) return { kind: "gone" as const }; // revoked/expired → friendly resend CTA
  return { kind: "bad" as const }; // 401 tampered / 5xx
}

async function fetchTasks(token: string) {
  const res = await fetch(`${API}/v1/portal/${token}/tasks`, { cache: "no-store" });
  if (res.status === 200) return (await res.json()) as Task[];
  return null;
}

export default async function PortalPage({
  params,
}: {
  params: Promise<{ token: string }>;
}) {
  const { token } = await params;
  const sess = await fetchSession(token);
  if (sess.kind === "bad") notFound();
  if (sess.kind === "gone") {
    return (
      <main className="min-h-screen bg-bg p-6 flex items-center justify-center">
        <div className="w-full max-w-sm rounded-[10px] border border-border bg-surface p-8 text-center">
          <h1 className="text-lg font-semibold text-text">This link has expired</h1>
          <p className="mt-2 text-sm text-muted">
            Magic links last 7 days for your security. Ask your project team to
            send a fresh one — it takes a second.
          </p>
          {/* §7.4-E2: resend CTA — real endpoint lands with the email pillar */}
          <span className="mt-6 inline-flex h-10 items-center justify-center rounded-[10px] bg-primary px-4 text-sm font-medium text-white">
            Request a new link
          </span>
        </div>
      </main>
    );
  }
  const tasks = (await fetchTasks(token)) ?? [];
  return <PortalClient token={token} session={sess.data} initialTasks={tasks} />;
}
