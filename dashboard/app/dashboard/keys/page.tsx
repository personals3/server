"use client";

import { useEffect, useState, FormEvent } from "react";
import { api, ApiError } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Trash2, Copy, Check } from "lucide-react";
import { formatDate } from "@/lib/format";

interface KeyDTO {
  id: string;
  name: string;
  keyPrefix: string;
  lastUsedAt?: string;
  createdAt: string;
}

interface S3CredDTO {
  accessKeyId: string;
  name: string;
  lastUsedAt?: string;
  createdAt: string;
}

export default function KeysPage() {
  const [keys, setKeys] = useState<KeyDTO[]>([]);
  const [s3Creds, setS3Creds] = useState<S3CredDTO[]>([]);

  const load = () => {
    void api<{ keys: KeyDTO[] }>("/auth/keys").then((r) => setKeys(r.keys));
    void api<{ credentials: S3CredDTO[] }>("/auth/s3-credentials").then((r) => setS3Creds(r.credentials));
  };
  useEffect(load, []);

  return (
    <div className="space-y-6">
      <h1 className="text-2xl font-bold">Credentials</h1>

      <BearerKeysSection keys={keys} reload={load} />
      <S3CredsSection creds={s3Creds} reload={load} />
    </div>
  );
}

function BearerKeysSection({ keys, reload }: { keys: KeyDTO[]; reload: () => void }) {
  const [newName, setNewName] = useState("");
  const [newKey, setNewKey] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setErr(null); setNewKey(null); setCopied(false);
    try {
      const r = await api<{ key: string }>("/auth/keys", {
        method: "POST", body: JSON.stringify({ name: newName }),
      });
      setNewKey(r.key); setNewName(""); reload();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "failed");
    }
  };

  const revoke = async (id: string) => {
    if (!confirm("Revoke this API key?")) return;
    try { await api(`/auth/keys/${id}`, { method: "DELETE" }); reload(); }
    catch (e) { alert(e instanceof ApiError ? e.message : "failed"); }
  };

  return (
    <Card>
      <h2 className="text-lg font-semibold mb-1">Bearer API Keys</h2>
      <p className="text-xs text-muted mb-4">
        Use with <code className="font-mono">Authorization: Bearer psk_...</code> headers
        — recommended for our native API and scripts.
      </p>

      <form onSubmit={create} className="flex flex-wrap gap-2">
        <Input value={newName} onChange={(e) => setNewName(e.target.value)}
               placeholder="e.g. laptop, backup-script" required />
        <Button type="submit">Create</Button>
      </form>
      {err && <p className="text-sm text-danger mt-2">{err}</p>}

      {newKey && (
        <div className="mt-3 p-3 bg-bg border border-warning/50 rounded">
          <p className="text-xs text-warning mb-2">Save this key now — it cannot be shown again</p>
          <div className="flex items-center gap-2">
            <code className="text-xs font-mono break-all flex-1">{newKey}</code>
            <button onClick={() => { navigator.clipboard.writeText(newKey); setCopied(true); }}
                    className="text-muted hover:text-accent shrink-0">
              {copied ? <Check size={16} className="text-success" /> : <Copy size={16} />}
            </button>
          </div>
        </div>
      )}

      {keys.length > 0 && (
        <div className="overflow-x-auto -mx-4 sm:mx-0 mt-4">
          <table className="stack-rows w-full text-sm min-w-[500px] sm:min-w-0">
            <thead className="text-xs text-muted uppercase">
              <tr><th className="text-left pb-2">Name</th><th className="text-left pb-2">Prefix</th>
                  <th className="text-left pb-2">Last used</th><th className="text-right pb-2"></th></tr>
            </thead>
            <tbody>
              {keys.map((k) => (
                <tr key={k.id} className="border-t border-border">
                  <td data-label="Name" className="py-2">{k.name || <span className="text-muted">unnamed</span>}</td>
                  <td data-label="Prefix" className="py-2 font-mono text-xs">psk_{k.keyPrefix}...</td>
                  <td data-label="Last used" className="py-2 text-muted">{k.lastUsedAt ? formatDate(k.lastUsedAt) : "never"}</td>
                  <td data-label="" className="actions py-2 text-right">
                    <button onClick={() => revoke(k.id)} className="text-danger"><Trash2 size={14} /></button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

function S3CredsSection({ creds, reload }: { creds: S3CredDTO[]; reload: () => void }) {
  const [newName, setNewName] = useState("");
  const [newCred, setNewCred] = useState<{ akid: string; secret: string } | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setErr(null); setNewCred(null);
    try {
      const r = await api<{ accessKeyId: string; secretAccessKey: string }>(
        "/auth/s3-credentials",
        { method: "POST", body: JSON.stringify({ name: newName }) },
      );
      setNewCred({ akid: r.accessKeyId, secret: r.secretAccessKey });
      setNewName(""); reload();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "failed");
    }
  };

  const revoke = async (akid: string) => {
    if (!confirm("Revoke this S3 credential?")) return;
    try { await api(`/auth/s3-credentials/${akid}`, { method: "DELETE" }); reload(); }
    catch (e) { alert(e instanceof ApiError ? e.message : "failed"); }
  };

  return (
    <Card>
      <h2 className="text-lg font-semibold mb-1">S3 Credentials (AWS SigV4)</h2>
      <p className="text-xs text-muted mb-4">
        Use with AWS CLI / boto3 / aws-sdk-* when you need S3 compatibility.
        Configure with: <code className="font-mono">aws --endpoint-url=&lt;your-url&gt;/api s3 ...</code>
      </p>

      <form onSubmit={create} className="flex flex-wrap gap-2">
        <Input value={newName} onChange={(e) => setNewName(e.target.value)}
               placeholder="e.g. backup-job" required />
        <Button type="submit">Create</Button>
      </form>
      {err && <p className="text-sm text-danger mt-2">{err}</p>}

      {newCred && (
        <div className="mt-3 p-3 bg-bg border border-warning/50 rounded space-y-2">
          <p className="text-xs text-warning">Save these credentials now — the secret cannot be shown again</p>
          <div>
            <p className="text-[10px] text-muted">AWS_ACCESS_KEY_ID</p>
            <code className="text-xs font-mono break-all">{newCred.akid}</code>
          </div>
          <div>
            <p className="text-[10px] text-muted">AWS_SECRET_ACCESS_KEY</p>
            <code className="text-xs font-mono break-all">{newCred.secret}</code>
          </div>
        </div>
      )}

      {creds.length > 0 && (
        <div className="overflow-x-auto -mx-4 sm:mx-0 mt-4">
          <table className="stack-rows w-full text-sm min-w-[500px] sm:min-w-0">
            <thead className="text-xs text-muted uppercase">
              <tr><th className="text-left pb-2">Name</th><th className="text-left pb-2">Access Key ID</th>
                  <th className="text-left pb-2">Last used</th><th className="text-right pb-2"></th></tr>
            </thead>
            <tbody>
              {creds.map((c) => (
                <tr key={c.accessKeyId} className="border-t border-border">
                  <td data-label="Name" className="py-2">{c.name || <span className="text-muted">unnamed</span>}</td>
                  <td data-label="Access Key ID" className="py-2 font-mono text-xs">{c.accessKeyId}</td>
                  <td data-label="Last used" className="py-2 text-muted">{c.lastUsedAt ? formatDate(c.lastUsedAt) : "never"}</td>
                  <td data-label="" className="actions py-2 text-right">
                    <button onClick={() => revoke(c.accessKeyId)} className="text-danger">
                      <Trash2 size={14} />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}
