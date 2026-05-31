"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import {
  Upload, Trash2, Download, FolderPlus, KeyRound, LogIn, FileVideo,
  ShieldCheck, Activity as ActivityIcon,
} from "lucide-react";

interface ActivityEntry {
  id: number;
  action: string;
  bucket?: string;
  key?: string;
  sizeBytes?: number;
  statusCode?: number;
  ts: string;
}

// One row's worth of presentation data. Keeps the JSX simple.
interface Presentation {
  icon: React.ReactNode;
  iconColor: string;
  sentence: React.ReactNode;
}

function present(e: ActivityEntry): Presentation {
  const filename = e.key ? e.key.split("/").pop() || e.key : null;
  const bucketLink = e.bucket ? (
    <Link href={`/dashboard/buckets/${e.bucket}`} className="text-text hover:text-accent">
      {e.bucket}
    </Link>
  ) : null;

  switch (e.action) {
    case "PUT_OBJECT":
      return {
        icon: <Upload size={14} />, iconColor: "text-success",
        sentence: <>Uploaded <span className="font-medium text-text">{filename}</span> to {bucketLink}</>,
      };
    case "DELETE_OBJECT":
      return {
        icon: <Trash2 size={14} />, iconColor: "text-danger",
        sentence: <>Deleted <span className="font-medium text-text">{filename}</span> from {bucketLink}</>,
      };
    case "GET_OBJECT":
      return {
        icon: <Download size={14} />, iconColor: "text-muted",
        sentence: <>Downloaded <span className="font-medium text-text">{filename}</span> from {bucketLink}</>,
      };
    case "CREATE_BUCKET":
      return {
        icon: <FolderPlus size={14} />, iconColor: "text-success",
        sentence: <>Created bucket {bucketLink}</>,
      };
    case "DELETE_BUCKET":
      return {
        icon: <Trash2 size={14} />, iconColor: "text-danger",
        sentence: <>Deleted bucket <span className="font-medium text-text">{e.bucket}</span></>,
      };
    case "CREATE_API_KEY":
      return {
        icon: <KeyRound size={14} />, iconColor: "text-success",
        sentence: <>Created a new API key</>,
      };
    case "REVOKE_API_KEY":
      return {
        icon: <KeyRound size={14} />, iconColor: "text-warning",
        sentence: <>Revoked an API key</>,
      };
    case "LOGIN":
      return {
        icon: <LogIn size={14} />, iconColor: "text-muted",
        sentence: <>Signed in</>,
      };
    case "TRANSCODE":
    case "TRANSCODE_DONE":
      return {
        icon: <FileVideo size={14} />, iconColor: "text-accent",
        sentence: <>Transcoded <span className="font-medium text-text">{filename}</span></>,
      };
    case "GRANT_2FA":
    case "ENABLE_2FA":
      return {
        icon: <ShieldCheck size={14} />, iconColor: "text-success",
        sentence: <>Enabled two-factor authentication</>,
      };
    default:
      // Unknown action — fall back to a neutral pill, no jargon.
      return {
        icon: <ActivityIcon size={14} />, iconColor: "text-muted",
        sentence: (
          <>
            {e.action.replace(/_/g, " ").toLowerCase()}
            {e.bucket && <> in {bucketLink}</>}
          </>
        ),
      };
  }
}

// Relative time formatter — "just now", "2 min ago", "yesterday", "Mar 14".
function relativeTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "";
  const now = Date.now();
  const secs = Math.max(0, Math.round((now - t) / 1000));
  if (secs < 30) return "just now";
  if (secs < 60) return `${secs} sec ago`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins} min ago`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return `${hrs} hr ago`;
  const days = Math.round(hrs / 24);
  if (days === 1) return "yesterday";
  if (days < 7) return `${days} days ago`;
  return new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

/**
 * Recent activity for the signed-in user — rendered as a human-readable
 * feed (icon + sentence + relative time), not a log table. Polls every
 * 5s so it stays fresh.
 */
export function RecentActivity({ limit = 12 }: { limit?: number }) {
  const [entries, setEntries] = useState<ActivityEntry[] | null>(null);

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;

    const tick = async () => {
      try {
        const r = await api<{ entries: ActivityEntry[] }>(`/auth/activity?limit=${limit}`);
        if (!cancelled) setEntries(r.entries);
      } catch { /* silent — auth may have expired */ }
      finally {
        if (!cancelled) timer = setTimeout(tick, 5000);
      }
    };
    void tick();
    return () => { cancelled = true; if (timer) clearTimeout(timer); };
  }, [limit]);

  if (entries === null) {
    return <p className="text-xs text-muted">Loading...</p>;
  }
  if (entries.length === 0) {
    return <p className="text-xs text-muted">Nothing yet — try uploading a file to get started.</p>;
  }

  return (
    <ul className="divide-y divide-border-subtle">
      {entries.map((e) => {
        const p = present(e);
        const failed = e.statusCode != null && e.statusCode >= 400;
        return (
          <li key={e.id} className="flex items-start gap-3 py-2.5 group">
            <span className={`mt-0.5 shrink-0 ${failed ? "text-danger" : p.iconColor}`}>
              {p.icon}
            </span>
            <div className="flex-1 min-w-0">
              <p className="text-sm text-text-soft leading-snug break-words">
                {p.sentence}
                {failed && <span className="ml-1 text-danger text-xs">(failed)</span>}
              </p>
              <p className="text-[11px] text-muted mt-0.5 flex items-center gap-2">
                <span>{relativeTime(e.ts)}</span>
                {e.sizeBytes != null && e.sizeBytes > 0 && (
                  <>
                    <span>·</span>
                    <span>{formatBytes(e.sizeBytes)}</span>
                  </>
                )}
              </p>
            </div>
          </li>
        );
      })}
    </ul>
  );
}
