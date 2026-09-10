import { NextRequest, NextResponse } from "next/server";

const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

// Fixed upstream path — user input only ever travels in the query string.
export async function GET(req: NextRequest) {
  const auth = req.headers.get("authorization");
  if (!auth) return NextResponse.json({ title: "unauthorized" }, { status: 401 });
  const url = new URL("/v1/projects", API);
  req.nextUrl.searchParams.forEach((v, k) => url.searchParams.set(k, v));
  const res = await fetch(url, { headers: { authorization: auth }, cache: "no-store" });
  return new NextResponse(await res.text(), {
    status: res.status,
    headers: { "content-type": res.headers.get("content-type") ?? "application/json" },
  });
}

export async function POST(req: NextRequest) {
  const auth = req.headers.get("authorization");
  if (!auth) return NextResponse.json({ title: "unauthorized" }, { status: 401 });
  const res = await fetch(`${API}/v1/projects`, {
    method: "POST",
    headers: { authorization: auth, "content-type": "application/json" },
    body: await req.text(),
    cache: "no-store",
  });
  return new NextResponse(await res.text(), {
    status: res.status,
    headers: { "content-type": res.headers.get("content-type") ?? "application/json" },
  });
}
