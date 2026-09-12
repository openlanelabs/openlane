import { NextRequest, NextResponse } from "next/server";

const API = process.env.OPENLANE_API_URL ?? "http://localhost:8080";

// The callback 302s to /sso/finish with handoff cookies. Pass the
// redirect through BUT rewrite the Location to this origin and keep
// Set-Cookie — the browser stays single-origin end to end.
export async function GET(req: NextRequest) {
  const url = new URL(req.url);
  const res = await fetch(`${API}/v1/sso/callback${url.search}`, { redirect: "manual", cache: "no-store" });
  const headers = new Headers();
  const setCookies: string[] = [];
  res.headers.forEach((v, k) => {
    if (k.toLowerCase() === "set-cookie") setCookies.push(v);
  });
  if (setCookies.length > 0) headers.set("set-cookie", setCookies.join(", "));
  // Force the finish hop onto THIS origin — the API's externalBaseURL
  // is its container host (api:8080), unreachable from the browser.
  const out = new NextResponse(null, { status: res.status, headers });
  out.headers.set("location", "/sso/finish");
  return out;
}
