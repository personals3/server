"use client";

import { useState, useCallback, useRef, DragEvent, ChangeEvent } from "react";
import { uploadFile, UploadHandle } from "@/lib/multipart";
import { formatBytes } from "@/lib/format";
import { Upload, CheckCircle2, XCircle, X } from "lucide-react";

type Status = "queued" | "uploading" | "done" | "error" | "cancelled";

interface Task {
  id: string;
  file: File;
  status: Status;
  loaded: number;
  error?: string;
  startedAt?: number;       // ms timestamp
  throughputBps: number;    // bytes/sec, rolling
  handle?: UploadHandle;    // for cancel
}

interface Props {
  bucket: string;
  prefix?: string;
  onComplete?: () => void;
}

export function UploadZone({ bucket, prefix = "", onComplete }: Props) {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [dragging, setDragging] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  const enqueue = useCallback((files: FileList | File[]) => {
    const newTasks: Task[] = Array.from(files).map((f) => ({
      id: crypto.randomUUID(),
      file: f,
      status: "queued",
      loaded: 0,
      throughputBps: 0,
    }));
    setTasks((prev) => [...prev, ...newTasks]);

    void (async () => {
      for (const t of newTasks) {
        // Skip if user cancelled before this task started
        let snapshot = newTasks.find((x) => x.id === t.id);
        if (!snapshot) continue;

        const key = (prefix ? prefix.replace(/\/$/, "") + "/" : "") + t.file.name;
        const startedAt = Date.now();
        let lastUpdate = startedAt;
        let lastLoaded = 0;

        const handle = uploadFile(bucket, key, t.file, (loaded) => {
          const now = Date.now();
          // EWMA-ish throughput: blend with previous reading
          if (now - lastUpdate >= 250) {
            const deltaBytes = loaded - lastLoaded;
            const deltaMs = now - lastUpdate;
            const instant = (deltaBytes * 1000) / Math.max(deltaMs, 1);
            setTasks((prev) => prev.map((x) => x.id === t.id ? {
              ...x,
              loaded,
              throughputBps: x.throughputBps === 0 ? instant : (x.throughputBps * 0.6 + instant * 0.4),
            } : x));
            lastUpdate = now;
            lastLoaded = loaded;
          } else {
            setTasks((prev) => prev.map((x) => x.id === t.id ? { ...x, loaded } : x));
          }
        });

        setTasks((prev) => prev.map((x) => x.id === t.id
          ? { ...x, status: "uploading", startedAt, handle }
          : x));

        try {
          await handle.promise;
          setTasks((prev) => prev.map((x) => x.id === t.id
            ? { ...x, status: "done", loaded: t.file.size }
            : x));
        } catch (e: unknown) {
          const isAbort = e instanceof DOMException && e.name === "AbortError";
          setTasks((prev) => prev.map((x) => x.id === t.id ? {
            ...x,
            status: isAbort ? "cancelled" : "error",
            error: isAbort ? undefined : (e instanceof Error ? e.message : String(e)),
          } : x));
        }
      }
      onComplete?.();
    })();
  }, [bucket, prefix, onComplete]);

  const cancelTask = (id: string) => {
    setTasks((prev) => prev.map((x) => {
      if (x.id !== id) return x;
      if (x.handle) x.handle.abort();
      // queued tasks haven't started; mark cancelled immediately
      return x.status === "queued" ? { ...x, status: "cancelled" } : x;
    }));
  };

  const dismiss = (id: string) => {
    setTasks((prev) => prev.filter((x) => x.id !== id));
  };

  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    setDragging(false);
    enqueue(e.dataTransfer.files);
  };

  const onFiles = (e: ChangeEvent<HTMLInputElement>) => {
    if (e.target.files) enqueue(e.target.files);
    e.target.value = "";
  };

  return (
    <div className="space-y-3">
      <div
        onDrop={onDrop}
        onDragOver={(e) => { e.preventDefault(); setDragging(true); }}
        onDragLeave={() => setDragging(false)}
        onClick={() => inputRef.current?.click()}
        className={`border-2 border-dashed rounded-lg p-6 cursor-pointer transition text-center
          ${dragging ? "border-accent bg-blue-950/30" : "border-border hover:border-muted"}`}
      >
        <Upload className="mx-auto mb-2 text-muted" size={28} />
        <p className="text-sm font-medium">Drop files here or click to browse</p>
        <p className="text-xs text-muted mt-1">
          Files &gt;8 MiB are uploaded in 5 MiB chunks with 4-way parallelism
        </p>
        <input ref={inputRef} type="file" multiple className="hidden" onChange={onFiles} />
      </div>

      {tasks.length > 0 && (
        <div className="space-y-2">
          {tasks.map((t) => <TaskRow key={t.id} task={t} onCancel={cancelTask} onDismiss={dismiss} />)}
        </div>
      )}
    </div>
  );
}

function TaskRow({ task: t, onCancel, onDismiss }: {
  task: Task;
  onCancel: (id: string) => void;
  onDismiss: (id: string) => void;
}) {
  const pct = t.file.size > 0 ? (t.loaded / t.file.size) * 100 : 0;
  const remainingBytes = t.file.size - t.loaded;
  const etaSec = t.throughputBps > 0 ? remainingBytes / t.throughputBps : 0;
  const showCancel = t.status === "queued" || t.status === "uploading";

  return (
    <div className="bg-panel border border-border rounded p-3">
      <div className="flex items-center justify-between text-sm mb-1 gap-2">
        <span className="font-mono truncate flex-1">{t.file.name}</span>
        <span className="text-muted text-xs whitespace-nowrap">
          {t.status === "done"      && <CheckCircle2 size={14} className="inline text-success" />}
          {t.status === "error"     && <XCircle size={14} className="inline text-danger" />}
          {t.status === "cancelled" && <XCircle size={14} className="inline text-muted" />}
          {" "}{formatBytes(t.loaded)} / {formatBytes(t.file.size)}
          {t.status === "uploading" && t.throughputBps > 0 && (
            <>  · <span className="text-accent">{formatBytes(t.throughputBps)}/s</span>
              {etaSec > 1 && etaSec < 86400 && <>  · ETA {fmtEta(etaSec)}</>}
            </>
          )}
        </span>
        {showCancel ? (
          <button onClick={() => onCancel(t.id)}
                  className="text-muted hover:text-danger shrink-0" title="Cancel upload">
            <X size={14} />
          </button>
        ) : (
          <button onClick={() => onDismiss(t.id)}
                  className="text-muted hover:text-text shrink-0" title="Dismiss">
            <X size={14} />
          </button>
        )}
      </div>
      <div className="h-1 bg-border rounded overflow-hidden">
        <div
          className={`h-full transition-all ${
            t.status === "error" ? "bg-danger" :
            t.status === "cancelled" ? "bg-muted" :
            t.status === "done" ? "bg-success" : "bg-accent"
          }`}
          style={{ width: `${pct}%` }}
        />
      </div>
      {t.error && <p className="text-xs text-danger mt-1">{t.error}</p>}
      {t.status === "cancelled" && (
        <p className="text-xs text-muted mt-1">Cancelled — quota refunded</p>
      )}
    </div>
  );
}

function fmtEta(seconds: number): string {
  if (seconds < 60) return `${Math.round(seconds)}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${Math.round(seconds % 60)}s`;
  return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`;
}
