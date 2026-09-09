import { NextRequest, NextResponse } from "next/server";

const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

// Same-origin proxy: the browser never learns the API origin; token stays in
// the path the user already holds. POST only — no caching semantics involved.
export async function POST(
  req: NextRequest,
  { params }: { params: Promise<{ token: string; taskId: string }> },
) {
  const { token, taskId } = await params;
  const upstream = await fetch(`${API}/v1/portal/${token}/tasks/${taskId}/complete`, {
    method: "POST",
    cache: "no-store",
  });
  const body = await upstream.text();
  return new NextResponse(body, {
    status: upstream.status,
    headers: { "content-type": upstream.headers.get("content-type") ?? "application/json" },
  });
}
