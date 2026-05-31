"use client";

import { useEffect, useState } from "react";
import { api, API, getToken, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { X, RotateCcw, Trash2, Download, AlertTriangle } from "lucide-react";
import { formatBytes, formatDate } from "@/lib/format";
import { useToast } from "@/components/toast";

interface VersionDTO {
  versionId: string;
  size: number;
  etag?: string;
  contentType?: string;
  createdAt: string;
  isCurrent: boolean;
  isDeleteMarker: boolean;
}

interface Props {
  bucket: string;
  objectKey: string;
  onClose: () => void;
  onRestored?: () => void;
}

/**
 * Lists prior versions of one object. Lets the caller download/restore/purge.
 *
 * - "Restore" copies an old version back into the current slot. The
 *   currently-live bytes (if any) are first snapshotted, so restore is
 *   itself reversible.
 * - "Delete" permanently purges that single version (file + DB row).
 *   Refunds quota.
 * - "Download" streams the version's bytes.
 */
export function VersionsModal({ bucket, objectKey, onClose, onRestored }: Props) {
  const toast = useToast();
  const [versions, setVersions] = useState<VersionDTO[]>([]);
  const [deleted, setDeleted] = useState(false);
  const [busy, setBusy] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const load = async () => {
    setBusy(true);
    setErr(null);
    try {
      const r = await api<{ versions: VersionDTO[]; deleted: boolean }>(
        `/${bucket}/${encodeKey(objectKey)}?versions`,
      );
      setVersions(r.versions);
      setDeleted(r.deleted);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "failed to load");
    } finally {
      setBusy(false);
    }
  };

  useEffect(() => { void load(); }, [bucket, objectKey]);

  const restore = async (v: VersionDTO) => {
    if (v.isCurrent) return;
    if (!confirm(
      `Restore the version from ${formatDate(v.createdAt)}?\n\n` +
      `The currently-live content will be saved as a new version, so you ` +
      `can roll back to it later.`,
    )) return;
    try {
      await api(`/${bucket}/${encodeKey(objectKey)}?restore&versionId=${v.versionId}`,
        { method: "POST" });
      toast.push("success", "Restored — preview will refresh.");
      await load();
      onRestored?.();
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "restore failed");
    }
  };

  const purge = async (v: VersionDTO) => {
    if (v.isCurrent) return;
    if (!confirm(
      `Permanently delete this version from ${formatDate(v.createdAt)}?\n\n` +
      `The bytes can't be recovered.`,
    )) return;
    try {
      await api(`/${bucket}/${encodeKey(objectKey)}?versionId=${v.versionId}`,
        { method: "DELETE" });
      await load();
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "delete failed");
    }
  };

  const download = (v: VersionDTO) => {
    const token = getToken();
    fetch(`${API}/${bucket}/${encodeKey(objectKey)}?versionId=${v.versionId}`, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    })
      .then((r) => r.blob())
      .then((blob) => {
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = `${objectKey.split("/").pop() || "download"}.${v.versionId.slice(0, 8)}`;
        a.click();
        URL.revokeObjectURL(url);
      });
  };

  return (
    <div className="fixed inset-0 z-50 bg-black/60 flex items-center justify-center p-4"
         onClick={onClose}>
      <div className="bg-panel border border-border rounded-lg max-w-2xl w-full max-h-[80vh] overflow-hidden flex flex-col"
           onClick={(e) => e.stopPropagation()}>
        <div className="p-4 border-b border-border flex items-center justify-between">
          <div>
            <h2 className="text-sm font-semibold">Version history</h2>
            <p className="text-xs text-muted font-mono truncate">{bucket}/{objectKey}</p>
          </div>
          <button onClick={onClose} className="text-muted hover:text-text"><X size={16} /></button>
        </div>

        {deleted && (
          <div className="mx-4 mt-3 p-2 bg-amber-500/10 border border-amber-500/40 rounded flex items-center gap-2 text-xs text-amber-200">
            <AlertTriangle size={14} className="shrink-0" />
            This object is currently deleted. Restore a non-marker version to bring it back.
          </div>
        )}

        <div className="p-4 overflow-y-auto flex-1">
          {busy ? (
            <p className="text-sm text-muted">Loading...</p>
          ) : err ? (
            <p className="text-sm text-danger">{err}</p>
          ) : versions.length === 0 ? (
            <p className="text-sm text-muted">No version history yet — this object hasn't been overwritten or deleted.</p>
          ) : (
            <ul className="space-y-2">
              {versions.map((v) => (
                <li key={v.versionId}
                    className={`p-3 rounded border ${
                      v.isCurrent ? "border-accent bg-blue-950/20"
                      : v.isDeleteMarker ? "border-red-500/40 bg-red-950/10"
                      : "border-border bg-bg"
                    }`}>
                  <div className="flex items-center justify-between gap-2">
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2 text-xs">
                        {v.isCurrent && <span className="px-1.5 py-0.5 bg-accent text-white rounded text-[10px]">CURRENT</span>}
                        {v.isDeleteMarker && <span className="px-1.5 py-0.5 bg-red-500 text-white rounded text-[10px]">DELETE</span>}
                        <span className="text-muted">{formatDate(v.createdAt)}</span>
                        {!v.isDeleteMarker && (
                          <span className="text-muted">· {formatBytes(v.size)}</span>
                        )}
                      </div>
                      <code className="text-[10px] text-muted font-mono">
                        id: {v.versionId.slice(0, 16)}{v.versionId.length > 16 && "..."}
                      </code>
                    </div>
                    {!v.isCurrent && !v.isDeleteMarker && (
                      <div className="flex items-center gap-1">
                        <button onClick={() => download(v)}
                                className="p-1.5 text-muted hover:text-text"
                                title="Download this version">
                          <Download size={14} />
                        </button>
                        <button onClick={() => restore(v)}
                                className="p-1.5 text-blue-300 hover:text-blue-200"
                                title="Restore as current">
                          <RotateCcw size={14} />
                        </button>
                        <button onClick={() => purge(v)}
                                className="p-1.5 text-danger hover:text-red-400"
                                title="Permanently delete">
                          <Trash2 size={14} />
                        </button>
                      </div>
                    )}
                    {v.isDeleteMarker && (
                      <button onClick={() => purge(v)}
                              className="p-1.5 text-muted hover:text-text"
                              title="Remove this delete marker">
                        <Trash2 size={14} />
                      </button>
                    )}
                  </div>
                </li>
              ))}
            </ul>
          )}
        </div>

        <div className="p-3 border-t border-border flex justify-end">
          <Button variant="ghost" onClick={onClose}>Close</Button>
        </div>
      </div>
    </div>
  );
}

function encodeKey(k: string): string {
  return k.split("/").map(encodeURIComponent).join("/");
}
