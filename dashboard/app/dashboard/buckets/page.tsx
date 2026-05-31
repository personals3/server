"use client";

import { useEffect, useState, FormEvent } from "react";
import Link from "next/link";
import { api, ApiError } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Database, Trash2, Film, Image as ImageIcon, FolderArchive, Globe, Lock, Copy, Check, History, Archive, ArchiveRestore } from "lucide-react";
import { formatDate } from "@/lib/format";

type AutoMode = "none" | "media" | "photos_only";

interface Bucket {
  id: string;
  name: string;
  autoTranscodeMode: AutoMode;
  isPublic: boolean;
  versioning: boolean;
  archived: boolean;
  createdAt: string;
}

export default function BucketsPage() {
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [newName, setNewName] = useState("");
  const [mode, setMode] = useState<AutoMode>("none");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = () => api<{ buckets: Bucket[] }>("/").then((r) => setBuckets(r.buckets));
  useEffect(() => { void load(); }, []);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setErr(null);
    setBusy(true);
    try {
      await api(`/${newName}`, {
        method: "PUT",
        body: JSON.stringify({ autoTranscodeMode: mode }),
      });
      setNewName("");
      setMode("none");
      void load();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (name: string) => {
    if (!confirm(`Delete bucket "${name}"?`)) return;
    try {
      await api(`/${name}`, { method: "DELETE" });
      void load();
      return;
    } catch (e) {
      // Bucket has objects — offer the nuclear option with the count.
      if (e instanceof ApiError && e.code === "BUCKET_NOT_EMPTY") {
        const count = e.objectCount ?? "some";
        if (confirm(
          `"${name}" still has ${count} object(s).\n\n` +
          `Permanently delete the bucket AND all its objects + transcoded streams?\n` +
          `This can't be undone.`,
        )) {
          try {
            await api(`/${name}?force=true`, { method: "DELETE" });
            void load();
            return;
          } catch (e2) {
            alert(e2 instanceof ApiError ? e2.message : "force-delete failed");
            return;
          }
        }
        return;
      }
      alert(e instanceof ApiError ? e.message : "failed");
    }
  };

  const changeMode = async (name: string, newMode: AutoMode) => {
    try {
      await api(`/${name}`, {
        method: "PATCH",
        body: JSON.stringify({ autoTranscodeMode: newMode }),
      });
      void load();
    } catch (e) {
      alert(e instanceof ApiError ? e.message : "failed");
    }
  };

  const toggleVersioning = async (name: string, current: boolean) => {
    if (!current && !confirm(
      `Enable versioning on "${name}"?\n\n` +
      `From now on, overwriting or deleting an object will keep the prior copy. ` +
      `All versions count toward your storage quota.\n\n` +
      `You can turn this off later — existing versions stay accessible.`,
    )) return;
    try {
      await api(`/${name}`, {
        method: "PATCH",
        body: JSON.stringify({ versioning: !current }),
      });
      void load();
    } catch (e) {
      alert(e instanceof ApiError ? e.message : "failed");
    }
  };

  const toggleArchived = async (name: string, current: boolean) => {
    try {
      await api(`/${name}`, {
        method: "PATCH",
        body: JSON.stringify({ archived: !current }),
      });
      void load();
    } catch (e) {
      alert(e instanceof ApiError ? e.message : "failed");
    }
  };

  const togglePublic = async (name: string, current: boolean) => {
    if (!current && !confirm(
      `Make "${name}" PUBLIC?\n\n` +
      `Anyone with the URL will be able to LIST and DOWNLOAD all files in this bucket. ` +
      `No login required. The URL pattern is /public/${name}/{key}.\n\n` +
      `Continue?`,
    )) return;
    try {
      await api(`/${name}`, {
        method: "PATCH",
        body: JSON.stringify({ isPublic: !current }),
      });
      void load();
    } catch (e) {
      alert(e instanceof ApiError ? e.message : "failed");
    }
  };

  return (
    <div className="space-y-6">
      <h1 className="text-2xl font-bold">Buckets</h1>

      <Card>
        <h2 className="text-sm font-semibold mb-3">Create bucket</h2>
        <form onSubmit={create} className="space-y-3">
          <div className="flex flex-wrap gap-2">
            <Input
              value={newName}
              onChange={(e) => setNewName(e.target.value.toLowerCase())}
              placeholder="my-bucket"
              pattern="^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$"
              required
            />
            <Button type="submit" disabled={busy || !newName}>Create</Button>
          </div>
          <p className="text-xs text-muted">
            3-63 chars, lowercase letters/digits/dots/hyphens. No underscores or spaces.
          </p>

          <div>
            <p className="text-xs font-semibold mb-2">What is this bucket for?</p>
            <div className="grid grid-cols-1 sm:grid-cols-3 gap-2">
              <PurposeCard
                kind="none"
                selected={mode === "none"}
                onClick={() => setMode("none")}
                icon={<FolderArchive size={16} />}
                title="General / backups"
                description="Stores files as-is. No CPU spent on processing. Great for archives, code, documents."
              />
              <PurposeCard
                kind="photos_only"
                selected={mode === "photos_only"}
                onClick={() => setMode("photos_only")}
                icon={<ImageIcon size={16} />}
                title="Photo collection"
                description="Generates fast-loading thumbnails for images. Doesn't touch video or audio."
              />
              <PurposeCard
                kind="media"
                selected={mode === "media"}
                onClick={() => setMode("media")}
                icon={<Film size={16} />}
                title="Media library"
                description="Auto-generates streamable HLS for video & audio, thumbnails for images. Higher CPU + disk."
              />
            </div>
            <p className="text-[11px] text-muted mt-2">
              You can change this later. Existing files in any bucket can still
              be transcoded manually from the file preview panel.
            </p>
          </div>

          {err && <p className="text-sm text-danger">{err}</p>}
        </form>
      </Card>

      <Card>
        <h2 className="text-sm font-semibold mb-3">All buckets</h2>
        {buckets.length === 0 ? (
          <p className="text-sm text-muted">No buckets yet.</p>
        ) : (
          <div className="overflow-x-auto -mx-4 sm:mx-0">
            <table className="stack-rows w-full text-sm table-fixed min-w-[600px] sm:min-w-0">
              <colgroup>
                <col style={{ width: "30%" }} />
                <col style={{ width: "25%" }} />
                <col style={{ width: "20%" }} />
                <col style={{ width: "17%" }} />
                <col style={{ width: "8%" }} />
              </colgroup>
              <thead className="text-xs text-muted uppercase">
                <tr>
                  <th className="text-left pb-2">Name</th>
                  <th className="text-left pb-2">Purpose</th>
                  <th className="text-left pb-2">Access</th>
                  <th className="text-left pb-2">Created</th>
                  <th className="text-right pb-2">Actions</th>
                </tr>
              </thead>
              <tbody>
                {buckets.map((b) => (
                  <tr key={b.id} className={`border-t border-border ${b.archived ? "opacity-50" : ""}`}>
                    <td data-label="Name" className="py-2">
                      <Link href={`/dashboard/buckets/${b.name}`}
                            className="flex items-center gap-2 hover:text-accent">
                        <Database size={14} className="text-muted" /> {b.name}
                        {b.archived && (
                          <span className="px-1 bg-zinc-500/30 text-zinc-300 rounded text-[9px]" title="Cleaner skips this bucket's shards">ARCHIVED</span>
                        )}
                      </Link>
                    </td>
                    <td data-label="Purpose" className="py-2">
                      <select
                        value={b.autoTranscodeMode}
                        onChange={(e) => changeMode(b.name, e.target.value as AutoMode)}
                        className="bg-panel border border-border rounded px-2 py-1 text-xs"
                      >
                        <option value="none">General / backups</option>
                        <option value="photos_only">Photo collection</option>
                        <option value="media">Media library</option>
                      </select>
                    </td>
                    <td data-label="Access" className="py-2">
                      <div className="flex flex-col gap-1 items-start">
                        <AccessToggle bucket={b} onToggle={() => togglePublic(b.name, b.isPublic)} />
                        <VersioningToggle bucket={b} onToggle={() => toggleVersioning(b.name, b.versioning)} />
                      </div>
                    </td>
                    <td data-label="Created" className="py-2 text-muted">{formatDate(b.createdAt)}</td>
                    <td data-label="" className="actions py-2 text-right space-x-2">
                      <button onClick={() => toggleArchived(b.name, b.archived)}
                              className="text-muted hover:text-text"
                              title={b.archived ? "Unarchive — resume cleaner walks" : "Archive — cleaner stops walking this bucket"}>
                        {b.archived ? <ArchiveRestore size={14} /> : <Archive size={14} />}
                      </button>
                      <button onClick={() => remove(b.name)}
                              className="text-danger hover:text-red-400" title="Delete bucket">
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
    </div>
  );
}

function AccessToggle({ bucket, onToggle }: { bucket: Bucket; onToggle: () => void }) {
  const [copied, setCopied] = useState(false);
  const publicUrl = typeof window !== "undefined"
    ? `${window.location.origin}/public/${bucket.name}/`
    : `/public/${bucket.name}/`;

  const copy = (e: React.MouseEvent) => {
    e.stopPropagation();
    navigator.clipboard.writeText(publicUrl);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  return (
    <div className="flex items-center gap-2">
      <button
        onClick={onToggle}
        className={`flex items-center gap-1.5 px-2 py-1 text-xs rounded border transition ${
          bucket.isPublic
            ? "border-amber-500/50 bg-amber-500/10 text-amber-300 hover:bg-amber-500/20"
            : "border-border bg-bg text-muted hover:border-muted"
        }`}
        title={bucket.isPublic ? "Anyone with the URL can read. Click to make private." : "Owner only. Click to make public."}
      >
        {bucket.isPublic ? <Globe size={12} /> : <Lock size={12} />}
        {bucket.isPublic ? "Public" : "Private"}
      </button>
      {bucket.isPublic && (
        <button onClick={copy}
                className="text-muted hover:text-text"
                title={`Copy ${publicUrl}`}>
          {copied ? <Check size={12} className="text-success" /> : <Copy size={12} />}
        </button>
      )}
    </div>
  );
}

function VersioningToggle({ bucket, onToggle }: { bucket: Bucket; onToggle: () => void }) {
  return (
    <button
      onClick={onToggle}
      className={`flex items-center gap-1.5 px-2 py-1 text-xs rounded border transition ${
        bucket.versioning
          ? "border-blue-500/50 bg-blue-500/10 text-blue-300 hover:bg-blue-500/20"
          : "border-border bg-bg text-muted hover:border-muted"
      }`}
      title={bucket.versioning
        ? "Overwrites/deletes keep prior versions (all count toward quota). Click to turn off."
        : "Overwrites/deletes are permanent. Click to enable version history."}
    >
      <History size={12} />
      {bucket.versioning ? "Versioned" : "No versions"}
    </button>
  );
}

function PurposeCard({
  kind, selected, onClick, icon, title, description,
}: {
  kind: AutoMode;
  selected: boolean;
  onClick: () => void;
  icon: React.ReactNode;
  title: string;
  description: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`text-left p-3 rounded border transition ${
        selected
          ? "border-accent bg-blue-950/30"
          : "border-border bg-bg hover:border-muted"
      }`}
    >
      <div className="flex items-center gap-2 mb-1">
        <span className={selected ? "text-accent" : "text-muted"}>{icon}</span>
        <span className="text-sm font-semibold">{title}</span>
      </div>
      <p className="text-[11px] text-muted">{description}</p>
    </button>
  );
}
