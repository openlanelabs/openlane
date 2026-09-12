import { NextRequest, NextResponse } from "next/server";

const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

// Browser-navigation passthrough: this must be a real 302 (fetch would
// swallow it), so it's a route handler, not the /api catch-all proxy.
export async function GET(req: NextRequest) {
  const ws = req.nextUrl.searchParams.get("ws") ?? "";
  const url = new URL(req.url);
  return NextResponse.redirect(`${API}/v1/sso/authorize?ws=${encodeURIComponent(ws)}${url.search ? "" : ""}`, 302);
}
