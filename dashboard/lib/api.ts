// Lightweight fetch wrapper that injects the saved auth token.
// Supports both JWT (from dashboard login) and API key (less common in browser).

// API base. Defaults to "/api" for same-origin deployments behind nginx.
// Override with NEXT_PUBLIC_API_URL=http://localhost:8080 for cross-origin dev.
export const API = (process.env.NEXT_PUBLIC_API_URL || "/api").replace(/\/$/, "");

// Streaming (HLS / image renditions) lives at /stream/* on the same nginx,
// NOT under /api. Derive the base from API.
export const STREAM = (() => {
  if (API.startsWith("http")) {
    const u = new URL(API);
    return `${u.protocol}//${u.host}/stream`;
  }
  return "/stream";
})();

const TOKEN_KEY = "ps3_token";

export function getToken(): string | null {
  if (typeof window === "undefined") return null;
  return localStorage.getItem(TOKEN_KEY);
}

export function setToken(t: string | null) {
  if (typeof window === "undefined") return;
  if (t) localStorage.setItem(TOKEN_KEY, t);
  else localStorage.removeItem(TOKEN_KEY);
}

export class ApiError extends Error {
  /** Any extra fields the server included in the JSON error body
   *  (e.g. objectCount on BUCKET_NOT_EMPTY). */
  extra: Record<string, unknown>;
  // Convenience getter so callers can write `err.objectCount` without casting.
  get objectCount(): number | undefined { return this.extra.objectCount as number | undefined; }

  constructor(public status: number, public code: string, message: string, extra: Record<string, unknown> = {}) {
    super(message);
    this.extra = extra;
  }
}

export async function api<T = unknown>(
  path: string,
  init: RequestInit & { auth?: boolean } = {},
): Promise<T> {
  const { auth = true, headers, ...rest } = init;
  const h = new Headers(headers);

  if (auth) {
    const token = getToken();
    if (token) h.set("Authorization", `Bearer ${token}`);
  }
  if (!h.has("Content-Type") && rest.body && typeof rest.body === "string") {
    h.set("Content-Type", "application/json");
  }

  const res = await fetch(`${API}${path}`, { ...rest, headers: h });

  // Global 401 handling — covers two real situations:
  //   1. Token expired (24h JWT TTL).
  //   2. Multi-tab race: another tab signed out, this tab still has
  //      stale closures making periodic calls.
  // We clear the local token (so other tabs notice via storage event)
  // and bounce to /login, but ONLY if we actually sent an Authorization
  // header. Unauthenticated calls that 401 (e.g. /auth/login wrong
  // password) must surface to the caller for normal error handling.
  if (res.status === 401 && auth && getToken()) {
    setToken(null);
    if (typeof window !== "undefined" && !window.location.pathname.startsWith("/login")) {
      // Preserve the page they were on so we can route back after re-login
      const next = encodeURIComponent(window.location.pathname + window.location.search);
      window.location.href = `/login?next=${next}`;
    }
  }

  if (!res.ok) {
    let code = "HTTP_" + res.status;
    let message = res.statusText;
    let extra: Record<string, unknown> = {};
    try {
      const j = await res.json();
      code = j.code || code;
      message = j.message || message;
      const { code: _c, message: _m, ...rest } = j;
      extra = rest;
    } catch { /* not JSON */ }
    throw new ApiError(res.status, code, message, extra);
  }

  if (res.status === 204) return undefined as T;
  const ct = res.headers.get("content-type") || "";
  if (ct.includes("application/json")) return res.json() as Promise<T>;
  return res.text() as unknown as Promise<T>;
}
