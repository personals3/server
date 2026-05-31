"use client";

import { useEffect, useState, useCallback } from "react";
import { useParams, useSearchParams, useRouter, usePathname } from "next/navigation";
import { api, API, STREAM, getToken, ApiError } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { UploadZone } from "@/components/upload-zone";
import { FolderUpload } from "@/components/folder-upload";
import { ImportURL } from "@/components/import-url";
import { ImportStatus } from "@/components/import-status";
import { VideoPlayer } from "@/components/video-player";
import { Thumb } from "@/components/thumb";
import { useToast } from "@/components/toast";
import { ShareModal } from "@/components/share-modal";
import { VersionsModal } from "@/components/versions-modal";
import { formatBytes, formatDate, classify } from "@/lib/format";
import { Trash2, Download, FileText, FileVideo, FileAudio, FileImage, RefreshCw, Copy, Check, ExternalLink, Share2, Globe, History, Folder, FolderUp, ChevronRight, Home } from "lucide-react";

interface ObjectDTO {
  key: string;
  size: number;
  etag: string;
  contentType: string;
  lastModified: string;
}

interface ObjectDetail extends ObjectDTO {
  transcoded?: {
    type: string;
    master?: string;
    hls?: string;
    webp?: string;
    avif?: string | null;
  };
  transcodeStatus?: string;
  objectId?: string;
}

export default function BucketPage() {
  const { name } = useParams<{ name: string }>();
  const searchParams = useSearchParams();
  const router = useRouter();
  const pathname = usePathname();

  // The current folder prefix (always ends with "/" unless empty = root).
  // Backed by ?path= so refresh / share-link keeps you in the same folder.
  const path = normalizePath(searchParams.get("path") || "");

  const [objects, setObjects] = useState<ObjectDTO[]>([]);
  const [folders, setFolders] = useState<string[]>([]);
  const [selected, setSelected] = useState<ObjectDetail | null>(null);
  const [err, setErr] = useState<string | null>(null);
  // Set of full keys currently checked for bulk operations
  const [picks, setPicks] = useState<Set<string>>(new Set());
  const [isPublic, setIsPublic] = useState(false);
  const [versioning, setVersioning] = useState(false);

  const navigateTo = (newPath: string) => {
    const np = normalizePath(newPath);
    const qs = np ? `?path=${encodeURIComponent(np)}` : "";
    router.push(pathname + qs);
  };

  // Load the bucket's settings. Cheap: one row from the buckets list
  // endpoint, filtered by name.
  const loadSettings = useCallback(async () => {
    try {
      const r = await api<{ buckets: { name: string; isPublic: boolean; versioning: boolean }[] }>("/");
      const me = r.buckets.find((b) => b.name === name);
      if (me) {
        setIsPublic(me.isPublic);
        setVersioning(me.versioning);
      }
    } catch { /* not fatal */ }
  }, [name]);

  const load = useCallback(async () => {
    try {
      const qs = new URLSearchParams({ delimiter: "/" });
      if (path) qs.set("prefix", path);
      const r = await api<{
        objects: ObjectDTO[];
        commonPrefixes: string[];
      }>(`/${name}?${qs.toString()}`);
      // Hide hidden files (anything whose basename starts with ".") from the
      // file list — that's where folder marker files like .keep live.
      const visibleObjects = (r.objects ?? []).filter((o) => {
        const base = o.key.split("/").pop() || "";
        return !base.startsWith(".");
      });
      setObjects(visibleObjects);
      setFolders(r.commonPrefixes ?? []);
      // Drop any picks no longer visible
      setPicks((prev) => {
        const present = new Set(visibleObjects.map((o) => o.key));
        const next = new Set<string>();
        prev.forEach((k) => { if (present.has(k)) next.add(k); });
        return next;
      });
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "failed to load");
    }
  }, [name, path]);

  const toggleAll = (checked: boolean) => {
    setPicks(checked ? new Set(objects.map((o) => o.key)) : new Set());
  };
  const togglePick = (key: string) => {
    setPicks((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const bulkDelete = async (skipTrash = false) => {
    const keys = Array.from(picks);
    if (keys.length === 0) return;
    const verb = skipTrash ? "Permanently delete" : "Delete";
    const tail = skipTrash ? "Bypasses trash — bytes cannot be recovered." : "Items move to trash.";
    if (!confirm(`${verb} ${keys.length} selected object(s)? ${tail}`)) return;
    try {
      if (skipTrash) {
        // No bulk-purge endpoint; iterate per-key with ?purge.
        let ok = 0, fail = 0;
        for (const k of keys) {
          try {
            await api(`/${name}/${encodeKey(k)}?purge`, { method: "DELETE" });
            ok++;
          } catch { fail++; }
        }
        if (fail > 0) alert(`Purged ${ok}, ${fail} failed.`);
      } else {
        const r = await api<{ deleted: number; errors: { key: string; error: string }[] }>(
          `/${name}?delete`,
          { method: "POST", body: JSON.stringify({ keys }) },
        );
        if (r.errors.length > 0) {
          alert(`Deleted ${r.deleted}, ${r.errors.length} failed:\n` +
            r.errors.slice(0, 5).map((e) => `${e.key}: ${e.error}`).join("\n"));
        }
      }
      setPicks(new Set());
      void load();
    } catch (e) {
      alert(e instanceof ApiError ? e.message : "bulk delete failed");
    }
  };

  useEffect(() => { void load(); void loadSettings(); }, [load, loadSettings]);

  // Keyboard navigation for the file list.
  //   ↑/↓     : move cursor through visible rows (folders + files)
  //   Enter   : open the highlighted row (drill into folder or open preview)
  //   Delete  : trash the highlighted file (Shift+Delete = ?purge)
  //   Cmd/Ctrl+A : select all files
  //   Escape  : clear selection
  const [cursor, setCursor] = useState<number>(-1);
  const totalRows = folders.length + objects.length;

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // Don't hijack typing in inputs/textareas.
      const tag = (e.target as HTMLElement | null)?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || (e.target as HTMLElement | null)?.isContentEditable) return;
      if (selected || shareOpenRef.current) return; // preview/modals own keys

      if (e.key === "ArrowDown") {
        e.preventDefault();
        setCursor((c) => Math.min(totalRows - 1, c + 1));
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        setCursor((c) => Math.max(0, c - 1));
      } else if (e.key === "Enter") {
        e.preventDefault();
        if (cursor < 0) return;
        if (cursor < folders.length) {
          navigateTo(folders[cursor]);
        } else {
          open(objects[cursor - folders.length]);
        }
      } else if (e.key === "Delete" || e.key === "Backspace") {
        e.preventDefault();
        if (cursor < 0 || cursor < folders.length) return;
        void remove(objects[cursor - folders.length].key, e.shiftKey);
      } else if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "a") {
        e.preventDefault();
        toggleAll(picks.size !== objects.length);
      } else if (e.key === "Escape") {
        setPicks(new Set());
        setCursor(-1);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
    // intentionally minimal deps; closures capture latest state via setCursor functional updates
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cursor, totalRows, selected, folders, objects, picks.size]);

  // Reset cursor when the path changes (different folder = different rows)
  useEffect(() => setCursor(-1), [path]);

  const shareOpenRef = { current: false }; // placeholder — wired via setSelected above

  // When an object is clicked we don't have transcoded info from list,
  // so we'd need an admin endpoint to get it. For Part 7 we infer from
  // contentType and try the stream URL; player handles 404 gracefully.
  const open = (o: ObjectDTO) => {
    setSelected({ ...o });
  };

  const remove = async (key: string, skipTrash = false) => {
    const verb = skipTrash ? "Permanently delete" : "Delete";
    const tail = skipTrash ? " — bytes cannot be recovered." : "";
    if (!confirm(`${verb} "${key}"?${tail}`)) return;
    try {
      const url = skipTrash
        ? `/${name}/${encodeKey(key)}?purge`
        : `/${name}/${encodeKey(key)}`;
      await api(url, { method: "DELETE" });
      if (selected?.key === key) setSelected(null);
      void load();
    } catch (e) {
      alert(e instanceof ApiError ? e.message : "failed");
    }
  };

  // Delete a folder = bulk-delete every object whose key starts with this prefix.
  // Lists first (flat, no delimiter so we pick up nested files too) then sends
  // them through the existing POST ?delete bulk endpoint.
  const deleteFolder = async (prefix: string) => {
    try {
      const r = await api<{ objects: { key: string }[] }>(
        `/${name}?prefix=${encodeURIComponent(prefix)}`,
      );
      const keys = r.objects.map((o) => o.key);
      if (keys.length === 0) {
        alert(`Folder "${prefix}" is already empty.`);
        void load();
        return;
      }
      if (!confirm(
        `Delete folder "${prefix}" and all ${keys.length} file(s) inside?\n\n` +
        `Files will be moved to trash (or hard-deleted if you've turned trash off).`,
      )) return;
      const del = await api<{ deleted: number; errors: { key: string; error: string }[] }>(
        `/${name}?delete`,
        { method: "POST", body: JSON.stringify({ keys }) },
      );
      if (del.errors.length > 0) {
        alert(`Deleted ${del.deleted}, ${del.errors.length} failed:\n` +
              del.errors.slice(0, 5).map((e) => `${e.key}: ${e.error}`).join("\n"));
      }
      void load();
    } catch (e) {
      alert(e instanceof ApiError ? e.message : "failed");
    }
  };

  const download = (key: string) => {
    // Authenticated download via fetch then blob URL (browser can't send
    // Authorization header to a raw <a href>)
    const token = getToken();
    fetch(`${API}/${name}/${encodeKey(key)}`, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    })
      .then((r) => r.blob())
      .then((blob) => {
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = key.split("/").pop() || "download";
        a.click();
        URL.revokeObjectURL(url);
      });
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between gap-3 flex-wrap">
        <h1 className="text-2xl font-bold min-w-0 break-all">
          <span className="text-muted">Buckets / </span>
          <span className="font-mono">{name}</span>
        </h1>
        <Button variant="ghost" onClick={() => void load()}>
          <RefreshCw size={14} className="mr-1" /> Refresh
        </Button>
      </div>

      {isPublic && <PublicBanner bucket={name} />}

      <Card>
        <h2 className="text-sm font-semibold mb-3">
          Upload {path && <span className="text-muted font-normal">→ <span className="font-mono">{path}</span></span>}
        </h2>
        <UploadZone bucket={name} prefix={path} onComplete={() => void load()} />
        <div className="mt-4 border-t border-border pt-3">
          <div className="text-[11px] uppercase tracking-wide text-muted mb-2">
            Other ways to add files
          </div>
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
            <FolderUpload bucket={name} prefix={path} onComplete={() => void load()} />
            <ImportURL bucket={name} prefix={path} onComplete={() => void load()} />
          </div>
        </div>
        <div className="mt-3">
          <ImportStatus bucket={name} onJobDone={() => void load()} />
        </div>
      </Card>

      {selected && <PreviewCard bucket={name} obj={selected} versioning={versioning} onClose={() => setSelected(null)} onReload={() => void load()} />}

      <Card>
        <div className="flex items-center justify-between mb-3 gap-3 flex-wrap">
          <Breadcrumb path={path} onNavigate={navigateTo} />
          <div className="flex items-center gap-3 flex-wrap">
            <NewFolderButton path={path} onCreated={() => void load()} bucket={name} />
            {picks.size > 0 && (
              <button
                onClick={(e) => bulkDelete(e.shiftKey)}
                className="text-xs text-danger hover:underline inline-flex items-center gap-1"
                title="Click to trash; Shift-click to permanently delete (skip trash)"
              >
                <Trash2 size={12} /> Delete {picks.size} selected
              </button>
            )}
          </div>
        </div>
        {err && <p className="text-sm text-danger">{err}</p>}
        {folders.length === 0 && objects.length === 0 ? (
          <p className="text-sm text-muted">
            {path ? "This folder is empty." : "Empty bucket. Upload something above."}
          </p>
        ) : (
          <div className="overflow-x-auto -mx-4 sm:mx-0">
          <table className="stack-rows w-full text-sm sm:table-fixed min-w-[600px] sm:min-w-0">
            <colgroup>
              <col style={{ width: "30px" }} />
              <col />
              <col style={{ width: "90px" }} />
              <col style={{ width: "180px" }} />
              <col style={{ width: "70px" }} />
            </colgroup>
            <thead className="text-xs text-muted uppercase">
              <tr>
                <th className="pb-2">
                  <input
                    type="checkbox"
                    checked={picks.size === objects.length && objects.length > 0}
                    ref={(el) => {
                      if (el) el.indeterminate = picks.size > 0 && picks.size < objects.length;
                    }}
                    onChange={(e) => toggleAll(e.target.checked)}
                    className="cursor-pointer"
                    title="Select all visible files (folders excluded)"
                  />
                </th>
                <th className="text-left pb-2">Name</th>
                <th className="text-left pb-2">Size</th>
                <th className="text-left pb-2">Modified</th>
                <th className="text-right pb-2">Actions</th>
              </tr>
            </thead>
            <tbody>
              {path && (
                <tr className="border-t border-border hover:bg-panel cursor-pointer"
                    onClick={() => navigateTo(parentOf(path))}>
                  <td className="py-2" data-label="" />
                  <td className="py-2 pr-3 min-w-0" data-label="Name">
                    <span className="flex items-center gap-2 font-mono text-muted">
                      <FolderUp size={14} /> ..
                    </span>
                  </td>
                  <td colSpan={3} data-label="" />
                </tr>
              )}
              {folders.map((cp, i) => {
                const display = trimPrefix(cp, path).replace(/\/$/, "");
                return (
                  <tr key={cp}
                      className={`border-t border-border hover:bg-panel cursor-pointer group ${cursor === i ? "bg-panel" : ""}`}
                      onClick={() => navigateTo(cp)}>
                    <td className="py-2" data-label="" />
                    <td className="py-2 pr-3 min-w-0" data-label="Name">
                      <span className="flex items-center gap-2 font-mono">
                        <Folder size={14} className="text-accent" />
                        <span className="truncate">{display}/</span>
                      </span>
                    </td>
                    <td className="py-2 text-muted text-xs" data-label="Size">folder</td>
                    <td className="py-2" data-label="Modified" />
                    <td className="py-2 text-right actions" data-label="">
                      <button
                        onClick={(e) => { e.stopPropagation(); void deleteFolder(cp); }}
                        className="text-danger hover:text-red-400 opacity-0 group-hover:opacity-100 transition"
                        title={`Delete folder ${cp} and everything in it`}
                      >
                        <Trash2 size={14} />
                      </button>
                    </td>
                  </tr>
                );
              })}
              {objects.map((o, i) => {
                const display = trimPrefix(o.key, path);
                const rowIdx = folders.length + i;
                return (
                  <tr key={o.key}
                      className={`border-t border-border hover:bg-panel ${cursor === rowIdx ? "bg-panel" : ""}`}>
                    <td className="py-2" data-label="">
                      <input
                        type="checkbox"
                        checked={picks.has(o.key)}
                        onChange={() => togglePick(o.key)}
                        onClick={(e) => e.stopPropagation()}
                        className="cursor-pointer"
                      />
                    </td>
                    <td className="py-2 pr-3 min-w-0" data-label="Name">
                      <button
                        onClick={() => open(o)}
                        className="flex items-center gap-2 font-mono hover:text-accent text-left w-full min-w-0"
                        title={o.key}
                      >
                        <Thumb bucket={name} objectKey={o.key} contentType={o.contentType} size={28} />
                        <span className="truncate">{display}</span>
                      </button>
                    </td>
                    <td className="py-2 text-muted whitespace-nowrap" data-label="Size">{formatBytes(o.size)}</td>
                    <td className="py-2 text-muted text-xs whitespace-nowrap" data-label="Modified">{formatDate(o.lastModified)}</td>
                    <td className="py-2 text-right space-x-2 actions" data-label="">
                      <button
                        onClick={() => download(o.key)}
                        className="text-muted hover:text-accent"
                        title="Download"
                      >
                        <Download size={14} />
                      </button>
                      <button
                        onClick={(e) => remove(o.key, e.shiftKey)}
                        className="text-danger hover:text-red-400"
                        title="Click to trash; Shift-click to permanently delete (skip trash)"
                      >
                        <Trash2 size={14} />
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          </div>
        )}
      </Card>
    </div>
  );
}

// ---------- helpers ----------

function normalizePath(p: string): string {
  // Trim leading "/" (keys never start with /); ensure trailing "/" so we
  // can compare prefixes cheaply. "" = root.
  p = p.replace(/^\/+/, "");
  if (p && !p.endsWith("/")) p += "/";
  return p;
}

function parentOf(p: string): string {
  if (!p) return "";
  const trimmed = p.replace(/\/$/, "");
  const i = trimmed.lastIndexOf("/");
  if (i < 0) return "";
  return trimmed.slice(0, i + 1);
}

function trimPrefix(s: string, prefix: string): string {
  return s.startsWith(prefix) ? s.slice(prefix.length) : s;
}

// NewFolderButton creates a {name}/.keep marker so the folder shows up in the
// browser. The marker is hidden from the file list (basename starts with ".").
function NewFolderButton({ bucket, path, onCreated }: { bucket: string; path: string; onCreated: () => void }) {
  const toast = useToast();
  const create = async () => {
    const raw = prompt("New folder name:");
    if (!raw) return;
    const clean = raw.replace(/^\/+|\/+$/g, "");
    if (!clean) return;
    const key = path + clean + "/.keep";
    try {
      const token = getToken();
      const res = await fetch(`${API}/${bucket}/${encodeKey(key)}`, {
        method: "PUT",
        headers: {
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
          "Content-Type": "application/octet-stream",
          "Content-Length": "0",
        },
        body: "",
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      toast.push("success", `Folder "${clean}" created.`);
      onCreated();
    } catch (e) {
      toast.push("error", e instanceof Error ? e.message : "failed");
    }
  };
  return (
    <button
      onClick={create}
      className="text-xs text-accent hover:underline inline-flex items-center gap-1"
      title="Create an empty folder (writes a hidden .keep marker)"
    >
      <Folder size={12} /> New folder
    </button>
  );
}

function Breadcrumb({ path, onNavigate }: { path: string; onNavigate: (p: string) => void }) {
  const parts = path ? path.replace(/\/$/, "").split("/") : [];
  return (
    <nav className="text-sm flex items-center gap-1 min-w-0 flex-wrap">
      <button onClick={() => onNavigate("")}
              className="text-muted hover:text-accent inline-flex items-center gap-1">
        <Home size={12} /> root
      </button>
      {parts.map((segment, i) => {
        const target = parts.slice(0, i + 1).join("/") + "/";
        const isLast = i === parts.length - 1;
        return (
          <span key={target} className="inline-flex items-center gap-1 min-w-0">
            <ChevronRight size={12} className="text-muted shrink-0" />
            <button onClick={() => onNavigate(target)}
                    className={isLast ? "font-mono truncate" : "font-mono text-muted hover:text-accent truncate"}
                    title={segment}>
              {segment}
            </button>
          </span>
        );
      })}
    </nav>
  );
}

function FileIcon({ kind }: { kind: "video" | "audio" | "image" | "other" }) {
  const cls = "text-muted";
  if (kind === "video") return <FileVideo size={14} className={cls} />;
  if (kind === "audio") return <FileAudio size={14} className={cls} />;
  if (kind === "image") return <FileImage size={14} className={cls} />;
  return <FileText size={14} className={cls} />;
}

interface VideoQuality {
  height: number;
  video_bitrate_kbps: number;
  audio_bitrate_kbps: number;
  playlist: string;       // e.g. "720p/playlist.m3u8"
}

interface ObjectInfo {
  objectId: string;
  bucket: string;
  key: string;
  size: number;
  contentType: string;
  transcoded: {
    type?: string;
    master?: string;                 // video: master.m3u8
    qualities?: VideoQuality[];      // video: per-rendition details
    thumbnails?: string[];           // video: ["thumb_0.jpg", ...]
    hls?: string;                    // audio: HLS playlist
    mp3?: string;                    // audio: MP3
    ogg?: string;                    // audio: OGG
    webp?: string;                   // image
    avif?: string | null;            // image
    // Populated when transcode_status is skipped_quota or failed_quota.
    quota?: {
      estimatedBytes?: number;
      actualBytes?: number;
      availableBytes: number;
      quotaBytes: number;
      usedBytes: number;
      deficitBytes: number;
      durationSec?: number;
      rungs?: number;
      reason: string;
    };
    // Populated when transcode_status is skipped_duration_limit or
    // skipped_height_limit — the source exceeded the per-user transcode cap.
    transcodeLimit?: {
      reason: "skipped_duration_limit" | "skipped_height_limit";
      sourceDurationSec?: number;
      maxDurationSec?: number;
      sourceHeight?: number;
      maxHeight?: number;
      durationSec?: number;
    };
  } | null;
  transcodeStatus: string;
  pipeline?: PipelineJob[];
}

interface PipelineJob {
  fileType: string;
  status: "pending" | "processing" | "done" | "failed" | "skipped" | "waiting";
  progressPct: number;
  error?: string;
}

function PreviewCard({ bucket, obj, versioning, onClose, onReload }: {
  bucket: string;
  obj: ObjectDetail;
  versioning: boolean;
  onClose: () => void;
  onReload: () => void;
}) {
  const [info, setInfo] = useState<ObjectInfo | null>(null);
  const [imageUrl, setImageUrl] = useState<string | null>(null);
  const [actionPending, setActionPending] = useState(false);
  const [shareOpen, setShareOpen] = useState(false);
  const [versionsOpen, setVersionsOpen] = useState(false);
  const kind = classify(obj.key, obj.contentType);
  const toast = useToast();

  // Server-side POST ?transcode now atomically cancels any existing pipeline
  // before queuing the new one, so we don't need a DELETE first (which had
  // a race window where ffmpeg was still writing to the segment dir).
  const generateStreams = async () => {
    if (actionPending) return;
    setActionPending(true);
    const wasActive = info?.transcodeStatus === "pending"
                   || info?.transcodeStatus === "processing";
    try {
      await api(`/${bucket}/${encodeKey(obj.key)}?transcode`, { method: "POST" });
      const updated = await api<ObjectInfo>(`/${bucket}/${encodeKey(obj.key)}?info`);
      setInfo(updated);
      toast.push("success", wasActive
        ? "Transcode restarted — old pipeline cancelled, fresh jobs queued."
        : "Transcoding queued. Watch the Active operations panel for progress.");
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "failed to start transcode");
    } finally {
      setActionPending(false);
    }
  };

  const deleteStreams = async () => {
    if (!confirm("Delete the streamable versions? The original file stays.")) return;
    setActionPending(true);
    try {
      await api(`/${bucket}/${encodeKey(obj.key)}?transcode`, { method: "DELETE" });
      const updated = await api<ObjectInfo>(`/${bucket}/${encodeKey(obj.key)}?info`);
      setInfo(updated);
      toast.push("success", "Streams deleted. Original file is untouched.");
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "failed");
    } finally {
      setActionPending(false);
    }
  };

  useEffect(() => {
    setInfo(null);
    setImageUrl(null);

    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;

    // Poll object info until transcoding finishes (or it's not a media file).
    const fetchInfo = () => {
      api<ObjectInfo>(`/${bucket}/${encodeKey(obj.key)}?info`)
        .then((i) => {
          if (cancelled) return;
          setInfo(i);
          // Re-poll every 2s while transcoding is in progress
          if (i.transcodeStatus === "pending" || i.transcodeStatus === "processing") {
            timer = setTimeout(fetchInfo, 2000);
          }
        })
        .catch(() => { /* swallow — file may have been deleted */ });
    };
    fetchInfo();

    // Original-file image preview (works regardless of transcode status)
    if (kind === "image") {
      const token = getToken();
      fetch(`${API}/${bucket}/${encodeKey(obj.key)}`, {
        headers: token ? { Authorization: `Bearer ${token}` } : {},
      })
        .then((r) => r.blob())
        .then((b) => { if (!cancelled) setImageUrl(URL.createObjectURL(b)); });
    }

    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [bucket, obj.key, kind]);

  const streamRoot = info ? `${STREAM}/${info.objectId}` : null;
  const ready = info?.transcodeStatus === "done";
  const canTranscode = kind !== "other";
  const isProcessing = info?.transcodeStatus === "pending" || info?.transcodeStatus === "processing";

  return (
    <Card>
      <div className="flex items-start justify-between mb-3 gap-3 flex-wrap">
        <div className="min-w-0 break-all">
          <h2 className="text-sm font-semibold break-all">{obj.key}</h2>
          <p className="text-xs text-muted">
            {formatBytes(obj.size)} · {obj.contentType} · {kind}
            {info && (
              <span className="ml-2">
                · transcode:{" "}
                <span className={
                  info.transcodeStatus === "done" ? "text-success"
                  : info.transcodeStatus === "failed" ? "text-danger"
                  : info.transcodeStatus === "none" ? "text-muted"
                  : "text-warning"
                }>{info.transcodeStatus}</span>
              </span>
            )}
          </p>
        </div>
        <div className="flex items-center gap-3 flex-wrap">
          {/* Generate/Re-generate works in every state — pending stuck? click
              it to wipe and re-queue. Done? click for a fresh re-encode. */}
          {canTranscode && info && (
            <button
              onClick={generateStreams}
              disabled={actionPending}
              className="text-xs text-accent hover:underline disabled:opacity-50"
              title={
                info.transcodeStatus === "none"   ? "Run transcoding now"
                : info.transcodeStatus === "done" ? "Re-run transcoding (replaces existing)"
                : info.transcodeStatus === "skipped_quota" || info.transcodeStatus === "failed_quota"
                                                  ? "Free up space first, then click to retry"
                : "Cancel existing job and start fresh"
              }
            >
              {info.transcodeStatus === "none"                                            ? "Generate streams"
              : info.transcodeStatus === "failed"                                         ? "Retry transcoding"
              : info.transcodeStatus === "skipped_quota"                                  ? "Retry (need more space)"
              : info.transcodeStatus === "failed_quota"                                   ? "Retry (need more space)"
              : info.transcodeStatus === "done"                                           ? "Re-generate"
              : /* pending or processing */                                                  "Cancel & restart"}
            </button>
          )}
          {canTranscode && ready && (
            <button
              onClick={deleteStreams}
              disabled={actionPending}
              className="text-xs text-muted hover:text-danger disabled:opacity-50"
              title="Remove transcoded versions, keep original"
            >
              Delete streams
            </button>
          )}
          <button
            onClick={() => setShareOpen(true)}
            className="text-xs text-accent hover:underline inline-flex items-center gap-1"
            title="Create a time-limited public link"
          >
            <Share2 size={12} /> Share link
          </button>
          {versioning && (
            <button
              onClick={() => setVersionsOpen(true)}
              className="text-xs text-blue-300 hover:underline inline-flex items-center gap-1"
              title="View prior versions and restore"
            >
              <History size={12} /> Versions
            </button>
          )}
          <button onClick={onClose} className="text-muted hover:text-text text-xs">close</button>
        </div>
      </div>
      {shareOpen && (
        <ShareModal
          bucket={bucket}
          objectKey={obj.key}
          onClose={() => setShareOpen(false)}
        />
      )}
      {versionsOpen && (
        <VersionsModal
          bucket={bucket}
          objectKey={obj.key}
          onClose={() => setVersionsOpen(false)}
          onRestored={onReload}
        />
      )}

      {kind === "image" && ready && info?.transcoded?.webp && streamRoot && (
        <img src={`${streamRoot}/${info.transcoded.webp}`} alt={obj.key}
             className="max-h-96 mx-auto rounded" />
      )}
      {kind === "image" && !ready && imageUrl && (
        <img src={imageUrl} alt={obj.key} className="max-h-96 mx-auto rounded" />
      )}

      {kind === "video" && ready && streamRoot && info?.transcoded?.master && (
        <div className="space-y-3">
          <VideoPlayer src={`${streamRoot}/${info.transcoded.master}`} />

          {/* Thumbnail strip — quick preview at 0/25/50/75% of duration */}
          {info.transcoded.thumbnails && info.transcoded.thumbnails.length > 0 && (
            <div className="flex gap-2 overflow-x-auto">
              {info.transcoded.thumbnails.map((t) => (
                <a
                  key={t}
                  href={`${streamRoot}/${t}`}
                  target="_blank"
                  rel="noreferrer"
                  className="shrink-0"
                  title={`Open ${t} in new tab`}
                >
                  <img
                    src={`${streamRoot}/${t}`}
                    alt={t}
                    className="h-16 rounded border border-border hover:border-accent transition"
                  />
                </a>
              ))}
            </div>
          )}

          {/* Stream URLs panel — copy-paste anywhere, no auth required */}
          <details className="text-xs" open>
            <summary className="text-muted cursor-pointer hover:text-text select-none">
              Stream URLs (no auth — paste anywhere)
            </summary>
            <div className="mt-2 space-y-2 bg-bg p-3 rounded">
              <UrlRow label="HLS master (adaptive)"
                     url={`${streamRoot}/${info.transcoded.master}`}
                     hint="The one URL most players want. video.js, hls.js, Safari, VLC, ffplay all handle it." />

              {info.transcoded.qualities && info.transcoded.qualities.length > 0 && (
                <details className="text-[11px] pl-2">
                  <summary className="text-muted cursor-pointer hover:text-text">
                    Direct per-quality playlists ({info.transcoded.qualities.length})
                  </summary>
                  <div className="mt-1 space-y-1">
                    {info.transcoded.qualities.map((q) => (
                      <UrlRow
                        key={q.height}
                        label={`${q.height}p`}
                        url={`${streamRoot}/${q.playlist}`}
                        hint={`${q.video_bitrate_kbps + q.audio_bitrate_kbps} kbps total`}
                        compact
                      />
                    ))}
                  </div>
                </details>
              )}
            </div>
          </details>
        </div>
      )}
      {kind === "video" && !ready && (
        <PipelineProgress info={info} />
      )}

      {kind === "audio" && ready && streamRoot && (
        <div className="space-y-3">
          {/* MP3 source — universal browser compat (Chrome/Firefox/Edge/Safari).
              <source> fallbacks: OGG for open browsers, HLS for Safari. */}
          <audio controls className="w-full">
            {info?.transcoded?.mp3 && (
              <source src={`${streamRoot}/${info.transcoded.mp3}`} type="audio/mpeg" />
            )}
            {info?.transcoded?.ogg && (
              <source src={`${streamRoot}/${info.transcoded.ogg}`} type="audio/ogg" />
            )}
            {info?.transcoded?.hls && (
              <source src={`${streamRoot}/${info.transcoded.hls}`} type="application/vnd.apple.mpegurl" />
            )}
          </audio>

          {/* Share/copy URLs */}
          <details className="text-xs" open>
            <summary className="text-muted cursor-pointer hover:text-text select-none">
              Stream URLs (no auth — paste anywhere)
            </summary>
            <div className="mt-2 space-y-2 bg-bg p-3 rounded">
              {info?.transcoded?.mp3 && (
                <UrlRow label="MP3 (universal)" url={`${streamRoot}/${info.transcoded.mp3}`}
                       hint="Works in every browser, every media player, every device." />
              )}
              {info?.transcoded?.ogg && (
                <UrlRow label="OGG (open)" url={`${streamRoot}/${info.transcoded.ogg}`}
                       hint="Free codec, plays in Firefox/Chrome natively." />
              )}
              {info?.transcoded?.hls && (
                <UrlRow label="HLS (adaptive)" url={`${streamRoot}/${info.transcoded.hls}`}
                       hint="Streams in chunks, native on Safari/iOS." />
              )}
            </div>
          </details>
        </div>
      )}
      {kind === "audio" && !ready && (
        <PipelineProgress info={info} />
      )}

      {kind === "other" && (
        <p className="text-sm text-muted">No preview available. Use Download to get the file.</p>
      )}
    </Card>
  );
}

/**
 * One row in the "Stream URLs" panel — shows the URL, a copy button, and a
 * "open in new tab" link. Compact variant tightens spacing for the per-quality list.
 */
function UrlRow({ label, url, hint, compact }: {
  label: string;
  url: string;
  hint?: string;
  compact?: boolean;
}) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard.writeText(url);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };
  return (
    <div className={compact ? "" : "space-y-0.5"}>
      <div className="flex items-center gap-2">
        <span className={`shrink-0 ${compact ? "text-[10px] font-semibold w-12" : "text-[11px] font-semibold w-32"}`}>
          {label}
        </span>
        <code className="flex-1 font-mono text-[11px] text-accent truncate" title={url}>
          {url}
        </code>
        <button onClick={copy} className="shrink-0 text-muted hover:text-text"
                title="Copy URL">
          {copied ? <Check size={12} className="text-success" /> : <Copy size={12} />}
        </button>
        <a href={url} target="_blank" rel="noreferrer" className="shrink-0 text-muted hover:text-text"
           title="Open in new tab">
          <ExternalLink size={12} />
        </a>
      </div>
      {hint && !compact && <p className="text-[10px] text-muted pl-32">{hint}</p>}
    </div>
  );
}

function PipelineProgress({ info }: { info: ObjectInfo | null }) {
  if (!info) return <div className="bg-bg p-4 rounded text-sm text-muted text-center">Loading...</div>;
  if (info.transcodeStatus === "failed") {
    return <div className="bg-bg p-4 rounded text-sm text-danger text-center">
      Transcoding failed — only original download is available.
    </div>;
  }
  if (info.transcodeStatus === "skipped_duration_limit" || info.transcodeStatus === "skipped_height_limit") {
    const t = info.transcoded?.transcodeLimit;
    const isDuration = info.transcodeStatus === "skipped_duration_limit";
    const headline = isDuration
      ? "Transcoding skipped — video is over the duration limit."
      : "Transcoding skipped — source is taller than your allowed quality.";
    const body = isDuration
      ? "Long videos take a lot of CPU to transcode. The original file is fully usable — you can download it, you just can't stream it as HLS without admin approval."
      : "Your account is currently capped at a max output resolution. The original file is fully usable; only the streaming variants weren't generated.";
    // Pre-filled mailto so users have a real path to ask without a new
    // API endpoint. Phase-2 will replace this with a self-serve form.
    const subject = encodeURIComponent("Request: transcoding access for a larger video");
    const bodyText = encodeURIComponent(
      `Hi,\n\nI'd like to request transcoding access for a video that exceeded the current limit.\n\n` +
      `Reason: ${info.transcodeStatus}\n` +
      (t?.sourceDurationSec ? `Source duration: ${Math.round(t.sourceDurationSec / 60)} min\n` : "") +
      (t?.maxDurationSec ? `Current duration cap: ${Math.round(t.maxDurationSec / 60)} min\n` : "") +
      (t?.sourceHeight ? `Source height: ${t.sourceHeight}p\n` : "") +
      (t?.maxHeight ? `Current height cap: ${t.maxHeight}p\n` : "") +
      `\nFile: (please attach details)\n\nThanks!`
    );
    return <div className="bg-bg p-4 rounded text-sm text-warning text-center space-y-3">
      <div className="font-semibold">{headline}</div>
      <div className="text-xs text-muted">{body}</div>
      {t && (
        <div className="text-[11px] text-muted bg-card border border-border rounded p-2 grid grid-cols-2 gap-x-4 gap-y-0.5 max-w-md mx-auto text-left">
          {isDuration ? (
            <>
              <span>Your video</span>
              <span className="text-text text-right font-mono">
                {t.sourceDurationSec ? `${Math.round(t.sourceDurationSec / 60)} min` : "?"}
              </span>
              <span>Current limit</span>
              <span className="text-danger text-right font-mono">
                {t.maxDurationSec ? `${Math.round(t.maxDurationSec / 60)} min` : "?"}
              </span>
            </>
          ) : (
            <>
              <span>Your video</span>
              <span className="text-text text-right font-mono">
                {t.sourceHeight ? `${t.sourceHeight}p` : "?"}
              </span>
              <span>Max allowed quality</span>
              <span className="text-danger text-right font-mono">
                {t.maxHeight ? `${t.maxHeight}p` : "?"}
              </span>
            </>
          )}
        </div>
      )}
      <a
        href={`mailto:support@personals3.tech?subject=${subject}&body=${bodyText}`}
        className="inline-block text-xs px-3 py-1.5 rounded-md bg-accent text-bg font-semibold hover:opacity-90"
      >
        Request transcoding access from admin
      </a>
    </div>;
  }

  if (info.transcodeStatus === "skipped_quota" || info.transcodeStatus === "failed_quota") {
    const q = info.transcoded?.quota;
    const skipped = info.transcodeStatus === "skipped_quota";
    const headline = skipped
      ? "Transcoding skipped — not enough quota."
      : "Transcoding stopped — over quota at publish.";
    const body = skipped
      ? "The estimated HLS output would exceed your storage cap, so no transcode was started."
      : "Output was larger than the pre-flight estimate, so the segments were discarded.";
    return <div className="bg-bg p-4 rounded text-sm text-warning text-center space-y-2">
      <div className="font-semibold">{headline}</div>
      <div className="text-xs text-muted">{body} The original file is fully usable.</div>
      {q && (
        <div className="text-[11px] text-muted bg-card border border-border rounded p-2 grid grid-cols-2 gap-x-4 gap-y-0.5 max-w-md mx-auto text-left">
          <span>{skipped ? "Estimated output" : "Actual output"}</span>
          <span className="text-text text-right font-mono">
            {formatBytes(skipped ? (q.estimatedBytes ?? 0) : (q.actualBytes ?? 0))}
          </span>
          <span>Available to you</span>
          <span className="text-text text-right font-mono">{formatBytes(q.availableBytes)}</span>
          <span>Free up at least</span>
          <span className="text-danger text-right font-mono">{formatBytes(q.deficitBytes)}</span>
          <span>Your quota</span>
          <span className="text-text text-right font-mono">
            {formatBytes(q.usedBytes)} / {formatBytes(q.quotaBytes)}
          </span>
        </div>
      )}
    </div>;
  }
  const pipeline = info.pipeline || [];
  if (pipeline.length === 0) {
    return <div className="bg-bg p-4 rounded text-sm text-muted text-center">
      Queued — waiting for a worker...
    </div>;
  }

  // Aggregate progress across all jobs
  const finished = pipeline.filter((p) => p.status === "done" || p.status === "skipped").length;
  const totalProgress = pipeline.reduce((sum, p) => {
    if (p.status === "done" || p.status === "skipped") return sum + 100;
    if (p.status === "processing") return sum + p.progressPct;
    return sum;
  }, 0);
  const avgPct = pipeline.length > 0 ? totalProgress / pipeline.length : 0;

  return (
    <div className="bg-bg border border-border rounded p-4 space-y-3">
      <div>
        <div className="flex items-center justify-between text-xs mb-1">
          <span className="font-semibold">Transcoding pipeline</span>
          <span className="text-muted">{finished}/{pipeline.length} stages complete</span>
        </div>
        <div className="h-1.5 bg-border rounded overflow-hidden">
          <div className="h-full bg-accent transition-all duration-500"
               style={{ width: `${avgPct}%` }} />
        </div>
      </div>

      <div className="space-y-2">
        {pipeline.map((p, i) => <PipelineRow key={i} job={p} />)}
      </div>
    </div>
  );
}

function PipelineRow({ job }: { job: PipelineJob }) {
  const label = prettyFileType(job.fileType);
  const color =
    job.status === "done"       ? "bg-success" :
    job.status === "processing" ? "bg-accent"  :
    job.status === "failed"     ? "bg-danger"  :
    job.status === "skipped"    ? "bg-muted"   :
                                  "bg-border";   // pending / waiting
  const pct =
    job.status === "done"     ? 100 :
    job.status === "skipped"  ? 100 :
    job.status === "failed"   ? 100 :
    job.status === "processing" ? job.progressPct :
                                  0;
  const statusText =
    job.status === "done"       ? "✓ done"      :
    job.status === "skipped"    ? "—  skipped (source too small)" :
    job.status === "failed"     ? "✗ failed"    :
    job.status === "processing" ? `${job.progressPct}%`           :
    job.status === "waiting"    ? "waiting"     :
                                  "queued";

  return (
    <div>
      <div className="flex items-center justify-between text-[11px] mb-0.5">
        <span className="font-mono">{label}</span>
        <span className="text-muted whitespace-nowrap">{statusText}</span>
      </div>
      <div className="h-1 bg-border rounded overflow-hidden">
        <div className={`h-full transition-all duration-500 ${color}`}
             style={{ width: `${pct}%` }} />
      </div>
      {job.error && <p className="text-[10px] text-danger mt-0.5 truncate" title={job.error}>{job.error}</p>}
    </div>
  );
}

function prettyFileType(ft: string): string {
  const m = /^video_quality_(\d+p)$/.exec(ft);
  if (m) return `${m[1]} video`;
  if (ft === "video_thumbnails") return "thumbnails";
  if (ft === "video_finalize")   return "finalize";
  if (ft === "video")            return "video (legacy single job)";
  return ft;
}

function encodeKey(k: string): string {
  return k.split("/").map(encodeURIComponent).join("/");
}

// Banner shown on the bucket page when isPublic=true. Surfaces the URL
// pattern + a copy button so the user immediately knows what changed.
function PublicBanner({ bucket }: { bucket: string }) {
  const [copied, setCopied] = useState(false);
  const url = typeof window !== "undefined"
    ? `${window.location.origin}/public/${bucket}/`
    : `/public/${bucket}/`;

  const copy = () => {
    navigator.clipboard.writeText(url);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  return (
    <div className="bg-amber-500/10 border border-amber-500/40 rounded p-3 flex items-start gap-3">
      <Globe size={16} className="text-amber-400 shrink-0 mt-0.5" />
      <div className="flex-1 min-w-0">
        <p className="text-xs font-semibold text-amber-200">This bucket is public.</p>
        <p className="text-[11px] text-amber-300/80 mt-0.5">
          Anyone with a URL below can read files. No login needed. Root URL
          serves <span className="font-mono">index.html</span> if present, otherwise a JSON listing.
        </p>
        <div className="mt-2 flex items-center gap-2 bg-bg/60 border border-amber-500/20 rounded px-2 py-1">
          <code className="flex-1 font-mono text-[11px] text-accent truncate" title={url}>
            {url}
          </code>
          <button onClick={copy} className="text-amber-300 hover:text-amber-200 shrink-0" title="Copy">
            {copied ? <Check size={12} className="text-success" /> : <Copy size={12} />}
          </button>
          <a href={url} target="_blank" rel="noreferrer"
             className="text-amber-300 hover:text-amber-200 shrink-0" title="Open in new tab">
            <ExternalLink size={12} />
          </a>
        </div>
      </div>
    </div>
  );
}
