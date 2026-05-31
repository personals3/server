"use client";

import { useEffect, useState } from "react";
import { api, ApiError } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { RefreshCw, Folder, FolderOpen, AlertCircle } from "lucide-react";

interface ShardNode {
  shardPath: string;
  depth: number;
  isLeaf: boolean;
  objectCount: number;
  dirty: boolean;
  lastWalkAt?: string;
  updatedAt: string;
}

interface TreeResp {
  bucket: string;
  bucketId: string;
  objectCount: number;
  summary: { nodes: number; leaves: number; dirtyLeaves: number };
  tree: ShardNode[];
}

export default function ShardTreePage() {
  const [bucket, setBucket] = useState("");
  const [data, setData] = useState<TreeResp | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = async (name: string) => {
    if (!name) return;
    setBusy(true);
    setErr(null);
    try {
      const r = await api<TreeResp>(`/admin/buckets/${encodeURIComponent(name)}/shards`);
      setData(r);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "failed");
      setData(null);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">Shard tree</h1>
      </div>

      <Card>
        <h2 className="text-sm font-semibold mb-3">Bucket</h2>
        <form
          onSubmit={(e) => { e.preventDefault(); void load(bucket); }}
          className="flex items-center gap-2"
        >
          <input
            type="text"
            value={bucket}
            onChange={(e) => setBucket(e.target.value)}
            placeholder="bucket-name"
            className="flex-1 bg-bg border border-border rounded px-3 py-1.5 text-sm font-mono"
          />
          <Button type="submit" disabled={busy || !bucket}>Load</Button>
          {data && (
            <Button variant="ghost" onClick={() => void load(bucket)}>
              <RefreshCw size={14} />
            </Button>
          )}
        </form>
      </Card>

      {err && (
        <Card>
          <div className="flex items-start gap-2 text-danger text-sm">
            <AlertCircle size={16} /> {err}
          </div>
        </Card>
      )}

      {data && (
        <>
          <Card>
            <div className="grid grid-cols-2 sm:grid-cols-4 gap-3 text-xs">
              <Stat label="Objects"      value={data.objectCount.toLocaleString()} />
              <Stat label="Shard nodes"  value={data.summary.nodes.toLocaleString()} />
              <Stat label="Leaves"       value={data.summary.leaves.toLocaleString()} />
              <Stat label="Dirty leaves"
                    value={data.summary.dirtyLeaves.toString()}
                    accent={data.summary.dirtyLeaves > 0 ? "warn" : undefined} />
            </div>
          </Card>

          <Card>
            <h2 className="text-sm font-semibold mb-3">Tree</h2>
            {data.tree.length === 0 ? (
              <p className="text-sm text-muted">Bucket has no shard index yet — cleaner will create one on next backfill.</p>
            ) : (
              <div className="font-mono text-xs space-y-0.5">
                {data.tree.map((n) => (
                  <div key={n.shardPath}
                       className={`flex items-center gap-2 py-0.5 ${
                         n.dirty ? "text-amber-300" : "text-text"
                       }`}>
                    <span className="text-muted shrink-0" style={{ paddingLeft: n.depth * 12 }}>
                      {n.isLeaf ? <Folder size={11} /> : <FolderOpen size={11} />}
                    </span>
                    <span className="text-muted">{n.shardPath || "(root)"}</span>
                    {n.isLeaf && (
                      <>
                        <span className="text-muted">·</span>
                        <span>{n.objectCount.toLocaleString()} obj</span>
                      </>
                    )}
                    {n.dirty && (
                      <span className="px-1 py-0 bg-amber-500/20 text-amber-300 rounded text-[9px]">DIRTY</span>
                    )}
                    {n.objectCount > 5000 && n.isLeaf && (
                      <span className="px-1 py-0 bg-blue-500/20 text-blue-300 rounded text-[9px]">SPLIT-QUEUED</span>
                    )}
                  </div>
                ))}
              </div>
            )}
          </Card>
        </>
      )}
    </div>
  );
}

function Stat({ label, value, accent }: { label: string; value: string; accent?: "warn" }) {
  const ring = accent === "warn"
    ? "border-amber-500/50 text-amber-200"
    : "border-border";
  return (
    <div className={`p-2 bg-bg border rounded ${ring}`}>
      <p className="text-[10px] uppercase text-muted">{label}</p>
      <p className="font-semibold">{value}</p>
    </div>
  );
}
