"use client";

import { useEffect, useState } from "react";
import { api, logout } from "@/lib/auth";
import { useRouter } from "next/navigation";

type Person = {
  id: string;
  name: string;
  role: string;
  capacity_hrs: string;
  active: boolean;
};

type Allocation = {
  id: string;
  person_id: string;
  project_id: string;
  hours_week: string;
  starts_on: string;
  ends_on: string;
  kind: string; // hard | soft
};

// Weeks start Monday. 4 columns starting the current week.
function weekStarts(count: number): Date[] {
  const now = new Date();
  const day = (now.getDay() + 6) % 7; // 0 = Monday
  const start = new Date(now);
  start.setHours(0, 0, 0, 0);
  start.setDate(start.getDate() - day);
  return Array.from({ length: count }, (_, i) => {
    const d = new Date(start);
    d.setDate(d.getDate() + i * 7);
    return d;
  });
}

function overlaps(aStart: string, aEnd: string, wStart: Date, wEnd: Date): boolean {
  const s = new Date(aStart + "T00:00:00Z").getTime();
  const e = new Date(aEnd + "T23:59:59Z").getTime();
  return s <= wEnd.getTime() && e >= wStart.getTime();
}

function utilClass(pct: number): string {
  if (pct > 110) return "text-danger";
  if (pct >= 95) return "text-warn";
  if (pct >= 60) return "text-text";
  return "text-muted";
}

export default function CapacityPage() {
  const router = useRouter();
  const [me, setMe] = useState<{ display_name: string; workspace_name: string; role: string } | null>(null);
  const [people, setPeople] = useState<Person[] | null>(null);
  const [allocs, setAllocs] = useState<Record<string, Allocation[]> | null>(null);
  const [err, setErr] = useState("");

  useEffect(() => {
    (async () => {
      const res = await fetch("/api/me");
      if (res.status === 401) {
        router.push("/login");
        return;
      }
      setMe(await res.json());
    })();
  }, [router]);

  useEffect(() => {
    if (!me) return;
    (async () => {
      const pres = await api("/people");
      if (!pres.ok) {
        setErr("Could not load people");
        setPeople([]);
        return;
      }
      const list: Person[] = await pres.json();
      setPeople(list);
      // N+1 by design: one request per person, teams are small at v1
      const per: Record<string, Allocation[]> = {};
      await Promise.all(
        list.map(async (p) => {
          const ares = await api(`/people/${p.id}/allocations`);
          per[p.id] = ares.ok ? await ares.json() : [];
        }),
      );
      setAllocs(per);
    })();
  }, [me]);

  const weeks = weekStarts(4);
  const weekEnd = (w: Date) => {
    const d = new Date(w);
    d.setDate(d.getDate() + 6);
    return d;
  };

  return (
    <div className="min-h-screen bg-bg">
      <header className="border-b border-border bg-surface px-4 py-3">
        <div className="mx-auto flex max-w-5xl items-center justify-between">
          <div>
            <p className="text-sm font-semibold text-text">{me?.workspace_name ?? ""}</p>
            <p className="text-xs text-muted">{me?.display_name ?? ""}</p>
          </div>
          <div className="flex items-center gap-4">
            <a href="/" className="text-xs text-muted hover:text-text">
              ← Projects
            </a>
            <button
              onClick={async () => {
                await logout();
                router.push("/login");
              }}
              className="text-xs text-muted hover:text-text"
            >
              Sign out
            </button>
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-5xl space-y-4 p-4">
        <h1 className="text-lg font-semibold text-text">Capacity</h1>
        {err && <p className="text-xs text-danger">{err}</p>}

        {people === null || allocs === null ? (
          <p className="text-xs text-muted">Loading…</p>
        ) : people.length === 0 ? (
          <p className="text-xs text-muted">Add people to see capacity.</p>
        ) : (
          <div className="overflow-x-auto rounded-[10px] border border-border bg-surface">
            <table className="w-full text-xs">
              <thead>
                <tr className="border-b border-border text-left text-muted">
                  <th className="p-3 font-medium">Person</th>
                  {weeks.map((w, i) => (
                    <th key={i} className="p-3 font-medium">
                      Wk of {w.toLocaleDateString(undefined, { month: "short", day: "numeric" })}
                    </th>
                  ))}
                  <th className="p-3 font-medium">Util</th>
                </tr>
              </thead>
              <tbody>
                {people.map((p) => {
                  const cap = Number(p.capacity_hrs) || 0;
                  const weekHours = weeks.map((w) => {
                    const active = (allocs[p.id] ?? []).filter((a) =>
                      overlaps(a.starts_on, a.ends_on, w, weekEnd(w)),
                    );
                    const hard = active.filter((a) => a.kind === "hard").reduce((s, a) => s + Number(a.hours_week), 0);
                    const soft = active.filter((a) => a.kind === "soft").reduce((s, a) => s + Number(a.hours_week), 0);
                    return { hard, soft, total: hard + soft };
                  });
                  const avg = cap > 0 ? weekHours.reduce((s, h) => s + h.total, 0) / weeks.length / cap : null;
                  return (
                    <tr key={p.id} className="border-b border-border last:border-0">
                      <td className="p-3">
                        <p className="font-medium text-text">{p.name}</p>
                        <p className="text-muted">
                          {p.role} · {p.capacity_hrs}h/wk
                        </p>
                      </td>
                      {weekHours.map((h, i) => (
                        <td key={i} className="p-3">
                          {h.total > 0 ? (
                            <span
                              className={`rounded-[10px] px-2 py-0.5 ${
                                h.soft > 0 && h.hard === 0 ? "bg-warn/10" : h.soft > 0 ? "bg-warn/10" : "bg-success/10"
                              }`}
                              title={`hard ${h.hard}h · soft ${h.soft}h`}
                            >
                              <span className={utilClass(cap > 0 ? (h.total / cap) * 100 : 0)}>{h.total}h</span>
                            </span>
                          ) : (
                            <span className="text-muted">—</span>
                          )}
                        </td>
                      ))}
                      <td className="p-3 font-medium">
                        {avg === null ? (
                          <span className="text-muted">—</span>
                        ) : (
                          <span className={utilClass(avg * 100)}>{Math.round(avg * 100)}%</span>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
        <p className="text-xs text-muted">
          4-week allocation view · hard allocations solid, soft at reduced weight (tooltip breaks it down) · utilization = weekly hours ÷
          capacity
        </p>
      </main>
    </div>
  );
}
