import { NextRequest, NextResponse } from "next/server";

const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

async function proxy(req: NextRequest) {
  const auth = req.headers.get("authorization");
  if (!auth) return NextResponse.json({ title: "unauthorized" }, { status: 401 });
  const url = new URL(req.nextUrl.pathname.replace(/^\/api/, `/v1`), API);
  req.nextUrl.searchParams.forEach((v, k) => url.searchParams.set(k, v));
  const res = await fetch(url, {
    method: req.method,
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
export const POST = proxy;
export const PATCH = proxy;
export const DELETE = proxy;
