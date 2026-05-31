"use client";

import { useState } from "react";
import { api, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { X, Copy, Check, ExternalLink } from "lucide-react";
import { useToast } from "@/components/toast";

interface Props {
  bucket: string;
  objectKey: string;
  onClose: () => void;
}

type Preset = { label: string; sec: number };

const PRESETS: Preset[] = [
  { label: "1 hour",  sec: 60 * 60 },
  { label: "24 hours", sec: 60 * 60 * 24 },
  { label: "7 days",   sec: 60 * 60 * 24 * 7 },
  { label: "30 days",  sec: 60 * 60 * 24 * 30 },
];

/**
 * Asks the API for a presigned URL with a user-picked expiry, then shows it
 * with a copy button. Closes via the X.
 */
export function ShareModal({ bucket, objectKey, onClose }: Props) {
  const toast = useToast();
  const [expiresSec, setExpiresSec] = useState(PRESETS[1].sec); // default: 24h
  const [forceDownload, setForceDownload] = useState(false);
  const [linkType, setLinkType] = useState<"GET" | "PUT">("GET");
  const [url, setUrl] = useState<string | null>(null);
  const [expiresAt, setExpiresAt] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);

  const create = async () => {
    setBusy(true);
    try {
      const r = await api<{ url: string; expiresAt: number }>(
        `/${bucket}/${encodeKey(objectKey)}?presign`,
        {
          method: "POST",
          body: JSON.stringify({
            expiresSec,
            method: linkType,
            download: linkType === "GET" ? forceDownload : false,
          }),
        },
      );
      // r.url is relative — make it absolute so it's shareable
      const absolute = new URL(r.url, window.location.origin).toString();
      setUrl(absolute);
      setExpiresAt(r.expiresAt);
      toast.push("success", linkType === "PUT"
        ? "Upload link ready — anyone with this URL can PUT a file to this key."
        : "Share link ready — copy it below.");
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  const copy = () => {
    if (!url) return;
    navigator.clipboard.writeText(url);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  return (
    <div className="fixed inset-0 z-50 bg-black/60 flex items-center justify-center p-4"
         onClick={onClose}>
      <div className="bg-panel border border-border rounded-lg max-w-lg w-full p-5 space-y-4"
           onClick={(e) => e.stopPropagation()}>
        <div className="flex items-center justify-between">
          <h2 className="text-sm font-semibold">Share link</h2>
          <button onClick={onClose} className="text-muted hover:text-text">
            <X size={16} />
          </button>
        </div>

        <p className="text-xs text-muted truncate" title={`${bucket}/${objectKey}`}>
          <span className="font-mono">{bucket}/{objectKey}</span>
        </p>

        <div>
          <label className="text-[10px] uppercase text-muted">Link type</label>
          <div className="grid grid-cols-2 gap-2 mt-1">
            <button
              onClick={() => setLinkType("GET")}
              className={`px-3 py-1.5 text-xs rounded border transition ${
                linkType === "GET"
                  ? "bg-accent border-accent text-white"
                  : "bg-bg border-border hover:border-muted"
              }`}
            >
              Download (GET)
            </button>
            <button
              onClick={() => setLinkType("PUT")}
              className={`px-3 py-1.5 text-xs rounded border transition ${
                linkType === "PUT"
                  ? "bg-amber-500 border-amber-500 text-white"
                  : "bg-bg border-border hover:border-muted"
              }`}
              title="Anyone with the URL can PUT a file to this exact key"
            >
              Upload (PUT)
            </button>
          </div>
          {linkType === "PUT" && (
            <p className="text-[10px] text-amber-300 mt-1">
              ⚠ Anyone with the URL can write any bytes to <code className="font-mono">{objectKey}</code>.
              Use one-shot uploads only — short expiry recommended.
            </p>
          )}
        </div>

        <div>
          <label className="text-[10px] uppercase text-muted">Expires after</label>
          <div className="grid grid-cols-4 gap-2 mt-1">
            {PRESETS.map((p) => (
              <button
                key={p.sec}
                onClick={() => setExpiresSec(p.sec)}
                className={`px-3 py-1.5 text-xs rounded border transition ${
                  expiresSec === p.sec
                    ? "bg-accent border-accent text-white"
                    : "bg-bg border-border hover:border-muted"
                }`}
              >
                {p.label}
              </button>
            ))}
          </div>
        </div>

        {linkType === "GET" && (
          <label className="flex items-center gap-2 text-xs">
            <input
              type="checkbox"
              checked={forceDownload}
              onChange={(e) => setForceDownload(e.target.checked)}
              className="cursor-pointer"
            />
            <span>
              Force download
              <span className="text-muted"> — browser saves to disk instead of opening inline</span>
            </span>
          </label>
        )}

        {!url ? (
          <Button onClick={create} disabled={busy} className="w-full">
            {busy ? "Generating..." : "Create link"}
          </Button>
        ) : (
          <div className="space-y-2">
            <div className="bg-bg p-3 rounded border border-border">
              <div className="flex items-center gap-2">
                <code className="flex-1 font-mono text-[11px] text-accent truncate" title={url}>
                  {url}
                </code>
                <button onClick={copy} className="text-muted hover:text-text shrink-0" title="Copy">
                  {copied ? <Check size={14} className="text-success" /> : <Copy size={14} />}
                </button>
                <a href={url} target="_blank" rel="noreferrer"
                   className="text-muted hover:text-text shrink-0" title="Open in new tab">
                  <ExternalLink size={14} />
                </a>
              </div>
              {expiresAt && (
                <p className="text-[10px] text-muted mt-2">
                  Expires {new Date(expiresAt * 1000).toLocaleString()}
                  {" ("}roughly {humanize(expiresSec)} from now{")"}
                </p>
              )}
            </div>
            <Button variant="secondary" onClick={() => setUrl(null)}>
              Generate another
            </Button>
          </div>
        )}
      </div>
    </div>
  );
}

function humanize(sec: number): string {
  if (sec >= 60 * 60 * 24) return `${Math.round(sec / 86400)} days`;
  if (sec >= 60 * 60)      return `${Math.round(sec / 3600)} hours`;
  return `${Math.round(sec / 60)} minutes`;
}

function encodeKey(k: string): string {
  return k.split("/").map(encodeURIComponent).join("/");
}
