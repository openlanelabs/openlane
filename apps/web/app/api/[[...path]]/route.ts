import { NextRequest, NextResponse } from "next/server";

const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

// Staff catch-all proxy: /api/{path} → /v1/{path} with the caller's
// bearer token. Same dumb-pipe design as the portal proxy (the API
// enforces auth/authz; this never sees or stores credentials). Specific
// routes (auth, me, portal, …) take precedence over this catch-all —
// Next resolves exact segments before [...path].
async function proxy(req: NextRequest, path: string[], method: string) {
  const auth = req.headers.get("authorization");
  if (!auth) return NextResponse.json({ title: "unauthorized" }, { status: 401 });
  const ws = req.headers.get("x-workspace-id");
  const sub = path.map(encodeURIComponent).join("/");
  const url = new URL(req.url);
  const init: RequestInit = {
    method,
    cache: "no-store",
    headers: {
      authorization: auth,
      ...(ws ? { "x-workspace-id": ws } : {}),
      ...(req.headers.get("content-type") ? { "content-type": req.headers.get("content-type")! } : {}),
    },
  };
  if (method !== "GET" && method !== "DELETE") init.body = await req.text();
  const upstream = await fetch(`${API}/v1/${sub}${url.search}`, init);
  return new NextResponse(await upstream.text(), {
    status: upstream.status,
    headers: { "content-type": upstream.headers.get("content-type") ?? "application/json" },
  });
}

type Ctx = { params: Promise<{ path?: string[] }> };

export async function GET(req: NextRequest, ctx: Ctx) {
  const { path } = await ctx.params;
  return proxy(req, path ?? [], "GET");
}
export async function POST(req: NextRequest, ctx: Ctx) {
  const { path } = await ctx.params;
  return proxy(req, path ?? [], "POST");
}
export async function PATCH(req: NextRequest, ctx: Ctx) {
  const { path } = await ctx.params;
  return proxy(req, path ?? [], "PATCH");
}
export async function DELETE(req: NextRequest, ctx: Ctx) {
  const { path } = await ctx.params;
  return proxy(req, path ?? [], "DELETE");
}
export async function PUT(req: NextRequest, ctx: Ctx) {
  const { path } = await ctx.params;
  return proxy(req, path ?? [], "PUT");
}
