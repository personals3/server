"use client";

// Admin onboarding queue.
//
// Two tabs: Accounts (people asking to sign up) and Quota (existing users
// asking for more space). Approve / Deny inline; each opens a small
// inline form (no modal) so the row turns into an editor without taking
// over the screen.

import { useEffect, useState } from "react";
import { api, ApiError } from "@/lib/api";
import { Inbox, Check, X, Mail, Database, Clock } from "lucide-react";
import { formatBytes, formatDate } from "@/lib/format";

interface AccountReq {
  id: string;
  email: string;
  name: string;
  reason: string;
  status: "pending" | "approved" | "denied";
  grantedQuotaBytes?: number;
  adminNote: string;
  requestedAt: string;
  decidedAt?: string;
}
interface QuotaReq {
  id: string;
  userId: string;
  userEmail: string;
  userName: string;
  currentBytes: number;
  usedBytes: number;
  requestedBytes: number;
  reason: string;
  status: "pending" | "approved" | "denied";
  grantedBytes?: number;
  adminNote: string;
  requestedAt: string;
  decidedAt?: string;
}

type Tab = "accounts" | "quotas";
type Filter = "pending" | "all";

export default function RequestsPage() {
  const [tab, setTab] = useState<Tab>("accounts");
  const [filter, setFilter] = useState<Filter>("pending");
  const [accounts, setAccounts] = useState<AccountReq[]>([]);
  const [quotas, setQuotas] = useState<QuotaReq[]>([]);
  const [loading, setLoading] = useState(true);

  const load = () => {
    setLoading(true);
    void api<{ accounts: AccountReq[]; quotas: QuotaReq[] }>(`/admin/requests?status=${filter}`)
      .then((r) => { setAccounts(r.accounts || []); setQuotas(r.quotas || []); })
      .finally(() => setLoading(false));
  };

  useEffect(() => { load(); }, [filter]);

  const pendingAccounts = accounts.filter((a) => a.status === "pending").length;
  const pendingQuotas   = quotas.filter((q) => q.status === "pending").length;

  return (
    <div className="space-y-6 max-w-5xl">
      <div className="border-b border-border pb-4">
        <div className="flex items-center gap-2 mb-1">
          <Inbox size={18} className="text-text-soft" />
          <h1 className="text-xl font-semibold tracking-tight">Requests</h1>
        </div>
        <p className="text-sm text-text-soft">
          Approve or deny new accounts and quota bumps. Decisions email the requester.
        </p>
      </div>

      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex gap-1">
          <Tab id="accounts" cur={tab} onClick={setTab} count={pendingAccounts}
            icon={<Mail size={13} />}>Account requests</Tab>
          <Tab id="quotas" cur={tab} onClick={setTab} count={pendingQuotas}
            icon={<Database size={13} />}>Quota requests</Tab>
        </div>
        <div className="text-xs">
          <button onClick={() => setFilter("pending")}
            className={"px-2.5 py-1 rounded-l-md border " + (filter === "pending"
              ? "bg-text text-bg border-text"
              : "border-border text-text-soft hover:border-text/40")}>
            Pending
          </button>
          <button onClick={() => setFilter("all")}
            className={"px-2.5 py-1 rounded-r-md border-y border-r " + (filter === "all"
              ? "bg-text text-bg border-text"
              : "border-border text-text-soft hover:border-text/40")}>
            All
          </button>
        </div>
      </div>

      {loading && <p className="text-sm text-muted py-10 text-center">Loading…</p>}

      {!loading && tab === "accounts" && (
        accounts.length === 0
          ? <Empty label={filter === "pending" ? "No pending account requests." : "No account requests."} />
          : <div className="space-y-3">
              {accounts.map((a) => <AccountRow key={a.id} req={a} onChange={load} />)}
            </div>
      )}

      {!loading && tab === "quotas" && (
        quotas.length === 0
          ? <Empty label={filter === "pending" ? "No pending quota requests." : "No quota requests."} />
          : <div className="space-y-3">
              {quotas.map((q) => <QuotaRow key={q.id} req={q} onChange={load} />)}
            </div>
      )}
    </div>
  );
}

// ----------------------------------------------------------------------------

function Tab({ id, cur, onClick, count, icon, children }: {
  id: Tab; cur: Tab; onClick: (t: Tab) => void;
  count: number; icon: React.ReactNode; children: React.ReactNode;
}) {
  const active = cur === id;
  return (
    <button onClick={() => onClick(id)}
      className={"px-3 py-2 text-sm font-medium rounded-md inline-flex items-center gap-2 transition-colors " +
        (active ? "bg-surface text-text" : "text-text-soft hover:text-text")}>
      {icon}{children}
      {count > 0 && (
        <span className={"text-[10px] font-medium px-1.5 py-0.5 rounded-full " +
          (active ? "bg-text text-bg" : "bg-codeBg text-muted border border-border")}>
          {count}
        </span>
      )}
    </button>
  );
}

function Empty({ label }: { label: string }) {
  return (
    <div className="border border-dashed border-border rounded-lg py-16 text-center">
      <p className="text-sm text-muted">{label}</p>
    </div>
  );
}

function StatusBadge({ s }: { s: "pending" | "approved" | "denied" }) {
  const m = {
    pending:  { color: "text-warning border-warning/30 bg-warning/5", label: "Pending" },
    approved: { color: "text-success border-success/30 bg-success/5", label: "Approved" },
    denied:   { color: "text-danger border-danger/30 bg-danger/5",   label: "Denied" },
  }[s];
  return <span className={`text-[10px] uppercase tracking-wider px-1.5 py-0.5 rounded border font-medium ${m.color}`}>{m.label}</span>;
}

// ----------------------------------------------------------------------------

function AccountRow({ req, onChange }: { req: AccountReq; onChange: () => void }) {
  const [showApprove, setShowApprove] = useState(false);
  const [showDeny, setShowDeny] = useState(false);
  const [quotaMB, setQuotaMB] = useState("100");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const approve = async () => {
    setBusy(true); setErr(null);
    try {
      await api(`/admin/requests/account/${req.id}/approve`, {
        method: "POST",
        body: JSON.stringify({ quotaBytes: Math.floor(parseFloat(quotaMB) * 1024 * 1024), note }),
      });
      onChange();
    } catch (e) { setErr(e instanceof ApiError ? e.message : "Failed"); }
    finally { setBusy(false); }
  };
  const deny = async () => {
    setBusy(true); setErr(null);
    try {
      await api(`/admin/requests/account/${req.id}/deny`, {
        method: "POST", body: JSON.stringify({ note }),
      });
      onChange();
    } catch (e) { setErr(e instanceof ApiError ? e.message : "Failed"); }
    finally { setBusy(false); }
  };

  return (
    <div className="border border-border rounded-lg bg-panel p-4 sm:p-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <h3 className="text-base font-semibold truncate">{req.name}</h3>
            <StatusBadge s={req.status} />
          </div>
          <p className="text-sm text-text-soft font-mono mt-0.5">{req.email}</p>
        </div>
        <p className="text-xs text-muted inline-flex items-center gap-1 shrink-0">
          <Clock size={11} /> {formatDate(req.requestedAt)}
        </p>
      </div>

      {req.reason && (
        <p className="mt-3 text-sm text-text-soft border-l-2 border-border pl-3 italic">
          {req.reason}
        </p>
      )}

      {req.status === "pending" ? (
        <>
          {!showApprove && !showDeny && (
            <div className="flex gap-2 mt-4">
              <button onClick={() => { setShowApprove(true); setShowDeny(false); }}
                className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md bg-text text-bg text-sm font-medium hover:opacity-90">
                <Check size={14} /> Approve
              </button>
              <button onClick={() => { setShowDeny(true); setShowApprove(false); }}
                className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md border border-border text-text-soft hover:text-danger hover:border-danger/40 text-sm font-medium">
                <X size={14} /> Deny
              </button>
            </div>
          )}

          {showApprove && (
            <div className="mt-4 p-4 bg-bg border border-border rounded-md space-y-3">
              <label className="block">
                <span className="text-xs font-medium">Starting quota (MB)</span>
                <input value={quotaMB} onChange={(e) => setQuotaMB(e.target.value)}
                  className="mt-1 w-32 px-3 py-1.5 bg-panel border border-border rounded text-sm" />
              </label>
              <label className="block">
                <span className="text-xs font-medium">Note (optional, included in email)</span>
                <textarea value={note} onChange={(e) => setNote(e.target.value)} rows={2}
                  className="mt-1 w-full px-3 py-1.5 bg-panel border border-border rounded text-sm" />
              </label>
              <div className="flex gap-2">
                <button onClick={approve} disabled={busy}
                  className="px-3 py-1.5 rounded-md bg-text text-bg text-sm font-medium hover:opacity-90 disabled:opacity-40">
                  {busy ? "Approving…" : "Confirm approval"}
                </button>
                <button onClick={() => { setShowApprove(false); setErr(null); }}
                  className="px-3 py-1.5 rounded-md text-sm text-text-soft hover:text-text">Cancel</button>
              </div>
              {err && <p className="text-xs text-danger">{err}</p>}
            </div>
          )}

          {showDeny && (
            <div className="mt-4 p-4 bg-bg border border-border rounded-md space-y-3">
              <label className="block">
                <span className="text-xs font-medium">Reason (optional, included in email)</span>
                <textarea value={note} onChange={(e) => setNote(e.target.value)} rows={2}
                  className="mt-1 w-full px-3 py-1.5 bg-panel border border-border rounded text-sm" />
              </label>
              <div className="flex gap-2">
                <button onClick={deny} disabled={busy}
                  className="px-3 py-1.5 rounded-md border border-danger/40 text-danger text-sm font-medium hover:bg-danger/5 disabled:opacity-40">
                  {busy ? "Denying…" : "Confirm denial"}
                </button>
                <button onClick={() => { setShowDeny(false); setErr(null); }}
                  className="px-3 py-1.5 rounded-md text-sm text-text-soft hover:text-text">Cancel</button>
              </div>
              {err && <p className="text-xs text-danger">{err}</p>}
            </div>
          )}
        </>
      ) : (
        <p className="mt-3 text-xs text-muted">
          {req.status === "approved" ? "Approved" : "Denied"} {req.decidedAt && <>· {formatDate(req.decidedAt)}</>}
          {req.adminNote && <> · &ldquo;{req.adminNote}&rdquo;</>}
        </p>
      )}
    </div>
  );
}

function QuotaRow({ req, onChange }: { req: QuotaReq; onChange: () => void }) {
  const [showApprove, setShowApprove] = useState(false);
  const [showDeny, setShowDeny] = useState(false);
  const [grantedMB, setGrantedMB] = useState((req.requestedBytes / (1024 * 1024)).toString());
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const approve = async () => {
    setBusy(true); setErr(null);
    try {
      await api(`/admin/requests/quota/${req.id}/approve`, {
        method: "POST",
        body: JSON.stringify({ grantedBytes: Math.floor(parseFloat(grantedMB) * 1024 * 1024), note }),
      });
      onChange();
    } catch (e) { setErr(e instanceof ApiError ? e.message : "Failed"); }
    finally { setBusy(false); }
  };
  const deny = async () => {
    setBusy(true); setErr(null);
    try {
      await api(`/admin/requests/quota/${req.id}/deny`, {
        method: "POST", body: JSON.stringify({ note }),
      });
      onChange();
    } catch (e) { setErr(e instanceof ApiError ? e.message : "Failed"); }
    finally { setBusy(false); }
  };

  return (
    <div className="border border-border rounded-lg bg-panel p-4 sm:p-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <h3 className="text-base font-semibold truncate">{req.userName}</h3>
            <StatusBadge s={req.status} />
          </div>
          <p className="text-sm text-text-soft font-mono mt-0.5">{req.userEmail}</p>
        </div>
        <p className="text-xs text-muted inline-flex items-center gap-1 shrink-0">
          <Clock size={11} /> {formatDate(req.requestedAt)}
        </p>
      </div>

      <div className="mt-3 grid grid-cols-2 sm:grid-cols-3 gap-3 text-xs">
        <Stat label="Current quota" value={formatBytes(req.currentBytes)} />
        <Stat label="Used"          value={formatBytes(req.usedBytes)} />
        <Stat label="Asking for"    value={"+ " + formatBytes(req.requestedBytes)} highlight />
      </div>

      {req.reason && (
        <p className="mt-3 text-sm text-text-soft border-l-2 border-border pl-3 italic">
          {req.reason}
        </p>
      )}

      {req.status === "pending" ? (
        <>
          {!showApprove && !showDeny && (
            <div className="flex gap-2 mt-4">
              <button onClick={() => { setShowApprove(true); setShowDeny(false); }}
                className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md bg-text text-bg text-sm font-medium hover:opacity-90">
                <Check size={14} /> Approve
              </button>
              <button onClick={() => { setShowDeny(true); setShowApprove(false); }}
                className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md border border-border text-text-soft hover:text-danger hover:border-danger/40 text-sm font-medium">
                <X size={14} /> Deny
              </button>
            </div>
          )}

          {showApprove && (
            <div className="mt-4 p-4 bg-bg border border-border rounded-md space-y-3">
              <label className="block">
                <span className="text-xs font-medium">Grant (MB) — default = requested amount</span>
                <input value={grantedMB} onChange={(e) => setGrantedMB(e.target.value)}
                  className="mt-1 w-32 px-3 py-1.5 bg-panel border border-border rounded text-sm" />
              </label>
              <label className="block">
                <span className="text-xs font-medium">Note (optional)</span>
                <textarea value={note} onChange={(e) => setNote(e.target.value)} rows={2}
                  className="mt-1 w-full px-3 py-1.5 bg-panel border border-border rounded text-sm" />
              </label>
              <div className="flex gap-2">
                <button onClick={approve} disabled={busy}
                  className="px-3 py-1.5 rounded-md bg-text text-bg text-sm font-medium hover:opacity-90 disabled:opacity-40">
                  {busy ? "Approving…" : "Confirm approval"}
                </button>
                <button onClick={() => { setShowApprove(false); setErr(null); }}
                  className="px-3 py-1.5 rounded-md text-sm text-text-soft hover:text-text">Cancel</button>
              </div>
              {err && <p className="text-xs text-danger">{err}</p>}
            </div>
          )}

          {showDeny && (
            <div className="mt-4 p-4 bg-bg border border-border rounded-md space-y-3">
              <label className="block">
                <span className="text-xs font-medium">Reason (optional)</span>
                <textarea value={note} onChange={(e) => setNote(e.target.value)} rows={2}
                  className="mt-1 w-full px-3 py-1.5 bg-panel border border-border rounded text-sm" />
              </label>
              <div className="flex gap-2">
                <button onClick={deny} disabled={busy}
                  className="px-3 py-1.5 rounded-md border border-danger/40 text-danger text-sm font-medium hover:bg-danger/5 disabled:opacity-40">
                  {busy ? "Denying…" : "Confirm denial"}
                </button>
                <button onClick={() => { setShowDeny(false); setErr(null); }}
                  className="px-3 py-1.5 rounded-md text-sm text-text-soft hover:text-text">Cancel</button>
              </div>
              {err && <p className="text-xs text-danger">{err}</p>}
            </div>
          )}
        </>
      ) : (
        <p className="mt-3 text-xs text-muted">
          {req.status === "approved"
            ? `Approved · granted ${req.grantedBytes ? formatBytes(req.grantedBytes) : "—"}`
            : "Denied"}
          {req.decidedAt && <> · {formatDate(req.decidedAt)}</>}
          {req.adminNote && <> · &ldquo;{req.adminNote}&rdquo;</>}
        </p>
      )}
    </div>
  );
}

function Stat({ label, value, highlight }: { label: string; value: string; highlight?: boolean }) {
  return (
    <div className="bg-bg border border-border rounded px-3 py-2">
      <div className="text-[10px] uppercase tracking-wider text-muted">{label}</div>
      <div className={"font-mono text-sm mt-0.5 " + (highlight ? "text-link font-semibold" : "")}>{value}</div>
    </div>
  );
}
