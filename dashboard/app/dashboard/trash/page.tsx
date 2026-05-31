"use client";

import { useCallback, useEffect, useState } from "react";
import { api, ApiError } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Trash2, RotateCcw, RefreshCw, AlertTriangle, History as HistoryIcon } from "lucide-react";
import { formatBytes, formatDate } from "@/lib/format";
import { useToast } from "@/components/toast";

interface TrashItem {
  bucket: string;
  key: string;
  size: number;
  contentType: string;
  deletedAt: string;
  fromVersionedBucket: boolean;
}

export default function TrashPage() {
  const toast = useToast();
  const [items, setItems] = useState<TrashItem[]>([]);
  const [totalBytes, setTotalBytes] = useState(0);
  const [picks, setPicks] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const itemKey = (it: TrashItem) => `${it.bucket}\x00${it.key}`;

  const load = useCallback(async () => {
    setBusy(true);
    setErr(null);
    try {
      const r = await api<{ items: TrashItem[]; totalBytes: number }>("/trash");
      setItems(r.items);
      setTotalBytes(r.totalBytes);
      // Drop any picks that are no longer in the list
      setPicks((prev) => {
        const present = new Set(r.items.map(itemKey));
        const next = new Set<string>();
        prev.forEach((k) => { if (present.has(k)) next.add(k); });
        return next;
      });
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "failed to load trash");
    } finally {
      setBusy(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  const toggle = (k: string) => {
    setPicks((prev) => {
      const next = new Set(prev);
      if (next.has(k)) next.delete(k); else next.add(k);
      return next;
    });
  };
  const toggleAll = (checked: boolean) => {
    setPicks(checked ? new Set(items.map(itemKey)) : new Set());
  };

  const selectedItems = items.filter((it) => picks.has(itemKey(it)));

  const restoreItems = async (targets: TrashItem[]) => {
    if (targets.length === 0) return;
    try {
      const r = await api<{ restored: number; errors: any[] }>("/trash", {
        method: "POST",
        body: JSON.stringify({
          items: targets.map((it) => ({ bucket: it.bucket, key: it.key })),
        }),
      });
      toast.push("success", `Restored ${r.restored} item(s).`);
      setPicks(new Set());
      void load();
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "restore failed");
    }
  };

  const purgeItems = async (targets: TrashItem[]) => {
    if (targets.length === 0) return;
    if (!confirm(
      `Permanently delete ${targets.length} item(s)?\n\n` +
      `The bytes (including all prior versions) will be removed from disk and quota refunded.\n` +
      `This can't be undone.`,
    )) return;
    try {
      const r = await api<{ purged: number; refundedBytes: number }>("/trash", {
        method: "DELETE",
        body: JSON.stringify({
          items: targets.map((it) => ({ bucket: it.bucket, key: it.key })),
        }),
      });
      toast.push("success",
        `Purged ${r.purged} item(s). Reclaimed ${formatBytes(r.refundedBytes)}.`);
      setPicks(new Set());
      void load();
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "purge failed");
    }
  };

  const restoreSelected = () => restoreItems(selectedItems);
  const purgeSelected   = () => purgeItems(selectedItems);

  const emptyAll = async () => {
    if (items.length === 0) return;
    if (!confirm(
      `Empty the trash entirely?\n\n` +
      `This will permanently delete all ${items.length} item(s) — ${formatBytes(totalBytes)} ` +
      `including every prior version. Cannot be undone.`,
    )) return;
    try {
      const r = await api<{ purged: number; refundedBytes: number }>(
        "/trash?all=1", { method: "DELETE" });
      toast.push("success",
        `Trash emptied — ${r.purged} item(s) purged, ${formatBytes(r.refundedBytes)} reclaimed.`);
      void load();
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "empty failed");
    }
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">Trash</h1>
        <Button variant="ghost" onClick={() => void load()}>
          <RefreshCw size={14} className="mr-1" /> Refresh
        </Button>
      </div>

      <div className="bg-amber-500/10 border border-amber-500/40 rounded p-3 flex gap-3">
        <AlertTriangle size={16} className="text-amber-400 shrink-0 mt-0.5" />
        <div className="text-xs text-amber-200/90 space-y-1">
          <p>
            Deleted objects sit here until you restore them or empty the trash.
            <span className="text-amber-300/80"> Their bytes still count toward your quota.</span>
          </p>
          <p className="text-amber-300/70">
            Tip: hold <kbd className="px-1 py-0.5 bg-amber-950/40 rounded text-[10px]">Shift</kbd>
            {" "}while clicking <em>Delete</em> in the bucket view to skip the trash entirely
            (uses <code>?purge</code>). UI hook for that lands in a follow-up.
          </p>
        </div>
      </div>

      <Card>
        <div className="flex flex-wrap items-center justify-between gap-2 mb-3">
          <h2 className="text-sm font-semibold">
            {items.length} item{items.length === 1 ? "" : "s"}
            <span className="text-muted font-normal"> · {formatBytes(totalBytes)} on disk</span>
          </h2>
          <div className="flex flex-wrap items-center gap-2">
            {picks.size > 0 && (
              <>
                <button onClick={restoreSelected}
                        className="text-xs text-blue-300 hover:underline inline-flex items-center gap-1">
                  <RotateCcw size={12} /> Restore {picks.size}
                </button>
                <button onClick={purgeSelected}
                        className="text-xs text-danger hover:underline inline-flex items-center gap-1">
                  <Trash2 size={12} /> Purge {picks.size}
                </button>
              </>
            )}
            {items.length > 0 && (
              <button onClick={emptyAll}
                      className="text-xs text-danger hover:underline inline-flex items-center gap-1"
                      title="Permanently delete everything in trash">
                Empty trash
              </button>
            )}
          </div>
        </div>

        {busy && items.length === 0 ? (
          <p className="text-sm text-muted">Loading...</p>
        ) : err ? (
          <p className="text-sm text-danger">{err}</p>
        ) : items.length === 0 ? (
          <p className="text-sm text-muted">Trash is empty.</p>
        ) : (
          <div className="overflow-x-auto -mx-4 sm:mx-0">
            <table className="stack-rows w-full text-sm min-w-[600px] sm:min-w-0">
              <thead className="text-xs text-muted uppercase">
                <tr>
                  <th className="text-left pb-2 w-8">
                    <input type="checkbox"
                           checked={picks.size > 0 && picks.size === items.length}
                           onChange={(e) => toggleAll(e.target.checked)} />
                  </th>
                  <th className="text-left pb-2">Bucket / Key</th>
                  <th className="text-right pb-2">Size</th>
                  <th className="text-right pb-2">Deleted</th>
                  <th className="text-right pb-2 w-32">Actions</th>
                </tr>
              </thead>
              <tbody>
                {items.map((it) => {
                  const k = itemKey(it);
                  return (
                    <tr key={k} className="border-t border-border">
                      <td data-label="" className="actions py-2">
                        <input type="checkbox" checked={picks.has(k)}
                               onChange={() => toggle(k)} />
                      </td>
                      <td data-label="Bucket / Key" className="py-2">
                        <div className="flex items-center gap-2 min-w-0">
                          <span className="text-muted text-xs">{it.bucket}/</span>
                          <span className="font-mono truncate" title={it.key}>{it.key}</span>
                          {it.fromVersionedBucket && (
                            <HistoryIcon size={11} className="text-blue-400 shrink-0"
                                         aria-label="versioned bucket — restores the latest non-marker version" />
                          )}
                        </div>
                      </td>
                      <td data-label="Size" className="py-2 text-right text-muted">{formatBytes(it.size)}</td>
                      <td data-label="Deleted" className="py-2 text-right text-muted">{formatDate(it.deletedAt)}</td>
                      <td data-label="" className="actions py-2 text-right">
                        <button
                          onClick={() => restoreItems([it])}
                          className="text-blue-300 hover:text-blue-200 mr-2"
                          title="Restore this object">
                          <RotateCcw size={14} />
                        </button>
                        <button
                          onClick={() => purgeItems([it])}
                          className="text-danger hover:text-red-400"
                          title="Permanently delete this object">
                          <Trash2 size={14} />
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  );
}
