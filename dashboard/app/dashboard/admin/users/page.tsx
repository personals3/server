"use client";

import { useEffect, useState, FormEvent } from "react";
import { api, ApiError } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { QuotaBar } from "@/components/quota-bar";
import { formatBytes, formatDate } from "@/lib/format";
import { Trash2 } from "lucide-react";

interface AdminUser {
  id: string;
  email: string;
  name: string;
  role: "admin" | "user";
  quotaBytes: number;
  usedBytes: number;
  isActive: boolean;
  createdAt: string;
}

export default function UsersAdminPage() {
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [editing, setEditing] = useState<string | null>(null);

  // Create form state
  const [showCreate, setShowCreate] = useState(false);
  const [nEmail, setNEmail] = useState("");
  const [nName, setNName] = useState("");
  const [nPass, setNPass] = useState("");
  const [nQuotaGB, setNQuotaGB] = useState("10");
  const [nRole, setNRole] = useState<"admin" | "user">("user");
  const [err, setErr] = useState<string | null>(null);

  const load = () => api<{ users: AdminUser[] }>("/admin/users").then((r) => setUsers(r.users));
  useEffect(() => { void load(); }, []);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setErr(null);
    try {
      await api("/admin/users", {
        method: "POST",
        body: JSON.stringify({
          email: nEmail,
          name: nName,
          password: nPass,
          role: nRole,
          quotaBytes: Math.floor(parseFloat(nQuotaGB) * 1024 * 1024 * 1024),
        }),
      });
      setNEmail(""); setNName(""); setNPass(""); setShowCreate(false);
      void load();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "failed");
    }
  };

  const updateQuota = async (id: string, gb: number) => {
    try {
      await api(`/admin/users/${id}`, {
        method: "PATCH",
        body: JSON.stringify({ quotaBytes: Math.floor(gb * 1024 * 1024 * 1024) }),
      });
      void load();
    } catch (e) {
      alert(e instanceof ApiError ? e.message : "failed");
    }
  };

  const toggleActive = async (id: string, active: boolean) => {
    try {
      await api(`/admin/users/${id}`, {
        method: "PATCH",
        body: JSON.stringify({ isActive: active }),
      });
      void load();
    } catch (e) {
      alert(e instanceof ApiError ? e.message : "failed");
    }
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">Users</h1>
        <Button onClick={() => setShowCreate(!showCreate)}>
          {showCreate ? "Cancel" : "+ New user"}
        </Button>
      </div>

      {showCreate && (
        <Card>
          <h2 className="text-sm font-semibold mb-3">Create user</h2>
          <form onSubmit={create} className="space-y-3">
            <div className="grid grid-cols-2 gap-3">
              <div>
                <label className="text-xs text-muted">Email</label>
                <Input type="email" value={nEmail} onChange={(e) => setNEmail(e.target.value)} required />
              </div>
              <div>
                <label className="text-xs text-muted">Name</label>
                <Input value={nName} onChange={(e) => setNName(e.target.value)} required />
              </div>
              <div>
                <label className="text-xs text-muted">Password</label>
                <Input type="password" value={nPass} onChange={(e) => setNPass(e.target.value)} required />
              </div>
              <div>
                <label className="text-xs text-muted">Quota (GB)</label>
                <Input type="number" step="0.1" min="0" value={nQuotaGB}
                       onChange={(e) => setNQuotaGB(e.target.value)} required />
              </div>
              <div className="col-span-2">
                <label className="text-xs text-muted">Role</label>
                <select value={nRole} onChange={(e) => setNRole(e.target.value as "admin" | "user")}
                        className="w-full px-3 py-2 bg-panel border border-border rounded text-sm">
                  <option value="user">user</option>
                  <option value="admin">admin</option>
                </select>
              </div>
            </div>
            {err && <p className="text-sm text-danger">{err}</p>}
            <Button type="submit">Create user</Button>
          </form>
        </Card>
      )}

      <Card className="overflow-x-auto -mx-4 sm:mx-0 sm:overflow-x-visible">
        <table className="stack-rows w-full text-sm min-w-[600px] sm:min-w-0">
          <thead className="text-xs text-muted uppercase">
            <tr>
              <th className="text-left pb-2">User</th>
              <th className="text-left pb-2">Role</th>
              <th className="text-left pb-2 w-64">Usage</th>
              <th className="text-left pb-2">Created</th>
              <th className="text-right pb-2">Actions</th>
            </tr>
          </thead>
          <tbody>
            {users.map((u) => {
              const isEditing = editing === u.id;
              return (
                <tr key={u.id} className={`border-t border-border ${!u.isActive ? "opacity-50" : ""}`}>
                  <td data-label="User" className="py-2">
                    <div className="font-medium">{u.name}</div>
                    <div className="text-xs font-mono text-muted">{u.email}</div>
                  </td>
                  <td data-label="Role" className="py-2">
                    <span className={`text-xs px-2 py-0.5 rounded ${u.role === "admin" ? "bg-accent text-white" : "bg-border"}`}>
                      {u.role}
                    </span>
                  </td>
                  <td data-label="Usage" className="py-2">
                    <QuotaBar used={u.usedBytes} total={u.quotaBytes} />
                    {isEditing ? (
                      <div className="flex gap-1 mt-1">
                        <Input
                          type="number"
                          step="0.1"
                          defaultValue={(u.quotaBytes / 1024 / 1024 / 1024).toFixed(1)}
                          className="h-7 text-xs"
                          onKeyDown={(e) => {
                            if (e.key === "Enter") {
                              const v = parseFloat((e.target as HTMLInputElement).value);
                              if (!isNaN(v)) updateQuota(u.id, v);
                              setEditing(null);
                            }
                          }}
                        />
                        <button onClick={() => setEditing(null)} className="text-xs text-muted">cancel</button>
                      </div>
                    ) : (
                      <button onClick={() => setEditing(u.id)}
                              className="text-xs text-accent hover:underline mt-1">
                        edit quota
                      </button>
                    )}
                  </td>
                  <td data-label="Created" className="py-2 text-muted text-xs">{formatDate(u.createdAt)}</td>
                  <td data-label="" className="actions py-2 text-right">
                    <button onClick={() => toggleActive(u.id, !u.isActive)}
                            className="text-xs text-muted hover:text-accent mr-2">
                      {u.isActive ? "deactivate" : "activate"}
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </Card>
    </div>
  );
}
