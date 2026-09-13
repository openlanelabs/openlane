import { NextRequest, NextResponse } from "next/server";

// Public passthrough: GET /api/portal/host → /v1/portal/host.
// Unauthenticated by design (a routing decision, not tenant data —
// §229: unresolved hosts serve the standard portal, never 404).
const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

export async function GET(req: NextRequest) {
  const url = new URL(req.url);
  const upstream = await fetch(`${API}/v1/portal/host${url.search}`, {
    cache: "no-store",
  });
  return new NextResponse(await upstream.text(), {
    status: upstream.status,
    headers: { "content-type": "application/json" },
  });
}
