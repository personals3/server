"use client";

import { useEffect, useState, FormEvent, Suspense } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { api, ApiError } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { PageHeader } from "@/components/ui/page-header";
import { SectionHeader } from "@/components/ui/section";
import { EmptyState } from "@/components/ui/empty-state";
import { Badge } from "@/components/ui/badge";
import {
  Database, Trash2, Film, Image as ImageIcon, FolderArchive, Globe, Lock,
  Copy, Check, History, Archive, ArchiveRestore, Plus, X,
} from "lucide-react";

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

const PURPOSE_LABEL: Record<AutoMode, { label: string; Icon: typeof Database }> = {
  none:         { label: "General",       Icon: FolderArchive },
  photos_only:  { label: "Photos",        Icon: ImageIcon },
  media:        { label: "Media library", Icon: Film },
};

// Next 15 requires useSearchParams() be inside a Suspense boundary for the
// static export. Wrap the inner body so build doesn't error out.
export default function BucketsPage() {
  return (
    <Suspense fallback={<div className="text-muted text-sm">Loading...</div>}>
      <BucketsPageInner />
    </Suspense>
  );
}

function BucketsPageInner() {
  const searchParams = useSearchParams();
  // ?create=1 from the dashboard home auto-opens the create form so the
  // "New bucket" buttons keep their promise instead of just navigating here.
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [showCreate, setShowCreate] = useState(searchParams.get("create") === "1");

  const load = () => api<{ buckets: Bucket[] }>("/").then((r) => setBuckets(r.buckets));
  useEffect(() => { void load(); }, []);

  return (
    <div>
      <PageHeader
        title="Buckets"
        description="Containers for your files. Each bucket can have its own purpose, visibility, and version history."
        actions={
          <Button variant="primary" onClick={() => setShowCreate((v) => !v)}>
            {showCreate ? <><X size={14} />Cancel</> : <><Plus size={14} />New bucket</>}
          </Button>
        }
      />

      <div className="space-y-6">
      {showCreate && (
        <Card variant="elevated">
          <CreateBucketForm
            onCreated={() => { setShowCreate(false); void load(); }}
            onCancel={() => setShowCreate(false)}
          />
        </Card>
      )}

      {buckets.length === 0 ? (
        <Card>
          <EmptyState
            icon={<Database size={20} />}
            title="No buckets yet"
            description="Buckets are where your files live. Create your first one to start uploading."
            action={
              <Button onClick={() => setShowCreate(true)}>
                <Plus size={14} /> Create your first bucket
              </Button>
            }
          />
        </Card>
      ) : (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
          {buckets.map((b) => (
            <BucketCard key={b.id} bucket={b} onReload={() => void load()} />
          ))}
        </div>
      )}
      </div>
    </div>
  );
}

// ----------------------------------------------------------------------------

function BucketCard({ bucket: b, onReload }: { bucket: Bucket; onReload: () => void }) {
  const purpose = PURPOSE_LABEL[b.autoTranscodeMode];

  const remove = async () => {
    if (!confirm(`Delete bucket "${b.name}"?`)) return;
    try {
      await api(`/${b.name}`, { method: "DELETE" });
      onReload();
    } catch (e) {
      if (e instanceof ApiError && e.code === "BUCKET_NOT_EMPTY") {
        const count = e.objectCount ?? "some";
        if (confirm(
          `"${b.name}" still has ${count} object(s).\n\n` +
          `Permanently delete the bucket AND all its objects + transcoded streams?\n` +
          `This can't be undone.`,
        )) {
          try {
            await api(`/${b.name}?force=true`, { method: "DELETE" });
            onReload();
          } catch (e2) {
            alert(e2 instanceof ApiError ? e2.message : "force-delete failed");
          }
        }
        return;
      }
      alert(e instanceof ApiError ? e.message : "failed");
    }
  };

  const togglePublic = async () => {
    if (!b.isPublic && !confirm(
      `Make "${b.name}" PUBLIC?\n\n` +
      `Anyone with the URL will be able to LIST and DOWNLOAD all files in this bucket. ` +
      `No login required.\n\nContinue?`,
    )) return;
    try {
      await api(`/${b.name}`, { method: "PATCH", body: JSON.stringify({ isPublic: !b.isPublic }) });
      onReload();
    } catch (e) { alert(e instanceof ApiError ? e.message : "failed"); }
  };

  const toggleVersioning = async () => {
    if (!b.versioning && !confirm(
      `Enable versioning on "${b.name}"?\n\n` +
      `Overwriting or deleting an object will keep the prior copy. All versions count toward your quota.`,
    )) return;
    try {
      await api(`/${b.name}`, { method: "PATCH", body: JSON.stringify({ versioning: !b.versioning }) });
      onReload();
    } catch (e) { alert(e instanceof ApiError ? e.message : "failed"); }
  };

  const toggleArchived = async () => {
    try {
      await api(`/${b.name}`, { method: "PATCH", body: JSON.stringify({ archived: !b.archived }) });
      onReload();
    } catch (e) { alert(e instanceof ApiError ? e.message : "failed"); }
  };

  return (
    <Card className={b.archived ? "opacity-60" : ""}>
      {/* Header: icon + name + actions */}
      <div className="flex items-start justify-between gap-3 mb-4">
        <Link href={`/dashboard/buckets/${b.name}`} className="min-w-0 flex-1 group">
          <div className="flex items-center gap-2 mb-1">
            <Database size={16} className="text-muted shrink-0 group-hover:text-text transition-colors" />
            <h3 className="text-base font-semibold tracking-tight truncate group-hover:text-link transition-colors">
              {b.name}
            </h3>
          </div>
          <p className="text-xs text-muted">
            Created {new Date(b.createdAt).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" })}
          </p>
        </Link>
        <div className="flex items-center gap-1 shrink-0">
          <button
            onClick={toggleArchived}
            className="p-1.5 rounded-md text-muted hover:text-text hover:bg-surface transition-colors"
            title={b.archived ? "Unarchive — resume cleaner walks" : "Archive — cleaner stops walking this bucket"}
          >
            {b.archived ? <ArchiveRestore size={14} /> : <Archive size={14} />}
          </button>
          <button
            onClick={remove}
            className="p-1.5 rounded-md text-muted hover:text-danger hover:bg-surface transition-colors"
            title="Delete bucket"
          >
            <Trash2 size={14} />
          </button>
        </div>
      </div>

      {/* Status badges */}
      <div className="flex flex-wrap gap-1.5 mb-4">
        <Badge variant="neutral"><purpose.Icon size={10} /> {purpose.label}</Badge>
        {b.isPublic ? (
          <Badge variant="warning"><Globe size={10} /> Public</Badge>
        ) : (
          <Badge variant="neutral"><Lock size={10} /> Private</Badge>
        )}
        {b.versioning && <Badge variant="neutral"><History size={10} /> Versioned</Badge>}
        {b.archived && <Badge variant="neutral">Archived</Badge>}
      </div>

      {/* Inline controls */}
      <div className="border-t border-border-subtle pt-3 space-y-2">
        <div className="flex items-center justify-between gap-2 text-xs">
          <label className="text-muted">Visibility</label>
          <button
            onClick={togglePublic}
            className="text-text hover:text-link underline-offset-2 hover:underline"
          >
            {b.isPublic ? "Make private" : "Make public"}
          </button>
        </div>
        <div className="flex items-center justify-between gap-2 text-xs">
          <label className="text-muted">Versioning</label>
          <button
            onClick={toggleVersioning}
            className="text-text hover:text-link underline-offset-2 hover:underline"
          >
            {b.versioning ? "Turn off" : "Enable"}
          </button>
        </div>
        {b.isPublic && <PublicLinkCopy bucket={b.name} />}
      </div>
    </Card>
  );
}

function PublicLinkCopy({ bucket }: { bucket: string }) {
  const [copied, setCopied] = useState(false);
  // Friendly file-explorer URL — what you'd send to a human visitor.
  // The raw /public/<bucket>/ JSON API URL is shown in the bucket detail
  // page banner alongside this one, for scripts / direct embedding.
  const browseURL = typeof window !== "undefined"
    ? `${window.location.origin}/p/${bucket}/`
    : `/p/${bucket}/`;
  const copy = () => {
    navigator.clipboard.writeText(browseURL);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };
  return (
    <button
      onClick={copy}
      className="w-full flex items-center justify-between gap-2 text-[11px] text-muted hover:text-text font-mono px-2 py-1 rounded bg-surface hover:bg-codeBg transition-colors truncate"
      title={`Copy public browse URL: ${browseURL}`}
    >
      <span className="truncate">{browseURL}</span>
      {copied ? <Check size={11} className="text-success shrink-0" /> : <Copy size={11} className="shrink-0" />}
    </button>
  );
}

// ----------------------------------------------------------------------------

function CreateBucketForm({ onCreated, onCancel }: { onCreated: () => void; onCancel: () => void }) {
  const [newName, setNewName] = useState("");
  const [mode, setMode] = useState<AutoMode>("none");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setErr(null);
    setBusy(true);
    try {
      await api(`/${newName}`, {
        method: "PUT",
        body: JSON.stringify({ autoTranscodeMode: mode }),
      });
      onCreated();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={create} className="space-y-5">
      <SectionHeader
        title="Create a new bucket"
        description="Buckets are isolated containers. Pick a purpose so we can pre-configure transcoding for you — you can change it later."
      />

      <div>
        <label className="block text-xs font-medium text-text mb-1.5">Bucket name</label>
        <Input
          value={newName}
          onChange={(e) => setNewName(e.target.value.toLowerCase())}
          placeholder="my-bucket"
          pattern="^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$"
          required
        />
        <p className="text-[11px] text-muted mt-1.5">
          3-63 chars, lowercase letters, digits, dots, hyphens. No underscores or spaces.
        </p>
      </div>

      <div>
        <label className="block text-xs font-medium text-text mb-2">What is this bucket for?</label>
        <div className="grid grid-cols-1 sm:grid-cols-3 gap-2">
          <PurposeCard
            selected={mode === "none"} onClick={() => setMode("none")}
            icon={<FolderArchive size={16} />}
            title="General / backups"
            description="Stores files as-is. No CPU spent on processing. Great for archives, code, documents."
          />
          <PurposeCard
            selected={mode === "photos_only"} onClick={() => setMode("photos_only")}
            icon={<ImageIcon size={16} />}
            title="Photo collection"
            description="Generates fast-loading thumbnails for images. Doesn't touch video or audio."
          />
          <PurposeCard
            selected={mode === "media"} onClick={() => setMode("media")}
            icon={<Film size={16} />}
            title="Media library"
            description="Auto-generates streamable HLS for video & audio, thumbnails for images. Higher CPU + disk."
          />
        </div>
      </div>

      {err && (
        <div className="text-sm text-danger bg-danger/5 border border-danger/20 rounded-md px-3 py-2">
          {err}
        </div>
      )}

      <div className="flex items-center justify-end gap-2 pt-2 border-t border-border-subtle">
        <Button type="button" variant="secondary" onClick={onCancel}>Cancel</Button>
        <Button type="submit" disabled={busy || !newName}>Create bucket</Button>
      </div>
    </form>
  );
}

function PurposeCard({ selected, onClick, icon, title, description }: {
  selected: boolean; onClick: () => void;
  icon: React.ReactNode; title: string; description: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`text-left p-3 rounded-lg border transition-colors ${
        selected
          ? "border-text bg-surface"
          : "border-border-subtle bg-transparent hover:border-border"
      }`}
    >
      <div className="flex items-center gap-2 mb-1">
        <span className={selected ? "text-text" : "text-muted"}>{icon}</span>
        <span className="text-sm font-semibold">{title}</span>
      </div>
      <p className="text-[11px] text-muted leading-relaxed">{description}</p>
    </button>
  );
}
