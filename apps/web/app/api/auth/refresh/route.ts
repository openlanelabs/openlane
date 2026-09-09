import { NextRequest, NextResponse } from "next/server";

const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

export async function POST(req: NextRequest) {
  const res = await fetch(`${API}/v1/auth/refresh`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: await req.text(),
    cache: "no-store",
  });
  return new NextResponse(await res.text(), {
    status: res.status,
    headers: { "content-type": res.headers.get("content-type") ?? "application/json" },
  });
}
