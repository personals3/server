"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api, ApiError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { Card } from "@/components/ui/card";
import { Loader2, Upload, Download, Film, X, AlertCircle } from "lucide-react";

interface ActiveUpload {
  uploadId: string;
  bucket: string;
  key: string;
  totalBytes: number;
  partsDone: number;
  createdAt: string;
}
interface ActiveImport {
  id: string;
  bucket: string;
  key: string;
  status: string;
  bytesDone: number;
  totalBytes?: number;
  throughputBps: number;
}
interface ActiveTranscode {
  jobId: string;
  bucket: string;
  key: string;
  fileType: string;
  status: string;
  attempts: number;
  progressPct: number;
}

interface ActiveResponse {
  uploads:    ActiveUpload[];
  imports:    ActiveImport[];
  transcodes: ActiveTranscode[];
}

/**
 * "What's happening right now" — three concurrent streams, one panel.
 *   - Uploads:    in-progress multipart sessions (survive page refresh; can be
 *                 aborted to refund quota even if the browser tab that
 *                 started them is gone).
 *   - Imports:    server-side URL downloads (already persistent).
 *   - Transcodes: ffmpeg jobs queued / running.
 *
 * Polls every 3s. Hides itself when nothing's happening.
 */
export function ActiveOps() {
  const [data, setData] = useState<ActiveResponse | null>(null);
  const [refreshTrigger, setRefreshTrigger] = useState(0);

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const tick = async () => {
      try {
        const r = await api<ActiveResponse>("/auth/active");
        if (!cancelled) setData(r);
      } catch { /* silent */ }
      finally { if (!cancelled) timer = setTimeout(tick, 3000); }
    };
    void tick();
    return () => { cancelled = true; if (timer) clearTimeout(timer); };
  }, [refreshTrigger]);

  if (!data) return null;
  const total = data.uploads.length + data.imports.length + data.transcodes.length;
  if (total === 0) return null;

  const refresh = () => setRefreshTrigger((n) => n + 1);

  return (
    <Card>
      <div className="flex items-center justify-between mb-3">
        <h2 className="text-sm font-semibold flex items-center gap-2">
          <Loader2 size={14} className="text-accent animate-spin" />
          Active operations
        </h2>
        <span className="text-xs text-muted">{total} running</span>
      </div>

      <div className="space-y-2">
        {data.uploads.map((u) => <UploadRow key={u.uploadId} u={u} onChange={refresh} />)}
        {data.imports.map((i) => <ImportRow key={i.id} i={i} />)}
        {data.transcodes.map((t) => <TranscodeRow key={t.jobId} t={t} />)}
      </div>

      {data.uploads.length > 0 && (
        <p className="text-[11px] text-muted mt-3 flex items-start gap-1">
          <AlertCircle size={11} className="mt-0.5 shrink-0" />
          <span>
            Browser folder uploads can&apos;t resume after a refresh — the file handles
            are gone. Aborting an abandoned upload here refunds the quota it reserved.
          </span>
        </p>
      )}
    </Card>
  );
}

function UploadRow({ u, onChange }: { u: ActiveUpload; onChange: () => void }) {
  const abort = async () => {
    if (!confirm(`Abort upload of "${u.key}"? Already-uploaded parts will be deleted and quota refunded.`)) return;
    try {
      await api(`/auth/active/uploads/${u.uploadId}`, { method: "DELETE" });
      onChange();
    } catch (e) {
      alert(e instanceof ApiError ? e.message : "abort failed");
    }
  };
  const ageMs = Date.now() - new Date(u.createdAt).getTime();
  const isStale = ageMs > 5 * 60 * 1000;  // > 5 min likely abandoned
  return (
    <div className="flex items-center gap-2 text-xs bg-bg border border-border rounded p-2">
      <Upload size={12} className={isStale ? "text-warning" : "text-accent shrink-0"} />
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2">
          <span className="font-mono truncate" title={`${u.bucket}/${u.key}`}>
            <Link href={`/dashboard/buckets/${u.bucket}`} className="hover:text-accent">
              {u.bucket}/{u.key}
            </Link>
          </span>
          {isStale && (
            <span className="text-[10px] text-warning shrink-0">
              abandoned ({Math.floor(ageMs / 60000)}m)
            </span>
          )}
        </div>
        <div className="text-[11px] text-muted">
          upload · {u.partsDone} part{u.partsDone === 1 ? "" : "s"} received
          {u.totalBytes > 0 && <> · {formatBytes(u.totalBytes)} held</>}
        </div>
      </div>
      <button onClick={abort} className="text-muted hover:text-danger shrink-0" title="Abort + refund">
        <X size={12} />
      </button>
    </div>
  );
}

function ImportRow({ i }: { i: ActiveImport }) {
  const pct = i.totalBytes && i.totalBytes > 0 ? (i.bytesDone / i.totalBytes) * 100 : 0;
  return (
    <div className="bg-bg border border-border rounded p-2">
      <div className="flex items-center gap-2 text-xs">
        <Download size={12} className="text-accent animate-pulse shrink-0" />
        <div className="flex-1 min-w-0">
          <div className="font-mono truncate" title={`${i.bucket}/${i.key}`}>
            <Link href={`/dashboard/buckets/${i.bucket}`} className="hover:text-accent">
              {i.bucket}/{i.key}
            </Link>
          </div>
          <div className="text-[11px] text-muted">
            import · {i.status}
            {" · "}{formatBytes(i.bytesDone)}{i.totalBytes ? ` / ${formatBytes(i.totalBytes)}` : ""}
            {i.throughputBps > 0 && <> · <span className="text-accent">{formatBytes(i.throughputBps)}/s</span></>}
          </div>
        </div>
      </div>
      {i.totalBytes && (
        <div className="h-0.5 bg-border rounded mt-1 overflow-hidden">
          <div className="h-full bg-accent transition-all" style={{ width: `${pct}%` }} />
        </div>
      )}
    </div>
  );
}

function prettifyFileType(ft: string): string {
  const m = /^video_quality_(\d+p)$/.exec(ft);
  if (m) return `${m[1]} video`;
  if (ft === "video_thumbnails") return "thumbnails";
  if (ft === "video_finalize")   return "finalize (writing master.m3u8)";
  return ft;
}

function TranscodeRow({ t }: { t: ActiveTranscode }) {
  const isRunning = t.status === "processing";
  return (
    <div className="bg-bg border border-border rounded p-2">
      <div className="flex items-center gap-2 text-xs">
        <Film size={12} className="text-warning shrink-0" />
        <div className="flex-1 min-w-0">
          <div className="font-mono truncate" title={`${t.bucket}/${t.key}`}>
            <Link href={`/dashboard/buckets/${t.bucket}`} className="hover:text-accent">
              {t.bucket}/{t.key}
            </Link>
          </div>
          <div className="text-[11px] text-muted">
            transcode {prettifyFileType(t.fileType)} · {t.status}
            {isRunning && ` · ${t.progressPct}%`}
            {t.attempts > 1 && ` (attempt ${t.attempts})`}
          </div>
        </div>
      </div>
      {isRunning && (
        <div className="h-0.5 bg-border rounded mt-1 overflow-hidden">
          <div className="h-full bg-warning transition-all duration-500"
               style={{ width: `${t.progressPct}%` }} />
        </div>
      )}
    </div>
  );
}
