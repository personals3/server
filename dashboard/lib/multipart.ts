// Browser-side multipart upload: splits a File into 5 MiB chunks and
// uploads them in parallel (max 4 at a time).
//
// Supports an AbortController for cancellation: on abort, in-flight
// XHRs are killed AND the server-side multipart upload is aborted so
// the user's quota is refunded.

import { API, getToken } from "./api";

const CHUNK = 5 * 1024 * 1024; // 5 MiB
const CONCURRENCY = 4;
const MULTIPART_THRESHOLD = 8 * 1024 * 1024; // use multipart for files > 8 MiB

type ProgressCB = (loaded: number, total: number) => void;

export interface UploadHandle {
  /** Resolves when complete, rejects on error/abort. */
  promise: Promise<void>;
  /** Cancel: aborts in-flight requests and the server-side upload session. */
  abort: () => void;
}

export function uploadFile(
  bucket: string,
  key: string,
  file: File,
  onProgress?: ProgressCB,
): UploadHandle {
  const controller = new AbortController();
  const promise = file.size <= MULTIPART_THRESHOLD
    ? singleUpload(bucket, key, file, onProgress, controller.signal)
    : multipartUpload(bucket, key, file, onProgress, controller);

  return {
    promise,
    abort: () => controller.abort(),
  };
}

async function singleUpload(
  bucket: string,
  key: string,
  file: File,
  onProgress: ProgressCB | undefined,
  signal: AbortSignal,
): Promise<void> {
  await xhrPut(`${API}/${bucket}/${encodeKey(key)}`, file,
    file.type || "application/octet-stream", onProgress, signal);
}

async function multipartUpload(
  bucket: string,
  key: string,
  file: File,
  onProgress: ProgressCB | undefined,
  controller: AbortController,
): Promise<void> {
  const token = getToken();
  if (!token) throw new Error("no auth token");
  const signal = controller.signal;

  // 1. Initiate
  const initRes = await fetch(
    `${API}/${bucket}/${encodeKey(key)}?uploads`,
    {
      method: "POST",
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": file.type || "application/octet-stream",
      },
      signal,
    },
  );
  if (!initRes.ok) throw new Error(`initiate failed: ${initRes.status}`);
  const { uploadId } = await initRes.json();

  // Wire up server-side abort on AbortSignal
  const abortServerSide = async () => {
    try {
      await fetch(`${API}/${bucket}/${encodeKey(key)}?uploadId=${uploadId}`, {
        method: "DELETE",
        headers: { Authorization: `Bearer ${token}` },
      });
    } catch { /* best effort */ }
  };
  signal.addEventListener("abort", () => { void abortServerSide(); }, { once: true });

  // 2. Build chunk list
  const chunks: { partNumber: number; start: number; end: number }[] = [];
  for (let off = 0, n = 1; off < file.size; off += CHUNK, n++) {
    chunks.push({ partNumber: n, start: off, end: Math.min(off + CHUNK, file.size) });
  }

  // Per-part progress for overall reporting
  const partProgress = new Map<number, number>();
  const reportProgress = () => {
    if (!onProgress) return;
    let loaded = 0;
    for (const v of partProgress.values()) loaded += v;
    onProgress(loaded, file.size);
  };

  const etags: { partNumber: number; etag: string }[] = [];

  // 3. Upload parts with a promise pool.
  // If ONE worker throws, abort the controller — siblings see signal.aborted,
  // their in-flight XHRs get killed, and the server-side abort fires. No
  // wasted bytes on a doomed upload.
  let cursor = 0;
  const workers = Array.from({ length: CONCURRENCY }, async () => {
    while (!signal.aborted) {
      const idx = cursor++;
      if (idx >= chunks.length) return;
      const { partNumber, start, end } = chunks[idx];
      const blob = file.slice(start, end);

      const url = `${API}/${bucket}/${encodeKey(key)}?partNumber=${partNumber}&uploadId=${uploadId}`;
      try {
        const etag = await xhrPut(url, blob, "application/octet-stream",
          (loaded) => { partProgress.set(partNumber, loaded); reportProgress(); },
          signal);
        etags.push({ partNumber, etag: etag.replace(/^"|"$/g, "") });
      } catch (e) {
        // First failure: abort everything else.
        if (!signal.aborted) controller.abort();
        throw e;
      }
    }
  });

  try {
    await Promise.all(workers);
  } catch (e) {
    // Server-side abort fires via signal listener; surface the original error.
    throw e;
  }

  if (signal.aborted) {
    throw new DOMException("aborted", "AbortError");
  }

  // 4. Complete
  etags.sort((a, b) => a.partNumber - b.partNumber);
  const completeRes = await fetch(
    `${API}/${bucket}/${encodeKey(key)}?uploadId=${uploadId}`,
    {
      method: "POST",
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ parts: etags }),
      signal,
    },
  );
  if (!completeRes.ok) {
    const txt = await completeRes.text().catch(() => "");
    throw new Error(`complete failed: ${completeRes.status} ${txt}`);
  }
}

/**
 * XHR-based PUT so we get upload progress events.
 * Returns the ETag from the response header.
 */
function xhrPut(
  url: string,
  body: Blob,
  contentType: string,
  onProgress: ProgressCB | undefined,
  signal: AbortSignal,
): Promise<string> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", url);
    const token = getToken();
    if (token) xhr.setRequestHeader("Authorization", `Bearer ${token}`);
    xhr.setRequestHeader("Content-Type", contentType);

    const onAbort = () => xhr.abort();
    signal.addEventListener("abort", onAbort, { once: true });

    if (onProgress) {
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) onProgress(e.loaded, e.total);
      };
    }
    xhr.onload = () => {
      signal.removeEventListener("abort", onAbort);
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(xhr.getResponseHeader("ETag") || "");
      } else {
        reject(new Error(`PUT failed: ${xhr.status} ${xhr.responseText.slice(0, 200)}`));
      }
    };
    xhr.onerror = () => {
      signal.removeEventListener("abort", onAbort);
      reject(new Error("network error"));
    };
    xhr.onabort = () => {
      signal.removeEventListener("abort", onAbort);
      reject(new DOMException("aborted", "AbortError"));
    };
    xhr.send(body);
  });
}

function encodeKey(key: string): string {
  return key.split("/").map(encodeURIComponent).join("/");
}
