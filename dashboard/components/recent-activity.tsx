"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api } from "@/lib/api";
import { formatBytes, formatDate } from "@/lib/format";

interface ActivityEntry {
  id: number;
  action: string;
  bucket?: string;
  key?: string;
  sizeBytes?: number;
  statusCode?: number;
  ts: string;
}

const ACTION_LABEL: Record<string, string> = {
  PUT_OBJECT:        "Uploaded",
  DELETE_OBJECT:     "Deleted",
  GET_OBJECT:        "Downloaded",
  CREATE_BUCKET:     "Created bucket",
  DELETE_BUCKET:     "Deleted bucket",
  CREATE_API_KEY:    "Created API key",
  REVOKE_API_KEY:    "Revoked API key",
  LOGIN:             "Logged in",
};

const ACTION_COLOR: Record<string, string> = {
  PUT_OBJECT:        "text-success",
  DELETE_OBJECT:     "text-danger",
  DELETE_BUCKET:     "text-danger",
  REVOKE_API_KEY:    "text-warning",
};

/**
 * Recent activity from the audit log, scoped to the current user.
 * Polls every 5s so it stays fresh without being noisy.
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
    return <p className="text-xs text-muted">No recent activity yet — try uploading something.</p>;
  }

  return (
    <ul className="space-y-1.5 text-sm">
      {entries.map((e) => {
        const label = ACTION_LABEL[e.action] || e.action;
        const color = ACTION_COLOR[e.action] || "text-text";
        const failed = e.statusCode != null && e.statusCode >= 400;
        return (
          <li key={e.id} className="flex items-center gap-2">
            <span className={`text-xs ${failed ? "text-danger" : color} shrink-0 w-28`}>
              {label}
              {failed && ` (${e.statusCode})`}
            </span>
            <span className="flex-1 min-w-0 font-mono text-xs truncate">
              {e.bucket && e.key ? (
                <Link
                  href={`/dashboard/buckets/${e.bucket}`}
                  className="hover:text-accent"
                  title={`${e.bucket}/${e.key}`}
                >
                  {e.bucket}/{e.key}
                </Link>
              ) : e.bucket ? (
                <Link href={`/dashboard/buckets/${e.bucket}`} className="hover:text-accent">
                  {e.bucket}
                </Link>
              ) : (
                <span className="text-muted">—</span>
              )}
            </span>
            {e.sizeBytes != null && e.sizeBytes > 0 && (
              <span className="text-[11px] text-muted shrink-0">{formatBytes(e.sizeBytes)}</span>
            )}
            <span className="text-[11px] text-muted shrink-0 whitespace-nowrap">{formatDate(e.ts)}</span>
          </li>
        );
      })}
    </ul>
  );
}
