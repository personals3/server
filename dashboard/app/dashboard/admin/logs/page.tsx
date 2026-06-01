"use client";

// Unified logs viewer — Cloudflare-console style.
//
// Header strip: title + tab pills + a single live-tail indicator that
// counts how many tabs are currently streaming.
// Each tab gets a small toolbar (live / filters / paging) and a single
// dense table. Quota events tab has two modes: live SSE stream OR
// paginated history from the quota_events table.

import { useEffect, useRef, useState } from "react";
import { api, API, getToken } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { formatBytes, formatDate } from "@/lib/format";
import {
  Pause, Play, Filter, FileSearch, Sparkles, FileVideo, Activity, ScrollText, Radio,
  ChevronLeft, ChevronRight,
} from "lucide-react";

type Tab = "audit" | "cleanup" | "transcode" | "quota-events";

const TABS: { id: Tab; label: string; icon: React.ReactNode; description: string }[] = [
  { id: "audit",        label: "API audit",  icon: <FileSearch size={13} />, description: "Every authenticated HTTP request" },
  { id: "cleanup",      label: "Cleaner",    icon: <Sparkles   size={13} />, description: "Each cleaner tick and what it reaped" },
  { id: "transcode",    label: "Transcodes", icon: <FileVideo  size={13} />, description: "Per-rung worker pipeline state" },
  { id: "quota-events", label: "Quota",      icon: <Activity   size={13} />, description: "Charge/refund events — live stream and full history" },
];

const PAGE_SIZES = [50, 100, 200];
const DEFAULT_PAGE_SIZE = 100;

export default function LogsPage() {
  const [tab, setTab] = useState<Tab>("audit");
  const active = TABS.find((t) => t.id === tab)!;
  return (
    <div className="space-y-5">
      <div className="border-b border-border pb-4">
        <div className="flex items-center gap-2 mb-1">
          <ScrollText size={18} className="text-accent" />
          <h1 className="text-xl font-semibold">Logs</h1>
        </div>
        <p className="text-sm text-muted">{active.description}</p>
      </div>

      <div className="flex gap-1 -mb-px">
        {TABS.map((t) => {
          const isActive = t.id === tab;
          return (
            <button
              key={t.id}
              onClick={() => setTab(t.id)}
              className={
                "px-3 py-2 text-sm font-medium flex items-center gap-1.5 rounded-t-md border " +
                (isActive
                  ? "border-border border-b-panel bg-panel text-text"
                  : "border-transparent text-muted hover:text-text hover:bg-bg/50")
              }
            >
              {t.icon} {t.label}
            </button>
          );
        })}
      </div>

      <div className="-mt-5">
        {tab === "audit"        && <AuditPanel />}
        {tab === "cleanup"      && <CleanupPanel />}
        {tab === "transcode"    && <TranscodePanel />}
        {tab === "quota-events" && <QuotaEventsPanel />}
      </div>
    </div>
  );
}

// ============================================================================
// Shared bits
// ============================================================================

function PanelCard({ children }: { children: React.ReactNode }) {
  return (
    <div className="bg-panel border border-border rounded-lg rounded-tl-none">
      {children}
    </div>
  );
}

function Toolbar({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex items-center gap-2 px-4 py-2.5 border-b border-border bg-bg/30 rounded-t-lg rounded-tl-none flex-wrap">
      {children}
    </div>
  );
}

function LiveButton({ live, onToggle }: { live: boolean; onToggle: () => void }) {
  return (
    <button
      onClick={onToggle}
      className={
        "inline-flex items-center gap-1.5 px-2 py-1 text-xs rounded-md border transition-colors " +
        (live
          ? "border-success/40 text-success bg-success/5"
          : "border-border text-muted hover:text-text")
      }
    >
      {live ? <Radio size={11} className="animate-pulse" /> : <Pause size={11} />}
      {live ? "Live" : "Paused"}
    </button>
  );
}

function StatusPill({ children, variant }: { children: React.ReactNode; variant: "success" | "danger" | "warning" | "info" | "muted" }) {
  const colors = {
    success: "bg-success/10 text-success border-success/30",
    danger:  "bg-danger/10 text-danger border-danger/30",
    warning: "bg-warning/10 text-warning border-warning/30",
    info:    "bg-accent/10 text-accent border-accent/30",
    muted:   "bg-bg text-muted border-border",
  };
  return (
    <span className={`inline-flex items-center px-1.5 py-0.5 rounded text-[10px] font-medium border ${colors[variant]}`}>
      {children}
    </span>
  );
}

function statusVariant(s: number): "success" | "danger" | "warning" | "muted" {
  if (s >= 500) return "danger";
  if (s >= 400) return "warning";
  if (s >= 300) return "muted";
  return "success";
}

function transcodeVariant(s: string): "success" | "danger" | "warning" | "info" | "muted" {
  if (s === "failed" || s === "failed_quota") return "danger";
  if (s === "done") return "success";
  if (s === "processing") return "info";
  if (s === "skipped" || s === "skipped_quota") return "warning";
  return "muted";
}

// Shared pager — left/right arrows, "Page X of Y", page-size selector.
// totalRows is what the server says; rendered rows is whatever the panel
// scrolled in. Stays muted when there's only one page.
function Pager({
  total, limit, offset, onJump, onPageSize,
}: {
  total: number; limit: number; offset: number;
  onJump: (newOffset: number) => void;
  onPageSize: (sz: number) => void;
}) {
  const page       = Math.floor(offset / limit) + 1;
  const totalPages = Math.max(1, Math.ceil(total / limit));
  const lo         = total === 0 ? 0 : offset + 1;
  const hi         = Math.min(offset + limit, total);
  const prev       = () => onJump(Math.max(0, offset - limit));
  const next       = () => onJump(offset + limit);
  const first      = () => onJump(0);
  const last       = () => onJump((totalPages - 1) * limit);
  return (
    <div className="flex items-center gap-2 text-[11px] text-muted">
      <span className="font-mono whitespace-nowrap">
        {lo}–{hi} of {total.toLocaleString()}
      </span>
      <div className="flex items-center gap-0.5 ml-2">
        <button
          onClick={first} disabled={offset === 0}
          className="px-1.5 py-1 rounded hover:bg-surface disabled:opacity-30 disabled:hover:bg-transparent"
          title="First page"
        >«</button>
        <button
          onClick={prev} disabled={offset === 0}
          className="p-1 rounded hover:bg-surface disabled:opacity-30 disabled:hover:bg-transparent"
          title="Previous page"
        ><ChevronLeft size={12} /></button>
        <span className="px-2 font-mono whitespace-nowrap">Page {page} / {totalPages}</span>
        <button
          onClick={next} disabled={hi >= total}
          className="p-1 rounded hover:bg-surface disabled:opacity-30 disabled:hover:bg-transparent"
          title="Next page"
        ><ChevronRight size={12} /></button>
        <button
          onClick={last} disabled={page >= totalPages}
          className="px-1.5 py-1 rounded hover:bg-surface disabled:opacity-30 disabled:hover:bg-transparent"
          title="Last page"
        >»</button>
      </div>
      <select
        value={limit}
        onChange={(e) => onPageSize(Number(e.target.value))}
        className="ml-1 bg-bg border border-border rounded px-1.5 py-0.5 text-[11px]"
        title="Rows per page"
      >
        {PAGE_SIZES.map((n) => <option key={n} value={n}>{n}/page</option>)}
      </select>
    </div>
  );
}

// ============================================================================
// Audit panel
// ============================================================================

interface AuditEntry {
  id: string;
  userId: string;
  userEmail: string;
  action: string;
  bucket?: string;
  key?: string;
  sizeBytes?: number;
  statusCode: number;
  ipAddress: string;
  ts: string;
}

function AuditPanel() {
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [total, setTotal]   = useState(0);
  const [filter, setFilter] = useState({ user: "", action: "" });
  const [live, setLive]     = useState(true);
  const [limit, setLimit]   = useState(DEFAULT_PAGE_SIZE);
  const [offset, setOffset] = useState(0);

  useEffect(() => {
    const load = () => {
      const q = new URLSearchParams();
      if (filter.user) q.set("user", filter.user);
      if (filter.action) q.set("action", filter.action);
      q.set("limit", String(limit));
      q.set("offset", String(offset));
      void api<{ entries: AuditEntry[]; total: number }>(`/admin/audit?${q}`).then((r) => {
        setEntries(r.entries);
        setTotal(r.total);
      });
    };
    load();
    if (!live) return;
    const id = setInterval(load, 4000);
    return () => clearInterval(id);
  }, [filter, live, limit, offset]);

  // Filter changes always reset to page 1 — otherwise the offset could
  // exceed total and we'd show "nothing matches" on what's actually page 5.
  useEffect(() => { setOffset(0); }, [filter, limit]);

  return (
    <PanelCard>
      <Toolbar>
        <LiveButton live={live} onToggle={() => setLive(!live)} />
        <span className="ml-2 inline-flex items-center gap-1 text-[11px] text-muted"><Filter size={10} /></span>
        <Input
          placeholder="filter by email..."
          value={filter.user}
          onChange={(e) => setFilter({ ...filter, user: e.target.value })}
          className="h-7 text-xs w-44"
        />
        <Input
          placeholder="action (e.g. PUT)"
          value={filter.action}
          onChange={(e) => setFilter({ ...filter, action: e.target.value })}
          className="h-7 text-xs w-32"
        />
        <div className="ml-auto">
          <Pager total={total} limit={limit} offset={offset}
                 onJump={setOffset} onPageSize={setLimit} />
        </div>
      </Toolbar>
      <div className="overflow-x-auto">
        <table className="w-full text-xs">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-wider text-muted border-b border-border">
              <th className="px-4 py-2 font-semibold">When</th>
              <th className="px-2 py-2 font-semibold">User</th>
              <th className="px-2 py-2 font-semibold">Action</th>
              <th className="px-2 py-2 font-semibold">Target</th>
              <th className="px-2 py-2 font-semibold">Status</th>
              <th className="px-2 py-2 font-semibold">Size</th>
              <th className="px-4 py-2 font-semibold">IP</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((e) => (
              <tr key={e.id} className="border-b border-border/40 hover:bg-bg/50">
                <td className="px-4 py-1.5 text-muted whitespace-nowrap font-mono text-[11px]">{formatDate(e.ts)}</td>
                <td className="px-2 py-1.5 truncate max-w-[14ch]" title={e.userEmail}>{e.userEmail || "—"}</td>
                <td className="px-2 py-1.5 font-mono text-[11px]">{e.action}</td>
                <td className="px-2 py-1.5 font-mono text-[11px] truncate max-w-[40ch]" title={`${e.bucket || ""}/${e.key || ""}`}>
                  {e.bucket ? `${e.bucket}${e.key ? "/" + e.key : ""}` : "—"}
                </td>
                <td className="px-2 py-1.5"><StatusPill variant={statusVariant(e.statusCode)}>{e.statusCode}</StatusPill></td>
                <td className="px-2 py-1.5 text-muted text-[11px]">{e.sizeBytes ? formatBytes(e.sizeBytes) : ""}</td>
                <td className="px-4 py-1.5 text-muted font-mono text-[11px]">{e.ipAddress}</td>
              </tr>
            ))}
            {entries.length === 0 && (
              <tr><td colSpan={7} className="py-10 text-center text-muted italic text-xs">no entries match the current filter</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </PanelCard>
  );
}

// ============================================================================
// Cleanup panel
// ============================================================================

interface CleanupRun {
  id: string;
  startedAt: string;
  finishedAt: string | null;
  dryRun: boolean;
  bytesFreed: number;
  reapedCounts: Record<string, number>;
  errors: string[];
  logPath: string;
}

function CleanupPanel() {
  const [runs, setRuns]     = useState<CleanupRun[]>([]);
  const [total, setTotal]   = useState(0);
  const [live, setLive]     = useState(true);
  const [limit, setLimit]   = useState(DEFAULT_PAGE_SIZE);
  const [offset, setOffset] = useState(0);

  useEffect(() => {
    const load = () => {
      const q = new URLSearchParams();
      q.set("limit", String(limit));
      q.set("offset", String(offset));
      void api<{ runs: CleanupRun[]; total: number }>(`/admin/cleanup?${q}`).then((r) => {
        setRuns(r.runs);
        setTotal(r.total);
      });
    };
    load();
    if (!live) return;
    const id = setInterval(load, 5000);
    return () => clearInterval(id);
  }, [live, limit, offset]);

  useEffect(() => { setOffset(0); }, [limit]);

  return (
    <PanelCard>
      <Toolbar>
        <LiveButton live={live} onToggle={() => setLive(!live)} />
        <div className="ml-auto">
          <Pager total={total} limit={limit} offset={offset}
                 onJump={setOffset} onPageSize={setLimit} />
        </div>
      </Toolbar>
      <div className="overflow-x-auto">
        <table className="w-full text-xs">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-wider text-muted border-b border-border">
              <th className="px-4 py-2 font-semibold">When</th>
              <th className="px-2 py-2 font-semibold">Mode</th>
              <th className="px-2 py-2 font-semibold">What it did</th>
              <th className="px-2 py-2 font-semibold">Freed</th>
              <th className="px-4 py-2 font-semibold">Errors</th>
            </tr>
          </thead>
          <tbody>
            {runs.map((r) => (
              <tr key={r.id} className="border-b border-border/40 hover:bg-bg/50">
                <td className="px-4 py-1.5 text-muted whitespace-nowrap font-mono text-[11px]">{formatDate(r.startedAt)}</td>
                <td className="px-2 py-1.5">
                  <StatusPill variant={r.dryRun ? "warning" : "success"}>
                    {r.dryRun ? "DRY" : "LIVE"}
                  </StatusPill>
                </td>
                <td className="px-2 py-1.5 text-[12px]">{summarizeCounts(r.reapedCounts)}</td>
                <td className="px-2 py-1.5 font-mono text-[11px]">{r.bytesFreed > 0 ? formatBytes(r.bytesFreed) : <span className="text-muted">—</span>}</td>
                <td className="px-4 py-1.5 text-danger text-[11px] truncate max-w-[40ch]" title={r.errors[0]}>{r.errors.length > 0 ? r.errors[0] : ""}</td>
              </tr>
            ))}
            {runs.length === 0 && (
              <tr><td colSpan={5} className="py-10 text-center text-muted italic text-xs">no cleaner runs yet</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </PanelCard>
  );
}

// ============================================================================
// Transcode panel
// ============================================================================

interface TranscodeJob {
  id: string;
  objectId: string;
  fileType: string;
  status: string;
  attempts: number;
  priority: number;
  progressPct: number;
  error: string;
  createdAt: string;
  startedAt?: string;
  doneAt?: string;
  key: string;
  bucket: string;
}

function TranscodePanel() {
  const [jobs, setJobs]     = useState<TranscodeJob[]>([]);
  const [total, setTotal]   = useState(0);
  const [status, setStatus] = useState("");
  const [live, setLive]     = useState(true);
  const [limit, setLimit]   = useState(DEFAULT_PAGE_SIZE);
  const [offset, setOffset] = useState(0);

  useEffect(() => {
    const load = () => {
      const q = new URLSearchParams();
      if (status) q.set("status", status);
      q.set("limit", String(limit));
      q.set("offset", String(offset));
      void api<{ jobs: TranscodeJob[]; total: number }>(`/admin/transcode-jobs?${q}`).then((r) => {
        setJobs(r.jobs);
        setTotal(r.total);
      });
    };
    load();
    if (!live) return;
    const id = setInterval(load, 4000);
    return () => clearInterval(id);
  }, [status, live, limit, offset]);

  useEffect(() => { setOffset(0); }, [status, limit]);

  return (
    <PanelCard>
      <Toolbar>
        <LiveButton live={live} onToggle={() => setLive(!live)} />
        <select
          value={status}
          onChange={(e) => setStatus(e.target.value)}
          className="px-2 py-1 bg-bg border border-border rounded text-xs text-text"
        >
          <option value="">all statuses</option>
          <option value="pending">pending</option>
          <option value="processing">processing</option>
          <option value="done">done</option>
          <option value="failed">failed</option>
          <option value="skipped">skipped</option>
        </select>
        <div className="ml-auto">
          <Pager total={total} limit={limit} offset={offset}
                 onJump={setOffset} onPageSize={setLimit} />
        </div>
      </Toolbar>
      <div className="overflow-x-auto">
        <table className="w-full text-xs">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-wider text-muted border-b border-border">
              <th className="px-4 py-2 font-semibold">Created</th>
              <th className="px-2 py-2 font-semibold">Job</th>
              <th className="px-2 py-2 font-semibold">Bucket / Key</th>
              <th className="px-2 py-2 font-semibold">Status</th>
              <th className="px-2 py-2 font-semibold">Attempt</th>
              <th className="px-4 py-2 font-semibold">Progress / Error</th>
            </tr>
          </thead>
          <tbody>
            {jobs.map((j) => (
              <tr key={j.id} className="border-b border-border/40 hover:bg-bg/50">
                <td className="px-4 py-1.5 text-muted whitespace-nowrap font-mono text-[11px]">{formatDate(j.createdAt)}</td>
                <td className="px-2 py-1.5 font-mono text-muted text-[11px]">{j.fileType}</td>
                <td className="px-2 py-1.5 font-mono text-[11px] truncate max-w-[40ch]" title={`${j.bucket}/${j.key}`}>
                  {j.bucket}/{j.key}
                </td>
                <td className="px-2 py-1.5"><StatusPill variant={transcodeVariant(j.status)}>{j.status}</StatusPill></td>
                <td className="px-2 py-1.5 text-muted text-[11px]">{j.attempts}</td>
                <td className="px-4 py-1.5 text-muted text-[11px]">
                  {j.status === "processing" && (
                    <div className="flex items-center gap-2 max-w-[28ch]">
                      <div className="flex-1 h-1 bg-bg rounded-full overflow-hidden">
                        <div className="h-full bg-accent transition-all" style={{ width: `${j.progressPct}%` }} />
                      </div>
                      <span className="font-mono text-[10px]">{j.progressPct}%</span>
                    </div>
                  )}
                  {j.error && j.status !== "processing" && (
                    <span className="text-danger truncate" title={j.error}>{j.error.slice(0, 60)}</span>
                  )}
                </td>
              </tr>
            ))}
            {jobs.length === 0 && (
              <tr><td colSpan={6} className="py-10 text-center text-muted italic text-xs">no jobs match</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </PanelCard>
  );
}

// ============================================================================
// Quota events — two modes:
//   live    → SSE tail of /admin/quota-events (in-memory, drops if slow)
//   history → paginated read of the quota_events table
// ============================================================================

interface LiveEvent {
  ts: string;
  userId: string;
  delta: number;
  newBytes: number;
  caller: string;
  rejected?: boolean;
}

interface HistEvent extends LiveEvent {
  userEmail: string;
}

function QuotaEventsPanel() {
  const [mode, setMode] = useState<"live" | "history">("history");
  return (
    <PanelCard>
      <Toolbar>
        <div className="inline-flex border border-border rounded overflow-hidden text-xs">
          <button
            onClick={() => setMode("history")}
            className={"px-2.5 py-1 " + (mode === "history"
              ? "bg-surface text-text font-medium"
              : "text-muted hover:text-text")}
          >History</button>
          <button
            onClick={() => setMode("live")}
            className={"px-2.5 py-1 border-l border-border " + (mode === "live"
              ? "bg-surface text-text font-medium"
              : "text-muted hover:text-text")}
          >Live tail</button>
        </div>
        <span className="text-[11px] text-muted">
          {mode === "live"
            ? <>Every <span className="font-mono text-text/80">QuotaReserve</span> as it happens</>
            : <>Persisted in the <span className="font-mono text-text/80">quota_events</span> table</>}
        </span>
      </Toolbar>
      {mode === "live" ? <QuotaLive /> : <QuotaHistory />}
    </PanelCard>
  );
}

function QuotaLive() {
  const [events, setEvents] = useState<LiveEvent[]>([]);
  const [paused, setPaused] = useState(false);
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  useEffect(() => {
    const ctrl = new AbortController();
    (async () => {
      const token = getToken();
      const resp = await fetch(`${API}/admin/quota-events`, {
        headers: { Authorization: `Bearer ${token}` },
        signal: ctrl.signal,
      });
      if (!resp.body) return;
      const reader = resp.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      while (!ctrl.signal.aborted) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const lines = buf.split("\n");
        buf = lines.pop() || "";
        for (const ln of lines) {
          if (!ln.startsWith("data: ")) continue;
          if (pausedRef.current) continue;
          try {
            const ev = JSON.parse(ln.slice(6)) as LiveEvent;
            setEvents((prev) => [ev, ...prev].slice(0, 500));
          } catch {}
        }
      }
    })().catch(() => {});
    return () => ctrl.abort();
  }, []);

  return (
    <>
      <div className="flex items-center gap-2 px-4 py-2 border-b border-border bg-bg/20 text-[11px] text-muted">
        <LiveButton live={!paused} onToggle={() => setPaused(!paused)} />
        <span className="ml-auto">{events.length} buffered (last 500)</span>
      </div>
      <div className="font-mono text-[11px] max-h-[65vh] overflow-y-auto p-3 space-y-0.5">
        {events.map((e, i) => (
          <div key={i} className="grid grid-cols-[10ch_8ch_12ch_14ch_9ch_1fr] gap-2 items-center hover:bg-bg/40 rounded px-1.5 py-0.5">
            <span className="text-muted">{new Date(e.ts).toLocaleTimeString()}</span>
            <span className={
              e.rejected ? "text-danger font-semibold"
              : e.delta < 0 ? "text-muted font-semibold"
              : "text-success font-semibold"
            }>
              {e.rejected ? "REJECT" : e.delta < 0 ? "REFUND" : "CHARGE"}
            </span>
            <span className="text-right">{e.delta > 0 ? "+" : ""}{formatBytes(Math.abs(e.delta))}</span>
            <span className="text-muted">{!e.rejected && <>→ {formatBytes(e.newBytes)}</>}</span>
            <span className="text-muted truncate" title={e.userId}>{e.userId.slice(0, 8)}</span>
            <span className="text-accent truncate" title={e.caller}>{e.caller}</span>
          </div>
        ))}
        {events.length === 0 && (
          <div className="text-muted italic py-10 text-center">listening for events…</div>
        )}
      </div>
    </>
  );
}

function QuotaHistory() {
  const [events, setEvents] = useState<HistEvent[]>([]);
  const [total, setTotal]   = useState(0);
  const [user, setUser]     = useState("");
  const [limit, setLimit]   = useState(DEFAULT_PAGE_SIZE);
  const [offset, setOffset] = useState(0);

  useEffect(() => {
    const q = new URLSearchParams();
    if (user) q.set("user", user);
    q.set("limit", String(limit));
    q.set("offset", String(offset));
    void api<{ events: HistEvent[]; total: number }>(`/admin/quota-events/history?${q}`).then((r) => {
      setEvents(r.events);
      setTotal(r.total);
    });
  }, [user, limit, offset]);

  useEffect(() => { setOffset(0); }, [user, limit]);

  return (
    <>
      <div className="flex items-center gap-2 px-4 py-2 border-b border-border bg-bg/20 flex-wrap">
        <Filter size={11} className="text-muted" />
        <Input
          placeholder="filter by user email..."
          value={user}
          onChange={(e) => setUser(e.target.value)}
          className="h-7 text-xs w-52"
        />
        <div className="ml-auto">
          <Pager total={total} limit={limit} offset={offset}
                 onJump={setOffset} onPageSize={setLimit} />
        </div>
      </div>
      <div className="overflow-x-auto">
        <table className="w-full text-xs">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-wider text-muted border-b border-border">
              <th className="px-4 py-2 font-semibold">When</th>
              <th className="px-2 py-2 font-semibold">User</th>
              <th className="px-2 py-2 font-semibold">Kind</th>
              <th className="px-2 py-2 font-semibold text-right">Delta</th>
              <th className="px-2 py-2 font-semibold">After</th>
              <th className="px-4 py-2 font-semibold">Caller</th>
            </tr>
          </thead>
          <tbody>
            {events.map((e, i) => (
              <tr key={i} className="border-b border-border/40 hover:bg-bg/50">
                <td className="px-4 py-1.5 text-muted whitespace-nowrap font-mono text-[11px]">{formatDate(e.ts)}</td>
                <td className="px-2 py-1.5 truncate max-w-[18ch]" title={e.userEmail || e.userId}>
                  {e.userEmail || <span className="text-muted font-mono">{e.userId.slice(0, 8)}</span>}
                </td>
                <td className="px-2 py-1.5">
                  <StatusPill variant={e.rejected ? "danger" : e.delta < 0 ? "muted" : "success"}>
                    {e.rejected ? "REJECT" : e.delta < 0 ? "REFUND" : "CHARGE"}
                  </StatusPill>
                </td>
                <td className="px-2 py-1.5 text-right font-mono text-[11px]">
                  {e.delta > 0 ? "+" : ""}{formatBytes(Math.abs(e.delta))}
                </td>
                <td className="px-2 py-1.5 text-muted font-mono text-[11px]">
                  {e.rejected ? "—" : formatBytes(e.newBytes)}
                </td>
                <td className="px-4 py-1.5 text-accent font-mono text-[11px] truncate max-w-[28ch]" title={e.caller}>
                  {e.caller}
                </td>
              </tr>
            ))}
            {events.length === 0 && (
              <tr><td colSpan={6} className="py-10 text-center text-muted italic text-xs">
                no events recorded yet — they'll start showing up after the next quota charge
              </td></tr>
            )}
          </tbody>
        </table>
      </div>
    </>
  );
}

// ----------------------------------------------------------------------------

function summarizeCounts(counts: Record<string, number>): string {
  const entries = Object.entries(counts).filter(([, n]) => n > 0);
  if (entries.length === 0) return "nothing notable";
  return entries.map(([k, n]) => `${n}× ${k.replace(/_/g, " ")}`).join(" · ");
}
