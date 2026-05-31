"use client";

import { useEffect, useRef, useState } from "react";
import { API, getToken } from "@/lib/api";
import { FileImage, FileVideo, FileAudio, FileText } from "lucide-react";

interface Props {
  bucket: string;
  objectKey: string;
  contentType: string;
  size?: number; // for layout — defaults to 32
}

// Thumb renders a small inline preview for image objects, falling back to a
// MIME-type icon for everything else.
//
// Tricks used:
//   - IntersectionObserver: only fetches when the row scrolls into view.
//     Lists of 1000s of files stay fast.
//   - Auth bearer header: we can't put a <img src=...> for a private bucket
//     because the browser won't attach Authorization. Instead we fetch +
//     wrap in a blob: URL.
//   - Resize endpoint: ?w=64&h=64&fit=cover&q=70&fmt=webp returns ~3KB.
//   - Cache: blob URLs are kept in a module-level Map so re-renders + scroll
//     don't re-download.
export function Thumb({ bucket, objectKey, contentType, size = 32 }: Props) {
  const ref = useRef<HTMLDivElement | null>(null);
  const [url, setUrl] = useState<string | null>(null);
  const [errored, setErrored] = useState(false);

  const isImage = contentType.startsWith("image/");

  useEffect(() => {
    if (!isImage) return;
    if (!ref.current) return;
    const cacheKey = `${bucket}/${objectKey}@${size}`;
    if (thumbCache.has(cacheKey)) {
      setUrl(thumbCache.get(cacheKey)!);
      return;
    }
    const io = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          if (!e.isIntersecting) continue;
          io.disconnect();
          void loadThumb(bucket, objectKey, size, cacheKey, setUrl, setErrored);
          return;
        }
      },
      { rootMargin: "200px" },
    );
    io.observe(ref.current);
    return () => io.disconnect();
  }, [bucket, objectKey, isImage, size]);

  if (!isImage) {
    return <FallbackIcon contentType={contentType} size={size} />;
  }

  if (errored) {
    return <FallbackIcon contentType={contentType} size={size} />;
  }

  return (
    <div ref={ref}
         className="rounded overflow-hidden bg-bg flex items-center justify-center shrink-0"
         style={{ width: size, height: size }}>
      {url ? (
        <img src={url} alt="" className="w-full h-full object-cover" />
      ) : (
        <FileImage size={Math.max(12, size * 0.4)} className="text-muted opacity-30" />
      )}
    </div>
  );
}

function FallbackIcon({ contentType, size }: { contentType: string; size: number }) {
  const cls = "text-muted shrink-0";
  const px = Math.max(12, size * 0.5);
  if (contentType.startsWith("video/")) return <FileVideo size={px} className={cls} />;
  if (contentType.startsWith("audio/")) return <FileAudio size={px} className={cls} />;
  if (contentType.startsWith("image/")) return <FileImage size={px} className={cls} />;
  return <FileText size={px} className={cls} />;
}

// Module-level cache of blob URLs. Survives re-renders of individual rows.
// Memory cost: ~5 KB per thumbnail × however many rows the user scrolled
// through this session. Cleared on hard refresh.
const thumbCache = new Map<string, string>();

async function loadThumb(
  bucket: string, key: string, size: number, cacheKey: string,
  setUrl: (u: string) => void, setErrored: (b: boolean) => void,
) {
  try {
    const token = getToken();
    const params = new URLSearchParams({
      w:   size.toString(),
      h:   size.toString(),
      fit: "cover",
      q:   "70",
      fmt: "webp",
    });
    const resp = await fetch(
      `${API}/${encodeURIComponent(bucket)}/${encodeKey(key)}?${params}`,
      { headers: token ? { Authorization: `Bearer ${token}` } : {} },
    );
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const blob = await resp.blob();
    const objectUrl = URL.createObjectURL(blob);
    thumbCache.set(cacheKey, objectUrl);
    setUrl(objectUrl);
  } catch {
    setErrored(true);
  }
}

function encodeKey(k: string): string {
  return k.split("/").map(encodeURIComponent).join("/");
}
