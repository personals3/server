"use client";

import { useCallback, useEffect, useState, FormEvent } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { api, ApiError } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { PageHeader } from "@/components/ui/page-header";
import { ListSkeleton } from "@/components/ui/skeleton";
import {
  Search as SearchIcon, RefreshCw, Database, FileVideo, FileAudio,
  FileImage, FileText, ChevronLeft, ChevronRight, X,
} from "lucide-react";
import { formatBytes, formatDate, classify } from "@/lib/format";

interface SearchHit {
  bucket: string;
  key: string;
  size: number;
  contentType: string;
  etag: string;
  lastModified: string;
  transcodeStatus: string;
}

interface SearchResp {
  count: number;
  total: number;
  limit: number;
  offset: number;
  results: SearchHit[];
}

const PAGE_SIZE = 50;

export default function SearchPage() {
  const router = useRouter();
  const sp = useSearchParams();

  // URL-backed state so refresh + share-link work the same
  const [q,         setQ]         = useState(sp.get("q") ?? "");
  const [bucket,    setBucket]    = useState(sp.get("bucket") ?? "");
  const [type,      setType]      = useState(sp.get("type") ?? "");
  const [ext,       setExt]       = useState(sp.get("ext") ?? "");
  const [minSize,   setMinSize]   = useState(sp.get("minSize") ?? "");
  const [maxSize,   setMaxSize]   = useState(sp.get("maxSize") ?? "");
  const [sort,      setSort]      = useState<"modified" | "size" | "key">(
    (sp.get("sort") as any) ?? "modified",
  );
  const [dir,       setDir]       = useState<"asc" | "desc">(
    (sp.get("dir") as any) ?? "desc",
  );
  const [offset,    setOffset]    = useState(Number(sp.get("offset") ?? 0));

  const [data, setData]   = useState<SearchResp | null>(null);
  const [busy, setBusy]   = useState(false);
  const [err,  setErr]    = useState<string | null>(null);

  // Encode current filters into the URL so navigation back/forth works.
  const syncURL = (next: Partial<Record<string, string>> = {}) => {
    const params = new URLSearchParams();
    const base = { q, bucket, type, ext, minSize, maxSize,
                   sort, dir, offset: offset.toString(), ...next };
    Object.entries(base).forEach(([k, v]) => {
      if (v) params.set(k, v.toString());
    });
    router.replace(`/dashboard/search?${params.toString()}`);
  };

  const run = useCallback(async (off = offset) => {
    setBusy(true);
    setErr(null);
    try {
      const params = new URLSearchParams();
      if (q)         params.set("q", q);
      if (bucket)    params.set("bucket", bucket);
      if (type)      params.set("type", type);
      if (ext)       params.set("ext", ext);
      if (minSize)   params.set("minSize", minSize);
      if (maxSize)   params.set("maxSize", maxSize);
      params.set("sort",   sort);
      params.set("dir",    dir);
      params.set("limit",  PAGE_SIZE.toString());
      params.set("offset", off.toString());
      const r = await api<SearchResp>(`/search?${params.toString()}`);
      setData(r);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "search failed");
    } finally {
      setBusy(false);
    }
  }, [q, bucket, type, ext, minSize, maxSize, sort, dir, offset]);

  // Re-run when the form is submitted; URL stays in sync.
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setOffset(0);
    syncURL({ offset: "0" });
    void run(0);
  };

  // Auto-run on first mount if URL has any filter set.
  useEffect(() => {
    const hasAnyFilter = sp.toString().length > 0;
    if (hasAnyFilter) void run(offset);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const clearAll = () => {
    setQ(""); setBucket(""); setType(""); setExt("");
    setMinSize(""); setMaxSize(""); setOffset(0);
    setData(null);
    router.replace("/dashboard/search");
  };

  const page = data ? Math.floor(data.offset / data.limit) + 1 : 1;
  const totalPages = data ? Math.max(1, Math.ceil(data.total / data.limit)) : 1;
  const goPage = (newOffset: number) => {
    setOffset(newOffset);
    syncURL({ offset: newOffset.toString() });
    void run(newOffset);
  };

  return (
    <div>
      <PageHeader
        title="Search"
        description="Find files across all your buckets by name, type, or size. Trash and versioned copies are excluded."
      />

      <div className="space-y-6">
      <Card>
        <form onSubmit={submit} className="space-y-3">
          <Input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="key contains... (e.g. vacation/2024)"
            autoFocus
          />

          <div className="grid grid-cols-2 sm:grid-cols-4 gap-3 text-xs">
            <Field label="Bucket"
                   value={bucket} onChange={setBucket} placeholder="any" />
            <Field label="Content type prefix"
                   value={type} onChange={setType} placeholder="e.g. image" />
            <Field label="Extension"
                   value={ext} onChange={setExt} placeholder="e.g. jpg" />
            <div>
              <label className="text-[10px] uppercase text-muted">Sort by</label>
              <select
                value={sort}
                onChange={(e) => setSort(e.target.value as any)}
                className="w-full bg-bg border border-border rounded px-2 py-1 mt-1"
              >
                <option value="modified">Modified</option>
                <option value="size">Size</option>
                <option value="key">Key (alphabetical)</option>
              </select>
            </div>
          </div>

          <div className="grid grid-cols-2 sm:grid-cols-4 gap-3 text-xs">
            <Field label="Min size (bytes)"
                   value={minSize} onChange={setMinSize}
                   placeholder="0" />
            <Field label="Max size (bytes)"
                   value={maxSize} onChange={setMaxSize}
                   placeholder="∞" />
            <div>
              <label className="text-[10px] uppercase text-muted">Direction</label>
              <select
                value={dir}
                onChange={(e) => setDir(e.target.value as any)}
                className="w-full bg-bg border border-border rounded px-2 py-1 mt-1"
              >
                <option value="desc">Descending</option>
                <option value="asc">Ascending</option>
              </select>
            </div>
            <div className="flex items-end gap-2">
              <Button type="submit" disabled={busy} className="flex-1">
                {busy ? "Searching..." : "Search"}
              </Button>
              {data && (
                <Button type="button" variant="ghost" onClick={clearAll}
                        title="Clear all filters">
                  <X size={14} />
                </Button>
              )}
            </div>
          </div>
        </form>
      </Card>

      {err && (
        <div className="text-sm text-danger bg-danger/5 border border-danger/20 rounded-md px-3 py-2">{err}</div>
      )}

      {data && (
        <Card>
          <div className="flex flex-wrap items-center justify-between gap-2 mb-3">
            <h2 className="text-sm font-semibold">
              {data.total === 0
                ? "No matches"
                : `${data.count} of ${data.total.toLocaleString()} matches`}
            </h2>
            {data.total > data.limit && (
              <div className="flex items-center gap-2 text-xs text-muted">
                <button
                  onClick={() => goPage(Math.max(0, offset - PAGE_SIZE))}
                  disabled={offset === 0}
                  className="p-1 hover:text-text disabled:opacity-30">
                  <ChevronLeft size={14} />
                </button>
                <span>Page {page} / {totalPages}</span>
                <button
                  onClick={() => goPage(offset + PAGE_SIZE)}
                  disabled={offset + PAGE_SIZE >= data.total}
                  className="p-1 hover:text-text disabled:opacity-30">
                  <ChevronRight size={14} />
                </button>
                <button
                  onClick={() => void run(offset)}
                  className="p-1 hover:text-text"
                  title="Refresh">
                  <RefreshCw size={12} />
                </button>
              </div>
            )}
          </div>

          {data.results.length === 0 ? (
            <p className="text-sm text-muted">Nothing to show on this page.</p>
          ) : (
            <div className="overflow-x-auto -mx-4 sm:mx-0">
              <table className="stack-rows w-full text-sm min-w-[500px] sm:min-w-0">
                <thead className="text-xs text-muted uppercase">
                  <tr>
                    <th className="text-left pb-2">Object</th>
                    <th className="text-right pb-2">Size</th>
                    <th className="text-right pb-2">Modified</th>
                  </tr>
                </thead>
                <tbody>
                  {data.results.map((hit) => (
                    <tr key={`${hit.bucket}/${hit.key}`}
                        className="border-t border-border-subtle hover:bg-surface transition-colors">
                      <td data-label="Object" className="py-2">
                        <Link
                          href={`/dashboard/buckets/${encodeURIComponent(hit.bucket)}?path=${encodeURIComponent(parentOf(hit.key))}`}
                          className="flex items-center gap-2 min-w-0 hover:text-accent"
                          title={hit.key}
                        >
                          <FileIcon kind={classify(hit.key, hit.contentType)} />
                          <span className="text-muted text-xs whitespace-nowrap">{hit.bucket}/</span>
                          <span className="font-mono truncate">{hit.key}</span>
                        </Link>
                      </td>
                      <td data-label="Size" className="py-2 text-muted text-right whitespace-nowrap">
                        {formatBytes(hit.size)}
                      </td>
                      <td data-label="Modified" className="py-2 text-muted text-right text-xs whitespace-nowrap">
                        {formatDate(hit.lastModified)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      )}

      {busy && !data && (
        <Card>
          <ListSkeleton rows={6} />
        </Card>
      )}

      {!data && !busy && !err && (
        <Card>
          <p className="text-sm text-muted">
            Type a substring of an object key (or just hit Search to list
            everything you own). Filters narrow the result without restricting
            buckets — leave Bucket empty to search across all of yours.
          </p>
        </Card>
      )}
      </div>
    </div>
  );
}

function Field({ label, value, onChange, placeholder }: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
}) {
  return (
    <div>
      <label className="text-[10px] uppercase text-muted">{label}</label>
      <input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        className="w-full bg-bg border border-border rounded px-2 py-1 mt-1 font-mono"
      />
    </div>
  );
}

function FileIcon({ kind }: { kind: "video" | "audio" | "image" | "other" }) {
  const cls = "text-muted shrink-0";
  if (kind === "video") return <FileVideo size={14} className={cls} />;
  if (kind === "audio") return <FileAudio size={14} className={cls} />;
  if (kind === "image") return <FileImage size={14} className={cls} />;
  return <FileText size={14} className={cls} />;
}

function parentOf(key: string): string {
  const i = key.lastIndexOf("/");
  return i < 0 ? "" : key.slice(0, i + 1);
}
