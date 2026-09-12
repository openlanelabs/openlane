"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { setSession } from "@/lib/auth";

export default function LoginPage() {
  const router = useRouter();
  const [email, setEmail] = useState("");
  const [slug, setSlug] = useState("");
  const [ssoOn, setSsoOn] = useState(false);
  const [phase, setPhase] = useState<"form" | "sent" | "error">("form");
  const [devLink, setDevLink] = useState<string | null>(null);
  const [err, setErr] = useState("");

  async function requestLink(e: React.FormEvent) {
    e.preventDefault();
    setErr("");
    const res = await fetch("/api/auth/magic-link", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ email, workspace_slug: slug }),
    });
    if (res.status === 429) {
      setPhase("error");
      setErr("Too many attempts — try again in a little while.");
      return;
    }
    if (!res.ok && res.status !== 202) {
      setPhase("error");
      setErr("Something went wrong. Try again.");
      return;
    }
    const data = await res.json().catch(() => ({}));
    if (data.dev_token) {
      // Dev mode: consume inline instead of emailing
      const c = await fetch(`/api/auth/consume?token=${data.dev_token}`);
      if (c.ok) {
        const sess = await c.json();
        setSession(sess.access_token, sess.refresh_token);
        router.push("/");
        return;
      }
    }
    setPhase("sent");
  }

  useEffect(() => {
    if (!slug) {
      setSsoOn(false);
      return;
    }
    (async () => {
      try {
        const res = await fetch(`/v1/sso/status?ws=${encodeURIComponent(slug)}`);
        const d = res.ok ? await res.json() : {};
        setSsoOn(!!d.configured);
      } catch {
        setSsoOn(false);
      }
    })();
  }, [slug]);

  return (
    <main className="min-h-screen bg-bg flex items-center justify-center p-6">
      <div className="w-full max-w-sm rounded-[10px] border border-border bg-surface p-8">
        <h1 className="text-lg font-semibold text-text">Sign in to OpenLane</h1>
        <p className="mt-1 text-sm text-muted">We&apos;ll email you a secure sign-in link. No passwords.</p>

        {phase === "sent" ? (
          <div className="mt-6 rounded-[10px] border border-border bg-bg p-4 text-center">
            <p className="text-sm font-medium text-text">Check your inbox</p>
            <p className="mt-1 text-xs text-muted">
              A sign-in link for <span className="font-medium">{email}</span> is on its way. It expires in 15 minutes.
            </p>
          </div>
        ) : (
          <form onSubmit={requestLink} className="mt-6 space-y-4">
            <div>
              <label htmlFor="email" className="text-xs font-medium text-muted">Work email</label>
              <input
                id="email"
                type="email"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="asha@acme.test"
                className="mt-1 w-full rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-primary"
              />
            </div>
            <div>
              <label htmlFor="slug" className="text-xs font-medium text-muted">Workspace</label>
              <input
                id="slug"
                required
                value={slug}
                onChange={(e) => setSlug(e.target.value)}
                placeholder="acme"
                className="mt-1 w-full rounded-[10px] border border-border bg-bg px-3 py-2 text-sm text-text placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-primary"
              />
            </div>
            {err && <p className="text-sm text-danger" role="alert">{err}</p>}
            <button
              type="submit"
              className="w-full rounded-[10px] bg-primary px-4 py-2 text-sm font-medium text-white hover:bg-primary-600"
            >
              Email me a link
            </button>
          </form>
        )}
        {ssoOn && (
          <div className="mt-4 border-t border-border pt-4">
            <a
              href={`/v1/sso/authorize?ws=${encodeURIComponent(slug)}`}
              className="block w-full rounded-[10px] border border-border bg-bg px-4 py-2 text-center text-sm font-medium text-text hover:bg-surface"
            >
              Sign in with SSO
            </a>
          </div>
        )}
      </div>
    </main>
  );
}
