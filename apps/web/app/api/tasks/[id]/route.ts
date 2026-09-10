import { NextRequest, NextResponse } from "next/server";

const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

async function proxy(req: NextRequest, { params }: { params: Promise<{ id: string }> }) {
  const auth = req.headers.get("authorization");
  if (!auth) return NextResponse.json({ title: "unauthorized" }, { status: 401 });
  const { id } = await params;
  const res = await fetch(`${API}/v1/tasks/${encodeURIComponent(id)}`, {
    method: req.method === "GET" ? "GET" : req.method === "PATCH" ? "PATCH" : "DELETE",
    headers: { authorization: auth, "content-type": "application/json" },
    body: req.method === "GET" || req.method === "DELETE" ? undefined : await req.text(),
    cache: "no-store",
  });
  return new NextResponse(await res.text(), {
    status: res.status,
    headers: { "content-type": res.headers.get("content-type") ?? "application/json" },
  });
}

export const GET = proxy;
export const PATCH = proxy;
export const DELETE = proxy;
