"use client";

// "Need more space?" widget — sits under the storage chart on Overview.
//
// Three states:
//   1. No pending request → small CTA card → opens an inline form
//   2. Pending → "Pending review since…" card with the asked amount
//   3. Just submitted → success card with a "submit another" affordance
//      after acknowledgement

import { useEffect, useState, FormEvent } from "react";
import { api, ApiError } from "@/lib/api";
import { Database, Clock, ArrowUpRight } from "lucide-react";
import { formatBytes } from "@/lib/format";

interface PendingResp {
  pending: null | {
    requestedBytes: number;
    reason: string;
    requestedAt: string;
  };
}

export function QuotaRequestWidget() {
  const [pending, setPending] = useState<PendingResp["pending"]>(null);
  const [loaded, setLoaded] = useState(false);
  const [open, setOpen] = useState(false);

  const refresh = () =>
    api<PendingResp>("/auth/me/quota-request")
      .then((r) => { setPending(r.pending); setLoaded(true); })
      .catch(() => setLoaded(true));

  useEffect(() => { void refresh(); }, []);

  if (!loaded) return null;

  if (pending) {
    return (
      <div className="bg-warning/5 border border-warning/30 rounded-lg p-4">
        <div className="flex items-start gap-3">
          <Clock size={16} className="text-warning shrink-0 mt-0.5" />
          <div className="flex-1 min-w-0">
            <p className="text-sm font-medium">Pending quota request</p>
            <p className="text-xs text-text-soft mt-1">
              You asked for <span className="font-mono">{formatBytes(pending.requestedBytes)}</span> more.
              {" "}Waiting for the administrator to decide.
            </p>
            {pending.reason && (
              <p className="text-xs text-text-soft mt-2 italic border-l-2 border-border pl-2">
                {pending.reason}
              </p>
            )}
          </div>
        </div>
      </div>
    );
  }

  if (!open) {
    return (
      <button onClick={() => setOpen(true)}
        className="w-full text-left bg-surface border border-border rounded-lg p-4 hover:border-text/30 transition-colors group">
        <div className="flex items-center gap-3">
          <Database size={16} className="text-muted shrink-0" />
          <div className="flex-1">
            <p className="text-sm font-medium">Need more space?</p>
            <p className="text-xs text-text-soft mt-0.5">
              Ask the administrator for a quota bump. Default accounts get 100 MB free.
            </p>
          </div>
          <ArrowUpRight size={14} className="text-muted group-hover:text-text" />
        </div>
      </button>
    );
  }

  return <RequestForm onSubmitted={() => { setOpen(false); void refresh(); }} onCancel={() => setOpen(false)} />;
}

function RequestForm({ onSubmitted, onCancel }: {
  onSubmitted: () => void;
  onCancel: () => void;
}) {
  const [amount, setAmount] = useState("500");
  const [unit, setUnit] = useState<"MB" | "GB">("MB");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true); setErr(null);
    const mult = unit === "GB" ? 1024 * 1024 * 1024 : 1024 * 1024;
    const bytes = Math.floor(parseFloat(amount) * mult);
    if (!bytes || bytes <= 0) { setErr("Pick a positive amount."); setBusy(false); return; }
    try {
      await api("/auth/me/quota-request", {
        method: "POST", body: JSON.stringify({ requestedBytes: bytes, reason }),
      });
      onSubmitted();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Couldn't submit");
    } finally { setBusy(false); }
  };

  return (
    <form onSubmit={submit} className="bg-surface border border-border rounded-lg p-4 space-y-3">
      <div>
        <h3 className="text-sm font-medium">Ask for more space</h3>
        <p className="text-xs text-text-soft mt-0.5">
          Your request is emailed to the administrator. You&apos;ll hear back the same way.
        </p>
      </div>
      <div className="flex gap-2">
        <input type="number" min="1" value={amount} onChange={(e) => setAmount(e.target.value)}
          className="flex-1 px-3 py-2 bg-bg border border-border rounded text-sm font-mono" required />
        <select value={unit} onChange={(e) => setUnit(e.target.value as "MB" | "GB")}
          className="px-2 py-2 bg-bg border border-border rounded text-sm">
          <option value="MB">MB</option>
          <option value="GB">GB</option>
        </select>
      </div>
      <textarea value={reason} onChange={(e) => setReason(e.target.value)} rows={2}
        placeholder="What's it for? (optional but helpful)"
        className="w-full px-3 py-2 bg-bg border border-border rounded text-sm" />
      {err && <p className="text-xs text-danger">{err}</p>}
      <div className="flex gap-2">
        <button type="submit" disabled={busy}
          className="px-3 py-1.5 rounded bg-text text-bg text-sm font-medium hover:opacity-90 disabled:opacity-40">
          {busy ? "Submitting…" : "Submit request"}
        </button>
        <button type="button" onClick={onCancel}
          className="px-3 py-1.5 text-sm text-text-soft hover:text-text">Cancel</button>
      </div>
    </form>
  );
}
