"use client";

import { useState, FormEvent } from "react";
import { api, ApiError } from "@/lib/api";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Download, Loader2 } from "lucide-react";

interface Props {
  bucket: string;
  prefix?: string;
  onComplete?: () => void;
}

type SourceKind = "direct" | "nc-public" | "nc-private";

/**
 * Server-side import. Three modes:
 *   1. Direct URL — paste any public download link
 *   2. Nextcloud public share — paste a /s/<token> link, we add /download
 *   3. Nextcloud private file — fill in server, user, app-password, path
 */
export function ImportURL({ bucket, prefix = "", onComplete }: Props) {
  const [open, setOpen] = useState(false);
  const [kind, setKind] = useState<SourceKind>("direct");

  // direct
  const [url, setUrl] = useState("");
  const [authHeader, setAuthHeader] = useState("");

  // nc-public
  const [shareLink, setShareLink] = useState("");

  // nc-private
  const [ncServer, setNcServer] = useState("");
  const [ncUser, setNcUser] = useState("");
  const [ncPass, setNcPass] = useState("");
  const [ncPath, setNcPath] = useState("");

  // common
  const [key, setKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [result, setResult] = useState<string | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setErr(null); setResult(null); setBusy(true);

    try {
      const { finalUrl, finalAuth, derivedFilename } = buildRequest(
        kind, { url, authHeader, shareLink, ncServer, ncUser, ncPass, ncPath },
      );

      const useKey = (key.trim() || derivedFilename || "import");
      const fullKey = (prefix ? prefix.replace(/\/$/, "") + "/" : "") + useKey;

      const headers: Record<string, string> = {};
      if (finalAuth) headers["Authorization"] = finalAuth;

      await api<{ jobId: string }>(`/${bucket}/${encodeKey(fullKey)}?import`, {
        method: "POST",
        body: JSON.stringify({ url: finalUrl, headers }),
      });

      setResult(`Queued — ${fullKey}. Progress shown below.`);
      setKey(""); setUrl(""); setAuthHeader(""); setShareLink(""); setNcPath("");
      onComplete?.();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e instanceof Error ? e.message : "import failed"));
    } finally {
      setBusy(false);
    }
  };

  if (!open) {
    return (
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="group flex items-start gap-3 p-3 w-full text-left border border-border rounded-md
                   bg-bg hover:bg-card hover:border-accent transition-colors"
      >
        <Download size={18} className="shrink-0 mt-0.5 text-accent" />
        <div className="min-w-0">
          <div className="text-sm font-semibold">Import from a URL</div>
          <div className="text-[11px] text-muted leading-snug">
            Server-side fetch from any URL or Nextcloud share.
          </div>
        </div>
      </button>
    );
  }

  return (
    <form onSubmit={submit} className="space-y-3 border border-border rounded p-3 bg-panel/50">
      <div className="flex items-center justify-between">
        <p className="text-xs font-semibold">Import from URL</p>
        <button type="button" onClick={() => setOpen(false)}
                className="text-xs text-muted hover:text-text">cancel</button>
      </div>

      {/* Source kind selector */}
      <div className="flex gap-1 text-xs">
        {([
          ["direct",     "Direct URL"],
          ["nc-public",  "Nextcloud share"],
          ["nc-private", "Nextcloud private"],
        ] as [SourceKind, string][]).map(([k, label]) => (
          <button
            key={k}
            type="button"
            onClick={() => setKind(k)}
            className={`px-3 py-1 rounded ${
              kind === k ? "bg-accent text-white" : "bg-bg border border-border text-muted hover:text-text"
            }`}
          >{label}</button>
        ))}
      </div>

      {/* Mode-specific fields */}
      {kind === "direct" && <>
        <div>
          <label className="text-[10px] text-muted uppercase">Source URL</label>
          <Input
            type="url"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://example.com/path/file.mp4"
            required
          />
        </div>
        <div>
          <label className="text-[10px] text-muted uppercase">Auth header (optional)</label>
          <Input
            value={authHeader}
            onChange={(e) => setAuthHeader(e.target.value)}
            placeholder='e.g. Basic dXNlcjpwYXNz   or   Bearer eyJhbGc...'
          />
        </div>
      </>}

      {kind === "nc-public" && <>
        <div>
          <label className="text-[10px] text-muted uppercase">Nextcloud share link</label>
          <Input
            type="url"
            value={shareLink}
            onChange={(e) => setShareLink(e.target.value)}
            placeholder="https://nextcloud.example.com/s/AbCdEf12345"
            required
          />
          <p className="text-[11px] text-muted mt-1">
            In Nextcloud: right-click file → Share → toggle &quot;Share link&quot; → copy.
            We append <code>/download</code> for you.
          </p>
        </div>
      </>}

      {kind === "nc-private" && <>
        <div className="grid grid-cols-2 gap-2">
          <div>
            <label className="text-[10px] text-muted uppercase">Nextcloud server URL</label>
            <Input
              type="url"
              value={ncServer}
              onChange={(e) => setNcServer(e.target.value)}
              placeholder="https://nextcloud.example.com"
              required
            />
          </div>
          <div>
            <label className="text-[10px] text-muted uppercase">Username</label>
            <Input
              value={ncUser}
              onChange={(e) => setNcUser(e.target.value)}
              placeholder="alice"
              required
            />
          </div>
        </div>
        <div>
          <label className="text-[10px] text-muted uppercase">App password</label>
          <Input
            type="password"
            value={ncPass}
            onChange={(e) => setNcPass(e.target.value)}
            placeholder="generate in Nextcloud → Settings → Security → Devices &amp; sessions"
            required
          />
        </div>
        <div>
          <label className="text-[10px] text-muted uppercase">File path</label>
          <Input
            value={ncPath}
            onChange={(e) => setNcPath(e.target.value)}
            placeholder="MOVIES/Tamasha (2015).mkv"
            required
          />
          <p className="text-[11px] text-muted mt-1">
            Path inside your Nextcloud root, exactly as shown in the URL bar when you browse to it.
            Don&apos;t URL-encode &mdash; just paste the human-readable name.
          </p>
        </div>
      </>}

      {/* Optional rename */}
      <div>
        <label className="text-[10px] text-muted uppercase">Save in bucket as (optional)</label>
        <Input
          value={key}
          onChange={(e) => setKey(e.target.value)}
          placeholder="auto-derived from source filename"
        />
      </div>

      <div className="flex items-center justify-between">
        <Button type="submit" disabled={busy} size="sm">
          {busy ? <><Loader2 size={14} className="mr-1 animate-spin" /> Queueing...</> : "Import"}
        </Button>
        {result && <p className="text-xs text-success">{result}</p>}
        {err && <p className="text-xs text-danger">{err}</p>}
      </div>

      <p className="text-[11px] text-muted">
        The download runs on the server. Close the tab if you want —
        come back later, the progress bar will still be there.
      </p>
    </form>
  );
}

// ---- helpers ----

function buildRequest(
  kind: SourceKind,
  fields: {
    url: string; authHeader: string;
    shareLink: string;
    ncServer: string; ncUser: string; ncPass: string; ncPath: string;
  },
): { finalUrl: string; finalAuth: string; derivedFilename: string } {
  if (kind === "direct") {
    if (!fields.url) throw new Error("URL is required");
    return {
      finalUrl: fields.url,
      finalAuth: fields.authHeader.trim(),
      derivedFilename: filenameFromUrl(fields.url),
    };
  }

  if (kind === "nc-public") {
    let link = fields.shareLink.trim().replace(/\/$/, "");
    if (!/^https?:\/\//.test(link)) {
      throw new Error("Share link must start with http(s)://");
    }
    // Idempotent: don't append twice
    if (!link.endsWith("/download")) link += "/download";
    return {
      finalUrl: link,
      finalAuth: "",
      derivedFilename: filenameFromUrl(link.replace(/\/download$/, "")) || "shared-file",
    };
  }

  // nc-private
  const server = fields.ncServer.trim().replace(/\/$/, "");
  if (!/^https?:\/\//.test(server)) {
    throw new Error("Nextcloud server URL must start with http(s)://");
  }
  if (!fields.ncUser) throw new Error("Username is required");
  if (!fields.ncPass) throw new Error("App password is required");
  if (!fields.ncPath) throw new Error("File path is required");

  // Build WebDAV URL with each path segment percent-encoded
  const encodedPath = fields.ncPath.split("/")
    .filter(Boolean)
    .map(encodeURIComponent)
    .join("/");
  const url = `${server}/remote.php/dav/files/${encodeURIComponent(fields.ncUser)}/${encodedPath}`;

  // UTF-8-safe base64 of "user:pass"
  const auth = "Basic " + utf8Base64(`${fields.ncUser}:${fields.ncPass}`);

  return {
    finalUrl: url,
    finalAuth: auth,
    derivedFilename: fields.ncPath.split("/").pop() || "file",
  };
}

function filenameFromUrl(u: string): string {
  try {
    const parsed = new URL(u);
    const last = parsed.pathname.split("/").filter(Boolean).pop() || "";
    return decodeURIComponent(last);
  } catch {
    return "";
  }
}

function utf8Base64(s: string): string {
  // Properly encode UTF-8 then base64 — btoa alone breaks on non-ASCII
  const bytes = new TextEncoder().encode(s);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}

function encodeKey(k: string): string {
  return k.split("/").map(encodeURIComponent).join("/");
}
