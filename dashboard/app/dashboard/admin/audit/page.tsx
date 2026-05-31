"use client";

import { useEffect, useState, FormEvent } from "react";
import { api } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { formatBytes, formatDate } from "@/lib/format";
import { RefreshCw } from "lucide-react";

interface AuditEntry {
  id: number;
  userEmail?: string;
  action: string;
  bucket?: string;
  key?: string;
  sizeBytes?: number;
  statusCode?: number;
  ipAddress?: string;
  ts: string;
}

export default function AuditPage() {
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [user, setUser] = useState("");
  const [action, setAction] = useState("");
  const [loading, setLoading] = useState(false);

  const load = async () => {
    setLoading(true);
    const qs = new URLSearchParams();
    if (user) qs.set("user", user);
    if (action) qs.set("action", action);
    qs.set("limit", "200");
    try {
      const r = await api<{ entries: AuditEntry[] }>(`/admin/audit?${qs.toString()}`);
      setEntries(r.entries);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => { void load(); /* eslint-disable-line */ }, []);

  const apply = (e: FormEvent) => { e.preventDefault(); void load(); };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">Audit Log</h1>
        <Button variant="ghost" onClick={() => void load()} disabled={loading}>
          <RefreshCw size={14} className={`mr-1 ${loading ? "animate-spin" : ""}`} />
          Refresh
        </Button>
      </div>

      <Card>
        <form onSubmit={apply} className="grid grid-cols-1 sm:grid-cols-3 gap-3">
          <div>
            <label className="text-xs text-muted">User (email contains)</label>
            <Input value={user} onChange={(e) => setUser(e.target.value)} placeholder="user@example.com" />
          </div>
          <div>
            <label className="text-xs text-muted">Action (exact)</label>
            <select value={action} onChange={(e) => setAction(e.target.value)}
                    className="w-full px-3 py-2 bg-panel border border-border rounded text-sm">
              <option value="">(any)</option>
              <option>LOGIN</option>
              <option>GET_ME</option>
              <option>CREATE_API_KEY</option>
              <option>LIST_API_KEYS</option>
              <option>REVOKE_API_KEY</option>
              <option>LIST_BUCKETS</option>
              <option>CREATE_BUCKET</option>
              <option>DELETE_BUCKET</option>
              <option>HEAD_BUCKET</option>
              <option>LIST_OBJECTS</option>
              <option>PUT_OBJECT</option>
              <option>GET_OBJECT</option>
              <option>HEAD_OBJECT</option>
              <option>DELETE_OBJECT</option>
            </select>
          </div>
          <div className="flex items-end">
            <Button type="submit" className="w-full">Apply filters</Button>
          </div>
        </form>
      </Card>

      <Card>
        <p className="text-xs text-muted mb-2">{entries.length} entries</p>
        <div className="overflow-x-auto -mx-4 sm:mx-0">
          <table className="stack-rows w-full text-xs min-w-[600px] sm:min-w-0">
            <thead className="text-[10px] text-muted uppercase">
              <tr>
                <th className="text-left pb-2">Time</th>
                <th className="text-left pb-2">User</th>
                <th className="text-left pb-2">Action</th>
                <th className="text-left pb-2">Bucket / Key</th>
                <th className="text-left pb-2">Size</th>
                <th className="text-left pb-2">Status</th>
                <th className="text-left pb-2">IP</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => (
                <tr key={e.id} className="border-t border-border">
                  <td data-label="Time" className="py-1.5 whitespace-nowrap">{formatDate(e.ts)}</td>
                  <td data-label="User" className="py-1.5 font-mono">{e.userEmail || "-"}</td>
                  <td data-label="Action" className="py-1.5 font-mono">{e.action}</td>
                  <td data-label="Bucket / Key" className="py-1.5 font-mono">
                    {e.bucket}{e.key ? "/" + e.key : ""}
                  </td>
                  <td data-label="Size" className="py-1.5">{e.sizeBytes ? formatBytes(e.sizeBytes) : "-"}</td>
                  <td data-label="Status" className="py-1.5">
                    <span className={
                      !e.statusCode ? "" :
                      e.statusCode < 300 ? "text-success" :
                      e.statusCode < 400 ? "text-warning" : "text-danger"
                    }>
                      {e.statusCode || "-"}
                    </span>
                  </td>
                  <td data-label="IP" className="py-1.5 text-muted">{e.ipAddress || "-"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>
    </div>
  );
}
