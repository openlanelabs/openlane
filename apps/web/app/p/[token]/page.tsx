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

const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

async function getJSON<T>(path: string): Promise<T | null> {
  try {
    const res = await fetch(`${API}${path}`, { cache: "no-store" });
    if (res.status === 200) return (await res.json()) as T;
    return null;
  } catch {
    return null;
  }
}

export default async function PortalPage({
  params,
}: {
  params: Promise<{ token: string }>;
}) {
  const { token } = await params;
  const sess = await getJSON<Session>(`/v1/portal/${token}/session`);
  if (sess === null) notFound(); // 401/404/5xx all → notFound (no oracle)
  // 410 (revoked/expired) needs the friendly resend CTA — distinguish:
  const probe = await fetch(`${API}/v1/portal/${token}/tasks`, { cache: "no-store" });
  if (probe.status === 410) {
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
  const [tasks, approvals, files, docs] = await Promise.all([
    getJSON<Task[]>(`/v1/portal/${token}/tasks`),
    getJSON<Approval[]>(`/v1/portal/${token}/approvals`),
    getJSON<File[]>(`/v1/portal/${token}/files`),
    getJSON<Doc[]>(`/v1/portal/${token}/docs`),
  ]);
  return (
    <PortalClient
      token={token}
      session={sess}
      initialTasks={tasks ?? []}
      initialApprovals={approvals ?? []}
      initialFiles={files ?? []}
      initialDocs={docs ?? []}
    />
  );
}
