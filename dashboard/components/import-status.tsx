"use client";

import { useEffect, useState } from "react";
import { api, ApiError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { X, CheckCircle2, XCircle, Loader2 } from "lucide-react";

interface ImportJob {
  id: string;
  bucket: string;
  key: string;
  sourceUrl: string;
  status: "pending" | "running" | "done" | "failed" | "cancelled";
  bytesDone: number;
  totalBytes?: number;
  throughputBps: number;
  error?: string;
  objectId?: string;
  createdAt: string;
  startedAt?: string;
  doneAt?: string;
}

interface Props {
  /** Only show imports targeting this bucket. If undefined, show all. */
  bucket?: string;
  /** Called when an import transitions to done so the parent can refresh listings. */
  onJobDone?: () => void;
}

/**
 * Live panel showing the user's in-progress imports.
 * Polls /imports?active=1 every 2 seconds. Survives tab close — when
 * the user comes back, the panel shows whatever's still running.
 */
export function ImportStatus({ bucket, onJobDone }: Props) {
  const [jobs, setJobs] = useState<ImportJob[]>([]);
  const [recentDone, setRecentDone] = useState<ImportJob[]>([]);
  const [knownDoneIDs] = useState<Set<string>>(new Set());

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | null = null;
    let cancelled = false;

    const tick = async () => {
      try {
        // Pull both active and last-20 (so we briefly see freshly-completed ones)
        const r = await api<{ imports: ImportJob[] }>("/imports?limit=20");
        if (cancelled) return;

        const filtered = bucket
          ? r.imports.filter((j) => j.bucket === bucket)
          : r.imports;

        const active = filtered.filter((j) => j.status === "pending" || j.status === "running");
        const finished = filtered.filter((j) => j.status !== "pending" && j.status !== "running");

        // Detect transitions to fire onJobDone
        for (const j of finished) {
          if (j.status === "done" && !knownDoneIDs.has(j.id)) {
            knownDoneIDs.add(j.id);
            onJobDone?.();
          }
        }

        setJobs(active);
        // Keep finished jobs visible briefly (last 3, recent first)
        setRecentDone(finished.slice(0, 3));
      } catch (e) {
        if (!(e instanceof ApiError && e.status === 401)) {
          console.warn("import poll failed", e);
        }
      } finally {
        if (!cancelled) timer = setTimeout(tick, 2000);
      }
    };

    void tick();
    return () => { cancelled = true; if (timer) clearTimeout(timer); };
  }, [bucket, onJobDone, knownDoneIDs]);

  if (jobs.length === 0 && recentDone.length === 0) return null;

  return (
    <div className="space-y-2">
      {jobs.length > 0 && (
        <div className="space-y-2">
          <p className="text-xs text-muted">Active imports ({jobs.length})</p>
          {jobs.map((j) => <ImportRow key={j.id} job={j} />)}
        </div>
      )}
      {recentDone.length > 0 && (
        <div className="space-y-2">
          <p className="text-xs text-muted">Recently finished</p>
          {recentDone.map((j) => <ImportRow key={j.id} job={j} />)}
        </div>
      )}
    </div>
  );
}

function ImportRow({ job: j }: { job: ImportJob }) {
  const pct = j.totalBytes && j.totalBytes > 0
    ? (j.bytesDone / j.totalBytes) * 100
    : 0;

  const etaSec = j.totalBytes && j.throughputBps > 0
    ? Math.max(0, (j.totalBytes - j.bytesDone) / j.throughputBps)
    : 0;

  const cancel = async () => {
    if (j.status === "running" || j.status === "pending") {
      if (!confirm("Cancel this import?")) return;
    }
    try {
      await api(`/imports/${j.id}`, { method: "DELETE" });
    } catch (e) {
      alert(e instanceof ApiError ? e.message : "failed");
    }
  };

  const barColor =
    j.status === "done"      ? "bg-success" :
    j.status === "failed"    ? "bg-danger"  :
    j.status === "cancelled" ? "bg-muted"   : "bg-accent";

  const StatusIcon =
    j.status === "done"      ? <CheckCircle2 size={14} className="text-success" /> :
    j.status === "failed"    ? <XCircle size={14} className="text-danger" /> :
    j.status === "cancelled" ? <XCircle size={14} className="text-muted" /> :
                               <Loader2 size={14} className="text-accent animate-spin" />;

  return (
    <div className="bg-panel border border-border rounded p-3">
      <div className="flex items-center justify-between gap-2 text-sm">
        <div className="flex items-center gap-2 flex-1 min-w-0">
          {StatusIcon}
          <div className="min-w-0 flex-1">
            <p className="font-mono truncate text-xs">{j.bucket}/{j.key}</p>
            <p className="text-[10px] text-muted truncate">{j.sourceUrl}</p>
          </div>
        </div>
        <div className="text-right text-xs text-muted whitespace-nowrap">
          {j.status === "running" && (
            <>
              {j.totalBytes
                ? <>{formatBytes(j.bytesDone)} / {formatBytes(j.totalBytes)}</>
                : <>{formatBytes(j.bytesDone)} (unknown total)</>}
              {j.throughputBps > 0 && <>  · <span className="text-accent">{formatBytes(j.throughputBps)}/s</span></>}
              {etaSec > 1 && etaSec < 86400 && <>  · ETA {fmtEta(etaSec)}</>}
            </>
          )}
          {j.status === "done" && <>{formatBytes(j.bytesDone)} done</>}
          {j.status === "failed" && <span className="text-danger">{j.error || "failed"}</span>}
          {j.status === "cancelled" && <>cancelled</>}
          {j.status === "pending" && <>queued...</>}
        </div>
        <button
          onClick={cancel}
          className="text-muted hover:text-danger shrink-0"
          title={j.status === "running" || j.status === "pending" ? "Cancel" : "Dismiss"}
        >
          <X size={14} />
        </button>
      </div>
      {(j.status === "running" || j.status === "pending") && j.totalBytes && (
        <div className="h-1 bg-border rounded overflow-hidden mt-2">
          <div className={`h-full ${barColor} transition-all`} style={{ width: `${pct}%` }} />
        </div>
      )}
    </div>
  );
}

function fmtEta(seconds: number): string {
  if (seconds < 60) return `${Math.round(seconds)}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${Math.round(seconds % 60)}s`;
  return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`;
}
