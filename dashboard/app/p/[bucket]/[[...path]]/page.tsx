"use client";

// Public file-explorer for buckets where is_public=true.
//
// Routes:
//   /p/{bucket}/                       → root listing
//   /p/{bucket}/folder/subfolder/      → drill into a folder (trailing slash)
//   /p/{bucket}/folder/file.jpg        → file (no trailing slash) → redirect
//                                        to /public/{bucket}/{key} so nginx
//                                        serves the bytes raw
//
// Data source: the API's public listing endpoint already returns JSON when
// no index.html sits at the prefix root:
//   GET /public/{bucket}/?prefix=folder/sub/
// It returns a FLAT list of all objects under the prefix (no delimiter),
// so we synthesize sub-folders client-side by splitting on the next "/".

import { useEffect, useMemo, useState } from "react";
import { useParams, usePathname, useRouter } from "next/navigation";
import Link from "next/link";
import {
  Folder, FileText, FileImage, FileVideo, FileAudio, ChevronRight,
  Download, FolderOpen, Inbox, Lock,
} from "lucide-react";
import { formatBytes, formatDate, classify } from "@/lib/format";

// ----- Where the public endpoint lives ----------------------------------
// In prod (nginx single-origin), /public/... is on the same origin as the
// dashboard, so a plain relative path is correct. In dev the dashboard at
// :3001 needs to hit the API directly at NEXT_PUBLIC_API_URL (e.g. :8080).
// We can't reuse the lib/api helper because that prepends /api which the
// public route does NOT live under.
function publicBase(): string {
  const env = process.env.NEXT_PUBLIC_API_URL;
  if (env && /^https?:\/\//i.test(env)) {
    try {
      const u = new URL(env);
      return `${u.protocol}//${u.host}`;
    } catch { /* fall through */ }
  }
  return ""; // same-origin
}

interface PublicObject {
  key: string;
  size: number;
  etag: string;
  contentType: string;
  lastModified: string;
}
interface PublicListResp {
  bucket: string;
  prefix: string;
  count: number;
  objects: PublicObject[];
  public: true;
}

function encodeKey(k: string): string {
  return k.split("/").map(encodeURIComponent).join("/");
}

export default function PublicExplorerPage() {
  const router    = useRouter();
  const params    = useParams<{ bucket: string; path?: string[] }>();
  const pathname  = usePathname();
  const bucket    = decodeURIComponent(params.bucket);
  const segments  = (params.path ?? []).map(decodeURIComponent);

  // Trailing-slash semantics get tricky because Next.js's default routing
  // strips trailing slashes via 308 redirects. So we can't rely on the URL
  // alone to know "file vs. directory". Instead:
  //
  //   - No segments                → it's the bucket root, definitely a dir.
  //   - URL ends with "/"          → user explicitly asked for a dir.
  //   - Otherwise                  → probe with HEAD against /public/{key}.
  //       200 → it's an object   → window.location.replace to the raw URL
  //       404 → treat as a folder under prefix = "joined/segments/"
  //
  // The HEAD round-trip costs ~10–50 ms and only runs for ambiguous URLs.
  const hasTrailingSlash = pathname?.endsWith("/") ?? false;
  const explicitDir      = segments.length === 0 || hasTrailingSlash;

  const [mode, setMode] = useState<"dir" | "probing">(explicitDir ? "dir" : "probing");
  const [prefix, setPrefix] = useState<string>(
    explicitDir
      ? (segments.length ? segments.join("/") + "/" : "")
      : "",
  );

  useEffect(() => {
    // Reset state when the URL changes
    if (explicitDir) {
      setMode("dir");
      setPrefix(segments.length ? segments.join("/") + "/" : "");
      return;
    }
    const candidateKey = segments.join("/");
    setMode("probing");
    const url = `${publicBase()}/public/${encodeURIComponent(bucket)}/${encodeKey(candidateKey)}`;
    let cancelled = false;
    fetch(url, { method: "HEAD", credentials: "omit" })
      .then((r) => {
        if (cancelled) return;
        if (r.ok) {
          // It's an object — redirect to the raw URL. replace() so the
          // back button skips this transient page.
          window.location.replace(url);
        } else {
          // Not an object — render as a directory listing.
          setPrefix(candidateKey + "/");
          setMode("dir");
        }
      })
      .catch(() => {
        if (cancelled) return;
        setPrefix(candidateKey + "/");
        setMode("dir");
      });
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pathname]);

  if (mode === "probing") {
    return (
      <div className="max-w-[1100px] mx-auto px-4 sm:px-6 lg:px-10 py-20 text-center text-muted text-sm">
        Opening…
      </div>
    );
  }

  return <DirectoryView bucket={bucket} prefix={prefix} router={router} />;
}

// ============================================================================
// Directory view
// ============================================================================

function DirectoryView({ bucket, prefix, router }: {
  bucket: string;
  prefix: string;
  router: ReturnType<typeof useRouter>;
}) {
  const [data,    setData]    = useState<PublicListResp | null>(null);
  const [error,   setError]   = useState<"notfound" | "server" | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    setError(null);
    // The public listing endpoint derives the prefix from the URL PATH, not
    // a query string. So /public/{bucket}/foo/bar/ lists everything under
    // "foo/bar/". We tack on ?format=json so the server bypasses any
    // index.html and gives us the JSON instead.
    const url = `${publicBase()}/public/${encodeURIComponent(bucket)}/${encodeKey(prefix)}?format=json`;
    fetch(url, {
      credentials: "omit",
      headers: { Accept: "application/json" },
    })
      .then(async (r) => {
        if (r.status === 404) { setError("notfound"); return null; }
        if (!r.ok)             { setError("server");   return null; }
        const ct = r.headers.get("content-type") || "";
        if (!ct.includes("application/json")) {
          // The bucket has an index.html at the prefix root and the server
          // served that instead of a listing. Treat as "no listing available"
          // — we redirect into the static site.
          window.location.href = `${publicBase()}/public/${encodeURIComponent(bucket)}/${encodeKey(prefix)}`;
          return null;
        }
        return r.json() as Promise<PublicListResp>;
      })
      .then((j) => { if (j) setData(j); })
      .catch(() => setError("server"))
      .finally(() => setLoading(false));
  }, [bucket, prefix]);

  // ---- Render ---------------------------------------------------------------

  if (error === "notfound") {
    return <NotFound bucket={bucket} />;
  }
  if (error === "server") {
    return (
      <div className="max-w-[1100px] mx-auto px-4 sm:px-6 lg:px-10 py-20 text-center">
        <p className="text-danger text-sm">Couldn’t load this listing.</p>
      </div>
    );
  }
  if (loading || !data) {
    return (
      <div className="max-w-[1100px] mx-auto px-4 sm:px-6 lg:px-10 py-10">
        <ListingSkeleton />
      </div>
    );
  }

  return (
    <Listing
      bucket={bucket}
      prefix={prefix}
      data={data}
      onNavigateFolder={(p) => {
        // Drop trailing slash so Next.js doesn't 308-redirect us.
        const noSlash = p.replace(/\/$/, "");
        router.push(`/p/${encodeURIComponent(bucket)}/${encodeKey(noSlash)}`);
      }}
    />
  );
}

// ============================================================================
// Listing — header, breadcrumbs, table/cards
// ============================================================================

interface FolderEntry { name: string; prefix: string; }
interface FileEntry   { name: string; key: string; size: number; contentType: string; lastModified: string; }

function Listing({ bucket, prefix, data, onNavigateFolder }: {
  bucket: string;
  prefix: string;
  data: PublicListResp;
  onNavigateFolder: (prefix: string) => void;
}) {
  // Group flat-list of objects into immediate children: folders (next slug
  // followed by "/") and files (key has no further slashes after prefix).
  const { folders, files, totalBytes } = useMemo(() => {
    const folderSet = new Map<string, FolderEntry>();
    const files: FileEntry[] = [];
    let totalBytes = 0;
    for (const o of data.objects) {
      if (!o.key.startsWith(prefix)) continue;
      const rest = o.key.slice(prefix.length);
      if (rest === "") continue; // exact-prefix oddity
      const slash = rest.indexOf("/");
      if (slash === -1) {
        files.push({
          name: rest,
          key: o.key,
          size: o.size,
          contentType: o.contentType,
          lastModified: o.lastModified,
        });
        totalBytes += o.size;
      } else {
        const folderName = rest.slice(0, slash);
        const folderPrefix = prefix + folderName + "/";
        if (!folderSet.has(folderPrefix)) {
          folderSet.set(folderPrefix, { name: folderName, prefix: folderPrefix });
        }
      }
    }
    const folders = Array.from(folderSet.values()).sort((a, b) =>
      a.name.localeCompare(b.name, undefined, { sensitivity: "base" }));
    files.sort((a, b) =>
      a.name.localeCompare(b.name, undefined, { sensitivity: "base" }));
    return { folders, files, totalBytes };
  }, [data, prefix]);

  const isEmpty = folders.length === 0 && files.length === 0;
  const crumbs = useMemo(() => buildCrumbs(bucket, prefix), [bucket, prefix]);

  return (
    <div className="max-w-[1100px] mx-auto px-4 sm:px-6 lg:px-10 py-8">
      {/* Breadcrumbs */}
      <nav className="text-[13px] text-muted flex items-center flex-wrap gap-y-1 mb-4">
        <Link href="/p" className="hover:text-text">Public</Link>
        {crumbs.map((c, i) => (
          <span key={c.href} className="flex items-center">
            <ChevronRight size={12} className="mx-1 opacity-50" />
            {i === crumbs.length - 1 ? (
              <span className="text-text font-medium truncate max-w-[40ch]">{c.label}</span>
            ) : (
              <Link href={c.href} className="hover:text-text truncate max-w-[20ch]">{c.label}</Link>
            )}
          </span>
        ))}
      </nav>

      {/* Title + counts strip */}
      <div className="flex items-baseline justify-between gap-4 mb-6 flex-wrap">
        <h1 className="text-2xl font-semibold tracking-tight flex items-center gap-2 min-w-0">
          <FolderOpen size={20} className="text-muted shrink-0" />
          <span className="font-mono text-[1.05rem] truncate">{prefix || "/"}</span>
        </h1>
        <p className="text-xs text-muted whitespace-nowrap">
          {files.length} {files.length === 1 ? "file" : "files"}
          {folders.length > 0 && <> · {folders.length} {folders.length === 1 ? "folder" : "folders"}</>}
          {totalBytes > 0 && <> · {formatBytes(totalBytes)}</>}
        </p>
      </div>

      {isEmpty ? (
        <EmptyState />
      ) : (
        <div className="border border-border rounded-lg overflow-hidden bg-surface">
          <table className="w-full text-sm stack-rows">
            <thead className="text-xs uppercase tracking-wider text-muted bg-bg/40">
              <tr className="border-b border-border">
                <th className="text-left font-medium px-4 py-2.5">Name</th>
                <th className="text-left font-medium px-4 py-2.5 w-28">Size</th>
                <th className="text-left font-medium px-4 py-2.5 w-44">Modified</th>
                <th className="text-right font-medium px-4 py-2.5 w-16"></th>
              </tr>
            </thead>
            <tbody>
              {folders.map((f) => (
                <tr
                  key={f.prefix}
                  className="border-b border-border-subtle last:border-b-0 hover:bg-bg/40 cursor-pointer transition-colors"
                  onClick={() => onNavigateFolder(f.prefix)}
                >
                  <td className="px-4 py-3 min-w-0" data-label="Name">
                    <span className="flex items-center gap-2.5 font-medium">
                      <Folder size={16} className="text-link shrink-0" />
                      <span className="truncate">{f.name}/</span>
                    </span>
                  </td>
                  <td className="px-4 py-3 text-muted text-xs" data-label="Size">folder</td>
                  <td className="px-4 py-3 text-muted text-xs" data-label="Modified">—</td>
                  <td className="px-4 py-3 text-right text-muted" data-label="">
                    <ChevronRight size={14} className="inline opacity-50" />
                  </td>
                </tr>
              ))}
              {files.map((f) => (
                <FileRow key={f.key} bucket={bucket} file={f} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

// ----------------------------------------------------------------------------
// File row — thumbnail (for media) or extension icon, plus download anchor
// ----------------------------------------------------------------------------

function FileRow({ bucket, file }: { bucket: string; file: FileEntry }) {
  const kind     = classify(file.key, file.contentType);
  const href     = `${publicBase()}/public/${encodeURIComponent(bucket)}/${encodeKey(file.key)}`;
  const thumbUrl = kind === "image"
    ? `${href}?w=80&h=80&fit=cover&fmt=webp&q=70`
    : null;

  return (
    <tr className="border-b border-border-subtle last:border-b-0 hover:bg-bg/40 transition-colors">
      <td className="px-4 py-3 min-w-0" data-label="Name">
        <a
          href={href}
          download={file.name}
          className="flex items-center gap-2.5 min-w-0 hover:text-link transition-colors"
          title={file.key}
        >
          <FileThumb thumbUrl={thumbUrl} kind={kind} />
          <span className="truncate font-mono text-[13px]">{file.name}</span>
        </a>
      </td>
      <td className="px-4 py-3 text-muted text-xs whitespace-nowrap" data-label="Size">
        {formatBytes(file.size)}
      </td>
      <td className="px-4 py-3 text-muted text-xs whitespace-nowrap" data-label="Modified">
        {formatDate(file.lastModified)}
      </td>
      <td className="px-4 py-3 text-right actions" data-label="">
        <a
          href={href}
          download={file.name}
          className="inline-flex text-muted hover:text-text transition-colors"
          aria-label={`Download ${file.name}`}
          title="Download"
        >
          <Download size={14} />
        </a>
      </td>
    </tr>
  );
}

function FileThumb({ thumbUrl, kind }: { thumbUrl: string | null; kind: ReturnType<typeof classify> }) {
  if (thumbUrl) {
    return (
      // eslint-disable-next-line @next/next/no-img-element
      <img
        src={thumbUrl}
        alt=""
        loading="lazy"
        className="w-7 h-7 rounded object-cover bg-bg shrink-0 border border-border-subtle"
        onError={(e) => { (e.currentTarget as HTMLImageElement).style.display = "none"; }}
      />
    );
  }
  const cls = "text-muted shrink-0";
  if (kind === "video") return <FileVideo size={16} className={cls} />;
  if (kind === "audio") return <FileAudio size={16} className={cls} />;
  if (kind === "image") return <FileImage size={16} className={cls} />;
  return <FileText size={16} className={cls} />;
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

function buildCrumbs(bucket: string, prefix: string): { label: string; href: string }[] {
  const out: { label: string; href: string }[] = [];
  const bEnc = encodeURIComponent(bucket);
  // We deliberately OMIT the trailing slash on every href; Next.js's default
  // trailingSlash:false would 308 it away, costing us a round trip.
  out.push({ label: bucket, href: `/p/${bEnc}` });
  if (!prefix) return out;
  const parts = prefix.replace(/\/$/, "").split("/");
  const acc: string[] = [];
  for (const part of parts) {
    acc.push(encodeURIComponent(part));
    out.push({ label: part, href: `/p/${bEnc}/${acc.join("/")}` });
  }
  return out;
}

// ============================================================================
// Empty / not-found states
// ============================================================================

function EmptyState() {
  return (
    <div className="border border-dashed border-border rounded-lg py-20 px-6 text-center">
      <Inbox size={28} className="mx-auto text-muted mb-3 opacity-60" />
      <p className="text-sm text-text-soft">This folder is empty.</p>
    </div>
  );
}

function NotFound({ bucket }: { bucket: string }) {
  return (
    <div className="max-w-[600px] mx-auto px-4 sm:px-6 py-24 text-center">
      <Lock size={32} className="mx-auto text-muted mb-4 opacity-60" />
      <h1 className="text-2xl font-semibold tracking-tight mb-3">This bucket isn’t public.</h1>
      <p className="text-sm text-text-soft leading-relaxed">
        <span className="font-mono text-text">{bucket}</span> either doesn’t exist or the owner
        hasn’t flagged it as publicly readable. If you’re the owner, head to the
        dashboard and toggle <em>Public</em> on the bucket.
      </p>
      <Link
        href="/"
        className="inline-block mt-8 text-sm text-link hover:underline underline-offset-2"
      >
        Back to dashboard
      </Link>
    </div>
  );
}

// ============================================================================
// Loading skeleton
// ============================================================================

function ListingSkeleton() {
  return (
    <div className="animate-pulse">
      <div className="h-4 bg-surface rounded w-1/3 mb-4" />
      <div className="h-8 bg-surface rounded w-2/3 mb-6" />
      <div className="border border-border rounded-lg overflow-hidden bg-surface">
        {Array.from({ length: 6 }).map((_, i) => (
          <div key={i} className="border-b border-border-subtle last:border-0 px-4 py-3 flex items-center gap-3">
            <div className="w-7 h-7 rounded bg-bg/60" />
            <div className="h-4 bg-bg/60 rounded flex-1 max-w-[40%]" />
            <div className="h-3 bg-bg/60 rounded w-16" />
            <div className="h-3 bg-bg/60 rounded w-24" />
          </div>
        ))}
      </div>
    </div>
  );
}
