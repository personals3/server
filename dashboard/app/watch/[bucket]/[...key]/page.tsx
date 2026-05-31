"use client";

import { useEffect, useState } from "react";
import { useParams, useSearchParams } from "next/navigation";
import Link from "next/link";
import { VideoPlayer } from "@/components/video-player";

interface ObjectInfo {
  objectId: string;
  key: string;
  contentType: string;
  transcoded?: {
    type?: string;
    master?: string;
    hls?: string;
  };
  transcodeStatus?: string;
}

/**
 * Chromeless video page — /watch/{bucket}/{key}
 *
 * Resolution order:
 *   1. If a ?sig=...&expires=... is on the URL, treat it as a presigned link
 *      and serve the original via /share/. Useful for sending one-shot links.
 *   2. Try the authenticated /api/{bucket}/{key}?info (uses the session
 *      cookie if signed in). This is the "logged-in dashboard user follows a
 *      /watch/ link" path and is the only way to get info for private buckets.
 *   3. On 401/403, fall back to /public/{bucket}/{key}?info — works only for
 *      buckets the owner has flagged public. Returns the same JSON shape.
 *   4. If both fail and we got a 401 from the authenticated probe, show a
 *      "Sign in to view" card instead of a generic error.
 *
 * Renders an HLS master playlist when transcoded; otherwise plays the raw
 * file directly via a <video src>.
 */
export default function WatchPage() {
  const params  = useParams<{ bucket: string; key: string[] }>();
  const sp      = useSearchParams();
  const bucket  = decodeURIComponent(params.bucket);
  const key     = (params.key ?? []).map(decodeURIComponent).join("/");
  const sig     = sp.get("sig");
  const expires = sp.get("expires");

  const [info,      setInfo]      = useState<ObjectInfo | null>(null);
  const [err,       setErr]       = useState<string | null>(null);
  const [needsAuth, setNeedsAuth] = useState(false);

  const encKey = key.split("/").map(encodeURIComponent).join("/");

  // Direct-source URLs (no API auth needed if signed or public)
  const buildShareURL = (suffix = "") =>
    sig && expires
      ? `/share/${encodeURIComponent(bucket)}/${encKey}?sig=${sig}&expires=${expires}${suffix}`
      : null;

  useEffect(() => {
    void (async () => {
      try {
        // 1. Authenticated /info first (works for both private + public when signed in).
        const auth = await fetch(
          `/api/${encodeURIComponent(bucket)}/${encKey}?info`,
          { credentials: "include" },
        );
        if (auth.ok) {
          setInfo(await auth.json());
          return;
        }

        // 2. Fall back to the public /info — returns 200 iff bucket.is_public.
        if (auth.status === 401 || auth.status === 403 || auth.status === 404) {
          const pub = await fetch(
            `/public/${encodeURIComponent(bucket)}/${encKey}?info`,
          );
          if (pub.ok) {
            setInfo(await pub.json());
            return;
          }
          // Public probe failed too. If the authenticated call said "no auth",
          // surface the sign-in card; otherwise treat as not found.
          if (auth.status === 401 || auth.status === 403) {
            setNeedsAuth(true);
            return;
          }
          setErr("File not found.");
          return;
        }

        setErr(`Failed to load (${auth.status}).`);
      } catch (e) {
        setErr(e instanceof Error ? e.message : "failed to load");
      }
    })();
  }, [bucket, encKey]);

  if (needsAuth) {
    const next = `/watch/${encodeURIComponent(bucket)}/${encKey}`;
    return (
      <main className="min-h-screen bg-bg flex items-center justify-center p-4">
        <div className="w-full max-w-sm space-y-4 rounded-lg border border-border bg-panel p-6 text-center">
          <h1 className="text-lg font-semibold">Sign in to view</h1>
          <p className="text-sm text-muted">
            This file is in a private bucket. Sign in to play it.
          </p>
          <p className="text-xs text-muted font-mono break-all">{bucket}/{key}</p>
          <Link
            href={`/login?next=${encodeURIComponent(next)}`}
            className="inline-block w-full rounded bg-accent px-4 py-2 text-sm font-medium text-black hover:opacity-90"
          >
            Sign in
          </Link>
        </div>
      </main>
    );
  }

  if (err) {
    return (
      <main className="min-h-screen flex items-center justify-center text-danger p-4 text-center">
        {err}
      </main>
    );
  }
  if (!info) {
    return (
      <main className="min-h-screen flex items-center justify-center text-muted p-4">
        Loading...
      </main>
    );
  }

  const hasHLS = info.transcoded?.master || info.transcoded?.hls;
  const hlsURL = hasHLS && info.objectId
    ? `/stream/${info.objectId}/${info.transcoded?.master || info.transcoded?.hls}`
    : null;

  const rawURL = buildShareURL() || `/public/${encodeURIComponent(bucket)}/${encKey}`;

  return (
    <main className="min-h-screen bg-black flex flex-col items-center justify-center p-2 sm:p-4">
      <div className="w-full max-w-5xl space-y-2">
        {hlsURL ? (
          <VideoPlayer src={hlsURL} type="application/x-mpegURL" />
        ) : (
          <video
            controls
            autoPlay
            playsInline
            className="w-full h-auto max-h-[100dvh] bg-black rounded"
          >
            <source src={rawURL} type={info.contentType} />
          </video>
        )}
        <p className="text-xs text-white/40 text-center font-mono truncate px-2">
          {bucket}/{key}
        </p>
      </div>
    </main>
  );
}
