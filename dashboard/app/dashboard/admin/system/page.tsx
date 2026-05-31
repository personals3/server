"use client";

import { useEffect, useState } from "react";
import { api, ApiError } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { QuotaBar } from "@/components/quota-bar";
import { formatBytes } from "@/lib/format";

interface DiskInfo {
  path: string;
  physicalTotal: number;
  physicalUsed: number;
  physicalFree: number;
  reservedBytes: number;
  effectiveTotal: number;
  allocatedToUsers: number;
  actuallyUsed: number;
  overcommitAllowed: boolean;
  diskFullPct: number;
  overcommitted: boolean;
}

interface Stats {
  userCount: number;
  bucketCount: number;
  objectCount: number;
  totalUsedBytes: number;
  totalQuotaBytes: number;
  transcodeJobs: { pending: number; processing: number; done: number; failed: number };
  requestsLastHour: number;
  topActions: { action: string; count: number }[];
  disk?: DiskInfo;
}

interface ConfigEntry {
  key: string;
  value: string;
  description: string;
}

export default function SystemPage() {
  const [stats, setStats] = useState<Stats | null>(null);
  const [config, setConfig] = useState<ConfigEntry[]>([]);

  const load = () => {
    void api<Stats>("/admin/stats").then(setStats);
    void api<{ config: ConfigEntry[] }>("/admin/system-config").then((r) => setConfig(r.config));
  };

  useEffect(() => {
    load();
    const id = setInterval(load, 5000);
    return () => clearInterval(id);
  }, []);

  if (!stats) return <p className="text-muted">Loading...</p>;

  return (
    <div className="space-y-6">
      <h1 className="text-2xl font-bold">System</h1>

      <div className="grid grid-cols-2 md:grid-cols-4 gap-3">
        <Stat label="Active Users"  value={stats.userCount} />
        <Stat label="Buckets"       value={stats.bucketCount} />
        <Stat label="Objects"       value={stats.objectCount.toLocaleString()} />
        <Stat label="Req / last hr" value={stats.requestsLastHour.toLocaleString()} />
      </div>
      {/* The 2×2 layout above already handles mobile; keep it. The
          transcode-queue grid below was a fixed 4-col which crammed on
          mobile — turned into 2×2 on narrow viewports. */}

      {stats.disk && <StorageSection disk={stats.disk} />}

      <CapacitySection stats={stats} config={config} />

      <ConfigSection
        title="Capacity"
        entries={config.filter((e) => isCapacityKey(e.key))}
        onChange={load}
        helper="Caps the number of users and the share of disk that can be allocated. Changes apply on the next signup, approval, or login."
      />
      <ConfigSection
        title="Storage configuration"
        entries={config.filter((e) => isStorageKey(e.key))}
        onChange={load}
      />
      <ConfigSection
        title="Cleaner configuration"
        entries={config.filter((e) => isCleanerKey(e.key))}
        onChange={load}
        helper="Changes take effect on the next cleaner tick (~30s). No restart needed."
      />

      <Card>
        <h2 className="text-sm font-semibold mb-3">Transcode queue</h2>
        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <JobStat label="Pending"    count={stats.transcodeJobs.pending}    color="text-warning" />
          <JobStat label="Processing" count={stats.transcodeJobs.processing} color="text-accent" />
          <JobStat label="Done"       count={stats.transcodeJobs.done}       color="text-success" />
          <JobStat label="Failed"     count={stats.transcodeJobs.failed}     color="text-danger" />
        </div>
      </Card>

      <Card>
        <h2 className="text-sm font-semibold mb-3">Top actions (last hour)</h2>
        {stats.topActions.length === 0 ? (
          <p className="text-sm text-muted">No traffic yet.</p>
        ) : (
          <table className="w-full text-sm">
            <tbody>
              {stats.topActions.map((a) => (
                <tr key={a.action} className="border-t border-border">
                  <td className="py-1.5 font-mono break-all">{a.action}</td>
                  <td className="py-1.5 text-right text-muted whitespace-nowrap pl-3">{a.count}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string | number }) {
  return (
    <Card>
      <p className="text-xs text-muted">{label}</p>
      <p className="text-2xl font-bold mt-1">{value}</p>
    </Card>
  );
}

function JobStat({ label, count, color }: { label: string; count: number; color: string }) {
  return (
    <div className="bg-bg border border-border rounded p-3">
      <p className="text-xs text-muted">{label}</p>
      <p className={`text-2xl font-bold mt-1 ${color}`}>{count}</p>
    </div>
  );
}

// -------- Storage breakdown --------------------------------------------------

function StorageSection({ disk }: { disk: DiskInfo }) {
  const fullPct = disk.physicalTotal > 0
    ? (Number(disk.physicalUsed) / Number(disk.physicalTotal)) * 100
    : 0;

  return (
    <Card>
      <div className="flex items-center justify-between mb-3">
        <h2 className="text-sm font-semibold">Storage</h2>
        <code className="text-xs text-muted">{disk.path}</code>
      </div>

      <div className="space-y-4">
        <div>
          <p className="text-xs text-muted mb-1">Physical disk (what the OS sees)</p>
          <QuotaBar used={Number(disk.physicalUsed)} total={Number(disk.physicalTotal)} />
          <p className="text-xs text-muted mt-1">
            {formatBytes(Number(disk.physicalFree))} free
            {" · "}upload-reject threshold: {disk.diskFullPct}%
            {fullPct >= disk.diskFullPct && (
              <span className="ml-2 text-danger font-semibold">DISK FULL — uploads disabled</span>
            )}
          </p>
        </div>

        <div>
          <p className="text-xs text-muted mb-1">
            User allocations (sum of per-user quotas)
            {disk.overcommitted && (
              <span className="ml-2 text-warning">overcommitted</span>
            )}
          </p>
          <QuotaBar used={Number(disk.allocatedToUsers)} total={Number(disk.effectiveTotal)} />
          <p className="text-xs text-muted mt-1">
            {formatBytes(Number(disk.effectiveTotal))} effective cap
            {disk.reservedBytes > 0 && (
              <> {" · "}{formatBytes(Number(disk.reservedBytes))} reserved for OS</>
            )}
          </p>
        </div>

        <div>
          <p className="text-xs text-muted mb-1">Actual usage (sum of users.used_bytes)</p>
          <QuotaBar used={Number(disk.actuallyUsed)} total={Number(disk.allocatedToUsers) || 1} />
        </div>
      </div>

      <details className="mt-4">
        <summary className="text-xs text-muted cursor-pointer hover:text-text">
          How to add more storage
        </summary>
        <div className="mt-2 text-xs text-muted space-y-1 pl-4">
          <p>The system auto-detects whatever filesystem is mounted at <code>{disk.path}</code>.</p>
          <p>To expand:</p>
          <ol className="list-decimal pl-4 space-y-1">
            <li>Add a new disk to your machine</li>
            <li>Extend the existing mount via LVM, ZFS, or btrfs</li>
            <li>Restart the API container (<code>docker compose restart api</code>)</li>
            <li>Refresh this page — the new capacity appears automatically</li>
          </ol>
          <p className="pt-2">
            <strong>Alternative:</strong> stop the stack, edit <code>STORAGE_ROOT</code> in <code>.env</code> to point at a larger mount, copy the data, restart.
          </p>
        </div>
      </details>
    </Card>
  );
}

// -------- Configuration editor ----------------------------------------------

// -------- Capacity overview ----------------------------------------------

function CapacitySection({ stats, config }: { stats: Stats; config: ConfigEntry[] }) {
  const maxUsers = parseInt(config.find((c) => c.key === "max_users")?.value || "0", 10);
  const maxPct   = parseInt(config.find((c) => c.key === "max_allocation_pct")?.value || "90", 10);
  const effective = stats.disk?.effectiveTotal || 0;
  const allocCap = effective > 0 ? Math.floor(effective * Math.max(1, Math.min(100, maxPct)) / 100) : 0;
  const allocated = stats.disk?.allocatedToUsers ?? stats.totalQuotaBytes;

  const userPct  = maxUsers > 0 ? (stats.userCount / maxUsers) * 100 : 0;
  const allocPctOfCap = allocCap > 0 ? (allocated / allocCap) * 100 : 0;
  const userColor  = colorFor(userPct);
  const allocColor = colorFor(allocPctOfCap);

  return (
    <Card>
      <h2 className="text-sm font-semibold mb-3">Capacity</h2>
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
        <CapacityTile
          label="Active users"
          headline={
            maxUsers === 0
              ? `${stats.userCount.toLocaleString()} / ∞`
              : `${stats.userCount.toLocaleString()} of ${maxUsers.toLocaleString()}`
          }
          subline={maxUsers === 0 ? "max_users is 0 (unlimited)" : `${userPct.toFixed(1)}% of cap`}
          color={userColor}
          pct={maxUsers === 0 ? 0 : userPct}
        />
        <CapacityTile
          label="Allocated to users"
          headline={
            allocCap === 0
              ? formatBytes(allocated)
              : `${formatBytes(allocated)} of ${formatBytes(allocCap)}`
          }
          subline={
            allocCap === 0
              ? "no disk info yet"
              : `${allocPctOfCap.toFixed(1)}% of cap · cap = ${maxPct}% of ${formatBytes(effective)}`
          }
          color={allocColor}
          pct={allocPctOfCap}
        />
      </div>
      {stats.disk?.overcommitted && (
        <p className="mt-3 text-xs text-warning">
          ⚠ overcommit_allowed is on — the allocation cap is bypassed. New approvals will succeed even past the 90% line.
        </p>
      )}
    </Card>
  );
}

function CapacityTile({ label, headline, subline, color, pct }: {
  label: string; headline: string; subline: string;
  color: "success" | "warning" | "danger" | "muted";
  pct: number;
}) {
  const colors: Record<typeof color, { bar: string; text: string }> = {
    success: { bar: "bg-success", text: "text-success" },
    warning: { bar: "bg-warning", text: "text-warning" },
    danger:  { bar: "bg-danger",  text: "text-danger" },
    muted:   { bar: "bg-muted",   text: "text-muted" },
  };
  const c = colors[color];
  return (
    <div className="bg-bg border border-border rounded-md p-4">
      <div className="text-[10px] uppercase tracking-wider text-muted">{label}</div>
      <div className="text-xl font-semibold mt-1">{headline}</div>
      <div className={`text-xs mt-1 ${c.text}`}>{subline}</div>
      <div className="mt-3 h-1.5 w-full bg-border rounded-full overflow-hidden">
        <div className={`h-full ${c.bar} transition-all`}
             style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} />
      </div>
    </div>
  );
}

function colorFor(pct: number): "success" | "warning" | "danger" | "muted" {
  if (pct <= 0)  return "muted";
  if (pct < 75)  return "success";
  if (pct < 90)  return "warning";
  return "danger";
}

// Partition keys into panels. Capacity is checked first so its keys
// don't accidentally land in the Storage panel (max_allocation_pct
// would otherwise match "disk"-ish heuristics).
function isCapacityKey(k: string): boolean {
  return k === "max_users" || k === "max_allocation_pct";
}
function isStorageKey(k: string): boolean {
  if (isCapacityKey(k)) return false;
  return k.includes("quota") || k.includes("bytes") || k.includes("disk") || k === "overcommit_allowed";
}
function isCleanerKey(k: string): boolean {
  return k.startsWith("cleanup_") || k.startsWith("orphan_") || k.startsWith("bloom_");
}

function ConfigSection({
  title, entries, onChange, helper,
}: {
  title: string;
  entries: ConfigEntry[];
  onChange: () => void;
  helper?: string;
}) {
  if (entries.length === 0) return null;
  return (
    <Card>
      <h2 className="text-sm font-semibold mb-1">{title}</h2>
      {helper && <p className="text-xs text-muted mb-3">{helper}</p>}
      <div className={helper ? "space-y-3" : "space-y-3 mt-3"}>
        {entries.map((e) => (
          <ConfigRow key={e.key} entry={e} onSaved={onChange} />
        ))}
      </div>
    </Card>
  );
}

function ConfigRow({ entry, onSaved }: { entry: ConfigEntry; onSaved: () => void }) {
  const [val, setVal] = useState(entry.value);
  const [err, setErr] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const isByteField = entry.key.endsWith("_bytes");
  const isBoolField = entry.value === "true" || entry.value === "false";

  // For byte fields, show in GB for editing
  const valueDisplay = isByteField
    ? (Number(val) / (1024 * 1024 * 1024)).toString()
    : val;

  const save = async () => {
    setErr(null);
    setSaving(true);
    try {
      const sendValue = isByteField
        ? Math.floor(parseFloat(valueDisplay) * 1024 * 1024 * 1024).toString()
        : val;
      await api(`/admin/system-config/${entry.key}`, {
        method: "PATCH",
        body: JSON.stringify({ value: sendValue }),
      });
      onSaved();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "failed");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="border-t border-border pt-3 first:border-t-0 first:pt-0">
      <div className="flex flex-col sm:flex-row sm:items-start gap-2 sm:gap-3">
        <div className="flex-1 min-w-0">
          <p className="text-sm font-mono break-all">{entry.key}</p>
          <p className="text-xs text-muted">{entry.description}</p>
        </div>
        <div className="flex gap-2 items-center flex-wrap">
          {isBoolField ? (
            <select
              value={val}
              onChange={(e) => setVal(e.target.value)}
              className="px-3 py-1.5 bg-panel border border-border rounded text-sm"
            >
              <option value="false">false</option>
              <option value="true">true</option>
            </select>
          ) : (
            <Input
              value={valueDisplay}
              onChange={(e) => {
                if (isByteField) {
                  const gb = parseFloat(e.target.value);
                  setVal(isNaN(gb) ? "0" : Math.floor(gb * 1024 * 1024 * 1024).toString());
                } else {
                  setVal(e.target.value);
                }
              }}
              className="w-32 text-right"
            />
          )}
          {isByteField && <span className="text-xs text-muted">GB</span>}
          <Button size="sm" onClick={save} disabled={saving}>Save</Button>
        </div>
      </div>
      {err && <p className="text-xs text-danger mt-1">{err}</p>}
    </div>
  );
}
