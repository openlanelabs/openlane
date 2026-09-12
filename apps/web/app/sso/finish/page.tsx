"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { setSession } from "@/lib/auth";

// SSO handoff (§406, #111): the API set ol_sso_at/ol_sso_rt cookies on
// the callback response and redirected here. Copy them into the app's
// token storage, clear the cookies (60s MaxAge anyway — belt and
// suspenders), go home.
export default function SSOFinish() {
  const router = useRouter();
  const [err, setErr] = useState("");

  useEffect(() => {
    const at = readCookie("ol_sso_at");
    const rt = readCookie("ol_sso_rt");
    if (!at || !rt) {
      setErr("Sign-in handoff expired — start again.");
      return;
    }
    setSession(at, rt);
    document.cookie = "ol_sso_at=; Max-Age=0; path=/";
    document.cookie = "ol_sso_rt=; Max-Age=0; path=/";
    router.push("/");
  }, [router]);

  return (
    <main className="min-h-screen bg-bg flex items-center justify-center p-6">
      <div className="w-full max-w-sm rounded-[10px] border border-border bg-surface p-8 text-center">
        {err ? (
          <>
            <p className="text-sm font-medium text-text">Sign-in didn&apos;t complete</p>
            <p className="mt-1 text-xs text-muted">{err}</p>
            <a href="/login" className="mt-4 inline-block text-xs text-primary hover:underline">
              Back to login
            </a>
          </>
        ) : (
          <p className="text-sm text-muted">Finishing sign-in…</p>
        )}
      </div>
    </main>
  );
}

function readCookie(name: string): string | null {
  const m = document.cookie.match(new RegExp(`(?:^|; )${name}=([^;]*)`));
  return m ? decodeURIComponent(m[1]) : null;
}
