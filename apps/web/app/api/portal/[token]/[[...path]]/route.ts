import { NextRequest, NextResponse } from "next/server";

const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

// Generic same-origin portal proxy: any GET/POST under
// /api/portal/{token}/... maps to /v1/portal/{token}/... on the API.
// The browser never learns the API origin; the token stays in the path
// the customer already holds. POST bodies pass through as JSON.
// ponytail: no method/body filtering — the API enforces everything;
// this is a dumb pipe by design.
export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ token: string; path?: string[] }> },
) {
  const { token, path } = await params;
  const sub = (path ?? []).map(encodeURIComponent).join("/");
  const url = new URL(req.url);
  const upstream = await fetch(`${API}/v1/portal/${token}/${sub}${url.search}`, {
    cache: "no-store",
  });
  const body = await upstream.text();
  return new NextResponse(body, {
    status: upstream.status,
    headers: { "content-type": upstream.headers.get("content-type") ?? "application/json" },
  });
}

export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ token: string; path?: string[] }> },
) {
  const { token, path } = await params;
  const sub = (path ?? []).map(encodeURIComponent).join("/");
  const raw = await req.text();
  const upstream = await fetch(`${API}/v1/portal/${token}/${sub}`, {
    method: "POST",
    cache: "no-store",
    headers: { "content-type": "application/json" },
    body: raw,
  });
  const body = await upstream.text();
  return new NextResponse(body, {
    status: upstream.status,
    headers: { "content-type": upstream.headers.get("content-type") ?? "application/json" },
  });
}
