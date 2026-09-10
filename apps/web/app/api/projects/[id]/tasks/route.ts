import { NextRequest, NextResponse } from "next/server";

const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

async function proxy(req: NextRequest, { params }: { params: Promise<{ id: string }> }) {
  const auth = req.headers.get("authorization");
  if (!auth) return NextResponse.json({ title: "unauthorized" }, { status: 401 });
  const { id } = await params;
  const url = new URL(`/v1/projects/${encodeURIComponent(id)}/tasks`, API);
  req.nextUrl.searchParams.forEach((v, k) => url.searchParams.set(k, v));
  const res = await fetch(url, {
    method: req.method === "GET" ? "GET" : "POST",
    headers: { authorization: auth, "content-type": "application/json" },
    body: req.method === "GET" ? undefined : await req.text(),
    cache: "no-store",
  });
  return new NextResponse(await res.text(), {
    status: res.status,
    headers: { "content-type": res.headers.get("content-type") ?? "application/json" },
  });
}

export const GET = proxy;
export const POST = proxy;
