"use client";

import { useCallback, useEffect, useState } from "react";
import { api, ApiError } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import Link from "next/link";
import { RefreshCw, Play, AlertTriangle, ChevronRight, ChevronDown, Network } from "lucide-react";
import { formatBytes, formatDate } from "@/lib/format";
import { useToast } from "@/components/toast";

interface CleanupRun {
  id: string;
  startedAt: string;
  finishedAt?: string;
  durationMs: number;
  dryRun: boolean;
  bytesFreed: number;
  reapedCounts: Record<string, number>;
  errors: string[];
  logPath: string;
}

interface ListResp {
  runs: CleanupRun[];
  window: {
    runs: number;
    bytesFreed: number;
    reapedCounts: Record<string, number>;
  };
}

interface EventEntry {
  ts: string;
  runId: string;
  action: string;
  targetType: string;
  targetRef: string;
  reason: string;
  bytesFreed: number;
  dryRun: boolean;
}

export default function AdminCleanupPage() {
  const toast = useToast();
  const [data, setData] = useState<ListResp | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [events, setEvents] = useState<Record<string, EventEntry[]>>({});

  const load = useCallback(async () => {
    setBusy(true);
    setErr(null);
    try {
      const r = await api<ListResp>("/admin/cleanup");
      // Normalize: API may serialize empty slices/maps as null; defend so the
      // render code can rely on .length / Object.entries everywhere.
      setData({
        runs: (r.runs ?? []).map((run) => ({
          ...run,
          errors:       run.errors ?? [],
          reapedCounts: run.reapedCounts ?? {},
        })),
        window: {
          runs:         r.window?.runs ?? 0,
          bytesFreed:   r.window?.bytesFreed ?? 0,
          reapedCounts: r.window?.reapedCounts ?? {},
        },
      });
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "failed");
    } finally {
      setBusy(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  const runNow = async () => {
    try {
      await api("/admin/cleanup/run", { method: "POST" });
      toast.push("success", "Cleaner notified — next tick will start shortly.");
      // Refresh after a moment to pick up the new run.
      setTimeout(() => void load(), 3000);
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "failed");
    }
  };

  const toggleRun = async (runID: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(runID)) next.delete(runID);
      else next.add(runID);
      return next;
    });
    if (!events[runID]) {
      try {
        const r = await api<{ events: EventEntry[] }>(`/admin/cleanup/runs/${runID}/log?tail=500`);
        setEvents((p) => ({ ...p, [runID]: r.events }));
      } catch (e) {
        toast.push("error", "couldn't load log");
      }
    }
  };

  if (busy && !data) return <p className="p-6 text-muted">Loading...</p>;
  if (err) return <p className="p-6 text-danger">{err}</p>;
  if (!data) return null;

  const anyDryRun = data.runs.some((r) => r.dryRun);

  return (
    <div className="space-y-6">
      <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3">
        <h1 className="text-2xl font-bold">Cleanup runs</h1>
        <div className="flex items-center gap-2 flex-wrap">
          <Link href="/dashboard/admin/cleanup/shards"
                className="text-xs text-accent hover:underline inline-flex items-center gap-1">
            <Network size={12} /> Shard tree
          </Link>
          <Button variant="ghost" onClick={() => void load()}>
            <RefreshCw size={14} className="mr-1" /> Refresh
          </Button>
          <Button onClick={runNow}>
            <Play size={14} className="mr-1" /> Run now
          </Button>
        </div>
      </div>

      {anyDryRun && (
        <div className="bg-amber-500/10 border border-amber-500/40 rounded p-3 flex items-start gap-2 text-xs text-amber-200">
          <AlertTriangle size={14} className="text-amber-400 shrink-0 mt-0.5" />
          <div>
            <p>
              Some recent runs were <strong>dry-run only</strong> — events were logged
              but nothing was deleted.
            </p>
            <p className="text-amber-300/80 mt-1">
              Flip <code className="bg-amber-950/40 px-1 rounded">CLEANUP_DRY_RUN=0</code> in
              <code className="bg-amber-950/40 px-1 rounded">.env</code> and restart the
              cleaner container once you've reviewed the audit log.
            </p>
          </div>
        </div>
      )}

      <Card>
        <h2 className="text-sm font-semibold mb-3">Last {data.window.runs} runs</h2>
        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3 text-xs">
          <Stat label="Runs"      value={data.window.runs.toString()} />
          <Stat label="Bytes freed" value={formatBytes(data.window.bytesFreed)} />
          <Stat label="Items reaped"
                value={Object.values(data.window.reapedCounts).reduce((a, b) => a + b, 0).toString()} />
          <Stat label="By type"
                value={Object.entries(data.window.reapedCounts).map(([k, v]) => `${k}:${v}`).join(" · ") || "-"}
                full />
        </div>
      </Card>

      <Card>
        <h2 className="text-sm font-semibold mb-3">Run history</h2>
        {data.runs.length === 0 ? (
          <p className="text-sm text-muted">No cleanup runs recorded yet.</p>
        ) : (
          <div className="space-y-1">
            {data.runs.map((run) => {
              const open = expanded.has(run.id);
              const evs = events[run.id] || [];
              const counts = Object.entries(run.reapedCounts);
              return (
                <div key={run.id} className="border border-border rounded">
                  <button
                    onClick={() => toggleRun(run.id)}
                    className="w-full flex items-center gap-2 p-2 hover:bg-panel text-left text-xs"
                  >
                    {open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
                    <span className="text-muted whitespace-nowrap">{formatDate(run.startedAt)}</span>
                    {run.dryRun && <span className="px-1.5 py-0.5 bg-amber-500/20 text-amber-300 rounded text-[10px]">DRY</span>}
                    {run.errors.length > 0 && <span className="px-1.5 py-0.5 bg-red-500/20 text-red-300 rounded text-[10px]">{run.errors.length} err</span>}
                    <span className="text-muted">{run.durationMs}ms</span>
                    <span className="text-muted">·</span>
                    <span>{formatBytes(run.bytesFreed)} freed</span>
                    {counts.length > 0 && (
                      <span className="text-muted truncate">
                        · {counts.map(([k, v]) => `${k}:${v}`).join(" · ")}
                      </span>
                    )}
                  </button>
                  {open && (
                    <div className="p-2 border-t border-border bg-bg text-xs space-y-1 max-h-96 overflow-y-auto">
                      {run.errors.length > 0 && (
                        <div className="text-danger mb-2">
                          {run.errors.map((e, i) => <div key={i} className="font-mono text-[10px]">{e}</div>)}
                        </div>
                      )}
                      {evs.length === 0 ? (
                        <p className="text-muted">No events logged for this run.</p>
                      ) : (
                        evs.map((e, i) => (
                          <div key={i} className="font-mono text-[10px] flex gap-2 items-baseline">
                            <span className="text-muted shrink-0">{new Date(e.ts).toLocaleTimeString()}</span>
                            <span className="px-1 rounded bg-border text-[9px] uppercase shrink-0">{e.action}</span>
                            <span className="truncate" title={e.targetRef}>{e.targetRef}</span>
                            {e.bytesFreed > 0 && (
                              <span className="text-success shrink-0">{formatBytes(e.bytesFreed)}</span>
                            )}
                            {e.dryRun && <span className="text-amber-300 shrink-0">[dry]</span>}
                          </div>
                        ))
                      )}
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        )}
      </Card>
    </div>
  );
}

function Stat({ label, value, full }: { label: string; value: string; full?: boolean }) {
  return (
    <div className={`p-2 bg-bg border border-border rounded ${full ? "col-span-2 sm:col-span-4" : ""}`}>
      <p className="text-[10px] uppercase text-muted">{label}</p>
      <p className="font-semibold truncate" title={value}>{value}</p>
    </div>
  );
}
