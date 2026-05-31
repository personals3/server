"use client";

import { useCallback, useEffect, useState } from "react";
import { api, ApiError } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import {
  RefreshCw, Link2, X, Clock, ArrowDown, ArrowUp,
  AlertTriangle, CheckCircle2, ArrowUpFromLine,
} from "lucide-react";
import { formatBytes, formatDate } from "@/lib/format";
import { useToast } from "@/components/toast";

interface Share {
  id: string;
  bucket: string;
  key: string;
  method: "GET" | "HEAD" | "PUT";
  expiresAt: string;
  forceDownload: boolean;
  revoked: boolean;
  revokedAt?: string;
  createdAt: string;
  lastUsedAt?: string;
  useCount: number;
  expired: boolean;
  active: boolean;
}

const EXTENSION_PRESETS = [
  { label: "+1 hour",   sec: 60 * 60 },
  { label: "+24 hours", sec: 60 * 60 * 24 },
  { label: "+7 days",   sec: 60 * 60 * 24 * 7 },
  { label: "+30 days",  sec: 60 * 60 * 24 * 30 },
];

export default function SharesPage() {
  const toast = useToast();
  const [shares, setShares] = useState<Share[]>([]);
  const [showInactive, setShowInactive] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    setBusy(true);
    setErr(null);
    try {
      const qs = showInactive ? "" : "?active=1";
      const r = await api<{ count: number; shares: Share[] }>(`/shares${qs}`);
      setShares(r.shares);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "failed to load");
    } finally {
      setBusy(false);
    }
  }, [showInactive]);

  useEffect(() => { void load(); }, [load]);

  const revoke = async (s: Share) => {
    if (!confirm(`Revoke this share link for ${s.bucket}/${s.key}?`)) return;
    try {
      await api(`/shares/${s.id}`, { method: "DELETE" });
      toast.push("success", "Share revoked.");
      void load();
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "failed");
    }
  };

  const setExpiry = async (s: Share, deltaSec: number) => {
    // Compute new expiry relative to current expiresAt (or now if expired).
    const base = Math.max(Date.now() / 1000, new Date(s.expiresAt).getTime() / 1000);
    const newExpiresAt = Math.floor(base + deltaSec);
    try {
      await api(`/shares/${s.id}`, {
        method: "PATCH",
        body: JSON.stringify({ expiresAt: newExpiresAt }),
      });
      toast.push("success",
        deltaSec > 0 ? "Extended." : "Expiry shortened.");
      void load();
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "failed");
    }
  };

  const revokeAll = async () => {
    const active = shares.filter((s) => s.active).length;
    if (active === 0) return;
    if (!confirm(`Revoke ALL ${active} active share link(s)? Cannot be undone.`)) return;
    try {
      const r = await api<{ revoked: number }>("/shares?all=1", { method: "DELETE" });
      toast.push("success", `Revoked ${r.revoked} share(s).`);
      void load();
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "failed");
    }
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between flex-wrap gap-2">
        <h1 className="text-2xl font-bold inline-flex items-center gap-2">
          <Link2 size={20} /> Share links
        </h1>
        <div className="flex items-center gap-2">
          <label className="text-xs text-muted inline-flex items-center gap-1">
            <input type="checkbox"
                   checked={showInactive}
                   onChange={(e) => setShowInactive(e.target.checked)} />
            Show expired/revoked
          </label>
          <Button variant="ghost" onClick={() => void load()}>
            <RefreshCw size={14} className="mr-1" /> Refresh
          </Button>
          {shares.some((s) => s.active) && (
            <Button onClick={revokeAll} className="bg-danger hover:bg-red-600">
              Revoke all
            </Button>
          )}
        </div>
      </div>

      {err && <p className="text-sm text-danger">{err}</p>}

      <Card>
        {busy && shares.length === 0 ? (
          <p className="text-sm text-muted">Loading...</p>
        ) : shares.length === 0 ? (
          <p className="text-sm text-muted">
            {showInactive ? "No share links yet." : "No active share links."}
          </p>
        ) : (
          <div className="space-y-2">
            {shares.map((s) => (
              <div key={s.id}
                   className={`border rounded p-3 ${
                     s.revoked  ? "border-red-500/40 bg-red-950/10" :
                     s.expired  ? "border-zinc-500/40 bg-zinc-950/10 opacity-70" :
                                  "border-border bg-bg"
                   }`}>
                <div className="flex items-start gap-3">
                  <div className="flex-1 min-w-0 space-y-1">
                    <div className="flex items-center gap-2 text-xs flex-wrap">
                      <MethodBadge m={s.method} />
                      {s.forceDownload && (
                        <span className="px-1 bg-zinc-500/30 text-zinc-300 rounded text-[9px]">DOWNLOAD</span>
                      )}
                      {s.revoked && (
                        <span className="px-1 bg-red-500/30 text-red-300 rounded text-[9px]">REVOKED</span>
                      )}
                      {!s.revoked && s.expired && (
                        <span className="px-1 bg-zinc-500/30 text-zinc-300 rounded text-[9px]">EXPIRED</span>
                      )}
                      {s.active && (
                        <span className="px-1 bg-green-500/30 text-green-300 rounded text-[9px] inline-flex items-center gap-0.5">
                          <CheckCircle2 size={10} /> ACTIVE
                        </span>
                      )}
                    </div>
                    <div className="font-mono text-xs truncate" title={`${s.bucket}/${s.key}`}>
                      {s.bucket}/{s.key}
                    </div>
                    <div className="text-[10px] text-muted">
                      created {formatDate(s.createdAt)}
                      {" · "}
                      expires {formatDate(s.expiresAt)}
                      {" · "}
                      hits {s.useCount}
                      {s.lastUsedAt && ` (last ${formatDate(s.lastUsedAt)})`}
                    </div>
                  </div>

                  {s.active && (
                    <div className="flex flex-col items-end gap-1 shrink-0">
                      <div className="flex items-center gap-1 text-[10px] text-muted">
                        <Clock size={10} /> change expiry:
                      </div>
                      <div className="flex flex-wrap gap-1 justify-end">
                        {EXTENSION_PRESETS.map((p) => (
                          <button
                            key={p.sec}
                            onClick={() => setExpiry(s, p.sec)}
                            className="text-[10px] px-1.5 py-0.5 rounded border border-border hover:border-accent"
                            title={`Extend by ${p.label.replace("+","")}`}
                          >
                            <ArrowUp size={9} className="inline" />{p.label.replace("+","")}
                          </button>
                        ))}
                        <button
                          onClick={() => setExpiry(s, -60 * 60)}
                          className="text-[10px] px-1.5 py-0.5 rounded border border-border hover:border-amber-500"
                          title="Shorten by 1 hour"
                        >
                          <ArrowDown size={9} className="inline" />1h
                        </button>
                      </div>
                      <button
                        onClick={() => revoke(s)}
                        className="text-xs text-danger hover:underline inline-flex items-center gap-1 mt-1"
                      >
                        <X size={12} /> Revoke
                      </button>
                    </div>
                  )}
                </div>
              </div>
            ))}
          </div>
        )}
      </Card>

      <div className="text-xs text-muted">
        <p className="inline-flex items-center gap-1">
          <AlertTriangle size={12} className="text-amber-400" />
          A revoked share link returns 403 immediately, even if its signature
          would otherwise still be valid.
        </p>
      </div>
    </div>
  );
}

function MethodBadge({ m }: { m: "GET" | "HEAD" | "PUT" }) {
  const cls = m === "PUT"
    ? "bg-amber-500/30 text-amber-300"
    : "bg-blue-500/30 text-blue-300";
  return (
    <span className={`px-1 rounded text-[9px] inline-flex items-center gap-0.5 ${cls}`}>
      {m === "PUT" ? <ArrowUpFromLine size={10} /> : null}{m}
    </span>
  );
}
