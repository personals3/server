"use client";

import { useEffect, useState } from "react";
import { useParams, useSearchParams } from "next/navigation";
import Link from "next/link";

interface ObjectInfo {
  objectId: string;
  key: string;
  contentType: string;
  size?: number;
  transcoded?: {
    type?: string;
    hls?: string;
    mp3?: string;
    ogg?: string;
  };
}

/**
 * Chromeless audio page — /listen/{bucket}/{key}
 *
 * Same resolution order as /watch:
 *   1. Presigned (?sig + ?expires) → /share/.
 *   2. Authenticated /api/{bucket}/{key}?info (session cookie if signed in).
 *   3. On 401/403, /public/{bucket}/{key}?info — succeeds only when the
 *      bucket is flagged public.
 *   4. If both fail with auth required, render a sign-in card.
 *
 * When transcoded, picks the best supported source:
 *   - MP3 for universal playback
 *   - OGG for fallback
 * Otherwise plays the raw file via <audio src>.
 */
export default function ListenPage() {
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

  const buildShareURL = () =>
    sig && expires
      ? `/share/${encodeURIComponent(bucket)}/${encKey}?sig=${sig}&expires=${expires}`
      : null;

  useEffect(() => {
    void (async () => {
      try {
        const auth = await fetch(
          `/api/${encodeURIComponent(bucket)}/${encKey}?info`,
          { credentials: "include" },
        );
        if (auth.ok) {
          setInfo(await auth.json());
          return;
        }

        if (auth.status === 401 || auth.status === 403 || auth.status === 404) {
          const pub = await fetch(
            `/public/${encodeURIComponent(bucket)}/${encKey}?info`,
          );
          if (pub.ok) {
            setInfo(await pub.json());
            return;
          }
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
    const next = `/listen/${encodeURIComponent(bucket)}/${encKey}`;
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

  // Pick the best playable source.
  const t = info.transcoded;
  let audioSrc: string;
  let audioType: string;
  if (t?.mp3 && info.objectId) {
    audioSrc = `/stream/${info.objectId}/${t.mp3}`;
    audioType = "audio/mpeg";
  } else if (t?.ogg && info.objectId) {
    audioSrc = `/stream/${info.objectId}/${t.ogg}`;
    audioType = "audio/ogg";
  } else {
    audioSrc = buildShareURL()
      || `/public/${encodeURIComponent(bucket)}/${encKey}`;
    audioType = info.contentType;
  }

  const filename = key.split("/").pop() || key;

  return (
    <main className="min-h-screen bg-bg flex items-center justify-center p-4">
      <div className="w-full max-w-md space-y-4 text-center">
        <div className="w-24 h-24 sm:w-32 sm:h-32 mx-auto rounded-full bg-panel flex items-center justify-center">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor"
               className="w-12 h-12 sm:w-16 sm:h-16 text-accent">
            <path strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"
                  d="M9 18V5l12-2v13M9 18a3 3 0 11-6 0 3 3 0 016 0zm12-2a3 3 0 11-6 0 3 3 0 016 0z" />
          </svg>
        </div>
        <h1 className="text-base sm:text-lg font-semibold break-words px-2" title={filename}>
          {filename}
        </h1>
        <p className="text-xs text-muted font-mono break-all px-2">{bucket}/{key}</p>
        <audio controls autoPlay playsInline className="w-full">
          <source src={audioSrc} type={audioType} />
        </audio>
      </div>
    </main>
  );
}
