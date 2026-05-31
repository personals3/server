"use client";

import { useEffect, useState } from "react";
import { api } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { Database, Trash2, Layers, Hourglass, ArchiveRestore } from "lucide-react";
import Link from "next/link";

interface BucketEntry {
  id: string;
  name: string;
  originalBytes: number;
  transcodedBytes: number;
  reservedBytes: number;
  trashBytes: number;
  totalBytes: number;
  archived: boolean;
}

interface Breakdown {
  quotaBytes: number;
  usedBytes: number;
  buckets: BucketEntry[];
  trashBytes: number;
  versionsBytes: number;
  reservedBytes: number;
}

// Distinct colors per bucket so each shows up on the stacked bar. Cycles for
// users with > BUCKET_PALETTE.length buckets.
const BUCKET_PALETTE = [
  "#3b82f6", // blue
  "#10b981", // emerald
  "#f59e0b", // amber
  "#8b5cf6", // violet
  "#ec4899", // pink
  "#06b6d4", // cyan
  "#84cc16", // lime
  "#f97316", // orange
];

const TRASH_COLOR    = "#94a3b8"; // slate (dim — it's "soft" usage)
const RESERVED_COLOR = "#fbbf24"; // amber (in-flight)

export function StorageBreakdown() {
  const [data, setData] = useState<Breakdown | null>(null);

  useEffect(() => {
    const load = () => api<Breakdown>("/auth/storage").then(setData).catch(() => {});
    load();
    const t = window.setInterval(load, 8000);
    return () => window.clearInterval(t);
  }, []);

  if (!data) {
    return <div className="h-32 animate-pulse bg-bg rounded" />;
  }

  // Build the stacked segments. Each non-zero slice gets one segment.
  // Bucket slice = its live (original + transcoded) bytes, plus a paler
  // reserved sub-slice when transcode is in-flight. Trash separately.
  // (data.buckets is nil when the user has no buckets yet — Go encodes
  // it as JSON null, so the `?? []` is load-bearing.)
  const buckets = data.buckets ?? [];
  const segments: { label: string; color: string; bytes: number; href?: string }[] = [];
  buckets.forEach((b, i) => {
    const live = b.originalBytes + b.transcodedBytes;
    if (live > 0) {
      segments.push({
        label: b.name,
        color: BUCKET_PALETTE[i % BUCKET_PALETTE.length],
        bytes: live,
        href: `/dashboard/buckets/${encodeURIComponent(b.name)}`,
      });
    }
  });
  if (data.reservedBytes > 0) {
    segments.push({ label: "Transcode in-flight", color: RESERVED_COLOR, bytes: data.reservedBytes });
  }
  if (data.trashBytes > 0) {
    segments.push({ label: "Trash", color: TRASH_COLOR, bytes: data.trashBytes });
  }

  const totalShown = segments.reduce((s, x) => s + x.bytes, 0);
  const quotaPct = data.quotaBytes > 0 ? (data.usedBytes / data.quotaBytes) * 100 : 0;
  const headroomPct = Math.max(0, 100 - quotaPct);

  return (
    <div className="space-y-4">
      {/* Stacked bar */}
      <div>
        <div className="flex items-baseline justify-between text-sm mb-1">
          <span>
            <span className="font-semibold">{formatBytes(data.usedBytes)}</span>
            <span className="text-muted"> of {formatBytes(data.quotaBytes)} used</span>
          </span>
          <span className="text-muted text-xs">
            {quotaPct.toFixed(1)}% • {formatBytes(Math.max(0, data.quotaBytes - data.usedBytes))} free
          </span>
        </div>
        <div
          className="h-3 w-full bg-bg rounded-full overflow-hidden flex border border-border"
          role="img"
          aria-label="storage breakdown"
        >
          {segments.map((s, i) => {
            const pct = data.quotaBytes > 0 ? (s.bytes / data.quotaBytes) * 100 : 0;
            if (pct < 0.2) return null; // too thin to render
            return (
              <div
                key={i}
                title={`${s.label}: ${formatBytes(s.bytes)}`}
                className="h-full transition-all"
                style={{ width: `${pct}%`, backgroundColor: s.color }}
              />
            );
          })}
          {headroomPct > 0.2 && (
            <div
              title={`Free: ${formatBytes(Math.max(0, data.quotaBytes - data.usedBytes))}`}
              className="h-full flex-1"
              style={{ backgroundColor: "transparent" }}
            />
          )}
        </div>
      </div>

      {/* Legend / per-bucket list */}
      <div className="space-y-1.5">
        <div className="text-[11px] uppercase tracking-wide text-muted">
          Where it&apos;s going
        </div>

        {buckets.map((b, i) => {
          if (b.totalBytes === 0) return null;
          const color = BUCKET_PALETTE[i % BUCKET_PALETTE.length];
          const live = b.originalBytes + b.transcodedBytes;
          return (
            <Link
              key={b.id}
              href={`/dashboard/buckets/${encodeURIComponent(b.name)}`}
              className="flex items-center gap-2 text-sm py-1 px-1.5 rounded hover:bg-bg group"
            >
              <span
                className="w-2.5 h-2.5 rounded-sm shrink-0"
                style={{ backgroundColor: color }}
              />
              <Database size={13} className="text-muted shrink-0" />
              <span className="font-mono truncate flex-1">{b.name}</span>
              {b.archived && (
                <span className="text-[10px] uppercase text-muted bg-bg px-1.5 rounded">
                  archived
                </span>
              )}
              <span className="font-mono text-xs text-muted tabular-nums">
                {formatBytes(live)}
              </span>
            </Link>
          );
        })}

        {data.reservedBytes > 0 && (
          <div className="flex items-center gap-2 text-sm py-1 px-1.5 rounded">
            <span className="w-2.5 h-2.5 rounded-sm shrink-0" style={{ backgroundColor: RESERVED_COLOR }} />
            <Hourglass size={13} className="text-muted shrink-0" />
            <span className="flex-1 text-muted">Transcode reservations (in-flight)</span>
            <span className="font-mono text-xs text-muted tabular-nums">
              {formatBytes(data.reservedBytes)}
            </span>
          </div>
        )}

        {data.versionsBytes > 0 && (
          <div className="flex items-center gap-2 text-sm py-1 px-1.5 rounded">
            <span className="w-2.5 h-2.5 rounded-sm shrink-0 border border-dashed border-muted" />
            <Layers size={13} className="text-muted shrink-0" />
            <span className="flex-1 text-muted">Older versions</span>
            <span className="font-mono text-xs text-muted tabular-nums">
              {formatBytes(data.versionsBytes)}
            </span>
          </div>
        )}

        {data.trashBytes > 0 && (
          <Link
            href="/dashboard/trash"
            className="flex items-center gap-2 text-sm py-1 px-1.5 rounded hover:bg-bg group"
          >
            <span className="w-2.5 h-2.5 rounded-sm shrink-0" style={{ backgroundColor: TRASH_COLOR }} />
            <Trash2 size={13} className="text-muted shrink-0" />
            <span className="flex-1">Trash <span className="text-muted text-xs">(empty to reclaim)</span></span>
            <span className="font-mono text-xs text-muted tabular-nums">
              {formatBytes(data.trashBytes)}
            </span>
          </Link>
        )}

        {totalShown === 0 && (
          <div className="text-xs text-muted italic py-2">
            No files yet — upload something to fill this in.
          </div>
        )}
      </div>

      {/* Drift hint when the breakdown sum doesn't match used_bytes — useful
          for spotting quota bugs early. */}
      {Math.abs(totalShown + data.versionsBytes - data.usedBytes) > 1024 * 1024 && (
        <div className="text-[11px] text-warning border-t border-border pt-2">
          <ArchiveRestore size={12} className="inline mr-1" />
          Recorded used ({formatBytes(data.usedBytes)}) doesn&apos;t match the breakdown
          ({formatBytes(totalShown + data.versionsBytes)}) — run
          <span className="font-mono"> scripts/quota-reconcile.sql</span> to fix.
        </div>
      )}
    </div>
  );
}
