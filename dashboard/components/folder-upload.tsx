"use client";

import { useState, useRef, useMemo, useEffect, ChangeEvent } from "react";
import { uploadFile, UploadHandle } from "@/lib/multipart";
import { api } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import {
  PatternConfig, DEFAULT_PATTERNS, loadPatterns, savePatterns,
  compileMatcher, validateRegex,
} from "@/lib/patterns";
import { Button } from "@/components/ui/button";
import {
  FolderUp, Settings2, X, CheckCircle2, XCircle, Loader2, RotateCcw,
  ArrowDown, ArrowUp,
} from "lucide-react";

interface Props {
  bucket: string;
  /** Optional key prefix prepended to every uploaded path. */
  prefix?: string;
  /** Concurrent files at a time (each does its own 4-way multipart). */
  fileConcurrency?: number;
  onComplete?: () => void;
}

interface PreviewFile {
  file: File;
  /** Path within the picked folder, e.g. "src/components/foo.tsx". */
  relPath: string;
}

type TaskStatus =
  | "queued"
  | "uploading"
  | "done"
  | "error"
  | "cancelled"
  | "rolling_back"   // server-side DELETE in progress for a previously-done file
  | "rolled_back";   // DELETE completed; file no longer on server

type Phase =
  | "idle"           // nothing uploading
  | "uploading"      // pool running
  | "cancelling"     // user clicked cancel; aborting + rolling back
  | "done";          // pool finished (success or fully cancelled)

interface Task extends PreviewFile {
  id: string;
  status: TaskStatus;
  loaded: number;
  error?: string;
  handle?: UploadHandle;
}

/**
 * Browser folder picker + recursive walk + gitignore/regex filtering + parallel uploads.
 *
 * Two ways the browser lets us "pick a folder":
 *   - `<input type="file" webkitdirectory>` — fills File.webkitRelativePath
 *   - Drag-and-drop with the DataTransferItem.webkitGetAsEntry API
 * We support both.
 */
export function FolderUpload({ bucket, prefix = "", fileConcurrency = 3, onComplete }: Props) {
  const [open, setOpen] = useState(false);
  const [showSettings, setShowSettings] = useState(false);
  const [picked, setPicked] = useState<PreviewFile[]>([]);
  const [patterns, setPatterns] = useState<PatternConfig>(DEFAULT_PATTERNS);
  const [scanning, setScanning] = useState(false);
  // Per-file user overrides — relPath → forced decision.
  // Overrides pattern matcher for that one file.
  const [overrides, setOverrides] = useState<Map<string, "include" | "exclude">>(new Map());
  const [tasks, setTasks] = useState<Task[]>([]);
  const [phase, setPhase] = useState<Phase>("idle");
  const [dragOver, setDragOver] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  // Refs the worker pool reads to stop accepting new work mid-stream.
  // setState wouldn't work — workers capture stale state via closure.
  const cancelledRef = useRef(false);
  const tasksRef = useRef<Task[]>([]);
  useEffect(() => { tasksRef.current = tasks; }, [tasks]);

  useEffect(() => { setPatterns(loadPatterns(bucket)); }, [bucket]);

  const matcher = useMemo(() => compileMatcher(patterns), [patterns]);

  const { included, excluded } = useMemo(() => {
    const inc: PreviewFile[] = [];
    const exc: PreviewFile[] = [];
    for (const f of picked) {
      const ov = overrides.get(f.relPath);
      const patternsExclude = matcher(f.relPath);
      let isExcluded: boolean;
      if (ov === "include")      isExcluded = false;
      else if (ov === "exclude") isExcluded = true;
      else                       isExcluded = patternsExclude;
      if (isExcluded) exc.push(f);
      else            inc.push(f);
    }
    return { included: inc, excluded: exc };
  }, [picked, matcher, overrides]);

  // Toggle a single file's inclusion. Cycles: pattern-default → forced-opposite.
  // Clicking again clears the override (returns to pattern-default).
  const toggleFile = (path: string) => {
    setOverrides((prev) => {
      const next = new Map(prev);
      const ov = next.get(path);
      const patternsExclude = matcher(path);
      // Goal: flip current visual state. If it's currently included, force exclude.
      // If currently excluded, force include. Clicking again clears.
      const currentlyExcluded = ov === "exclude" || (ov === undefined && patternsExclude);
      if (ov) {
        next.delete(path); // restore pattern default
      } else {
        next.set(path, currentlyExcluded ? "include" : "exclude");
      }
      return next;
    });
  };

  const clearAllOverrides = () => setOverrides(new Map());

  const includedBytes = useMemo(
    () => included.reduce((sum, f) => sum + f.file.size, 0),
    [included],
  );
  const excludedBytes = useMemo(
    () => excluded.reduce((sum, f) => sum + f.file.size, 0),
    [excluded],
  );

  // ---------- folder selection ----------

  const onPickerChange = async (e: ChangeEvent<HTMLInputElement>) => {
    const list = e.target.files;
    if (!list) return;
    setScanning(true);
    setOverrides(new Map());
    setTasks([]);
    // Yield to the browser so the spinner paints before we block.
    await new Promise((r) => setTimeout(r, 0));
    const files: PreviewFile[] = [];
    // Read in chunks so the UI can paint progress on huge folders.
    const CHUNK = 5000;
    for (let i = 0; i < list.length; i++) {
      const f = list[i];
      const rel = (f as any).webkitRelativePath || f.name;
      files.push({ file: f, relPath: rel });
      if (i % CHUNK === 0 && i > 0) {
        await new Promise((r) => setTimeout(r, 0));
      }
    }
    setPicked(files);
    setScanning(false);
    e.target.value = "";
  };

  const onDrop = async (e: React.DragEvent) => {
    e.preventDefault();
    setDragOver(false);
    const items = Array.from(e.dataTransfer.items).filter(
      (i) => i.kind === "file" && typeof i.webkitGetAsEntry === "function",
    );
    if (items.length === 0) return;
    setScanning(true);
    setOverrides(new Map());
    setTasks([]);
    await new Promise((r) => setTimeout(r, 0));   // paint spinner first
    const files: PreviewFile[] = [];
    for (const item of items) {
      const entry = (item as any).webkitGetAsEntry();
      if (entry) await walkEntry(entry, "", files);
    }
    setPicked(files);
    setScanning(false);
  };

  // ---------- upload ----------

  const startUpload = () => {
    if (phase === "uploading" || phase === "cancelling" || included.length === 0) return;

    cancelledRef.current = false;
    const initial: Task[] = included.map((f) => ({
      ...f,
      id: crypto.randomUUID(),
      status: "queued",
      loaded: 0,
    }));
    setTasks(initial);
    setPhase("uploading");

    // Promise pool — N files in parallel; each file may use multipart with 4-way
    // internal concurrency. Worker bails immediately if cancelledRef flips.
    let cursor = 0;
    const workers = Array.from({ length: fileConcurrency }, async () => {
      while (!cancelledRef.current) {
        const idx = cursor++;
        if (idx >= initial.length) return;
        const t = initial[idx];
        const key = (prefix ? prefix.replace(/\/$/, "") + "/" : "") + t.relPath;

        const handle = uploadFile(bucket, key, t.file, (loaded) => {
          setTasks((prev) => prev.map((x) => x.id === t.id ? { ...x, loaded } : x));
        });
        setTasks((prev) => prev.map((x) => x.id === t.id
          ? { ...x, status: "uploading", handle } : x));

        try {
          await handle.promise;
          // Only mark done if not cancelled while we were waiting.
          if (cancelledRef.current) {
            setTasks((prev) => prev.map((x) => x.id === t.id
              ? { ...x, status: "cancelled" } : x));
          } else {
            setTasks((prev) => prev.map((x) => x.id === t.id
              ? { ...x, status: "done", loaded: t.file.size } : x));
          }
        } catch (e: unknown) {
          const isAbort = e instanceof DOMException && e.name === "AbortError";
          setTasks((prev) => prev.map((x) => x.id === t.id ? {
            ...x,
            status: isAbort ? "cancelled" : "error",
            error: isAbort ? undefined : (e instanceof Error ? e.message : String(e)),
          } : x));
        }
      }

      // While cancelled, mark any leftover queued tasks
      setTasks((prev) => prev.map((x) =>
        x.status === "queued" ? { ...x, status: "cancelled" } : x));
    });

    void Promise.all(workers).then(() => {
      if (!cancelledRef.current) {
        setPhase("done");
        onComplete?.();
      }
    });
  };

  /**
   * Cancel-all does TWO things:
   *   1. Halts new uploads + aborts in-flight ones (their server-side
   *      multipart sessions also get aborted via the existing handle.abort()).
   *   2. ROLLS BACK already-completed files by DELETE-ing them server-side,
   *      one at a time. The progress bar visibly shrinks as each comes off
   *      (we set loaded=0 per file once it's gone).
   */
  const cancelAll = async () => {
    if (phase !== "uploading") return;
    cancelledRef.current = true;
    setPhase("cancelling");

    // Step 1: kill in-flight transfers + mark queued cancelled.
    const live = tasksRef.current;
    for (const t of live) {
      if (t.handle && t.status === "uploading") {
        try { t.handle.abort(); } catch { /* ignore */ }
      }
    }
    setTasks((prev) => prev.map((t) =>
      t.status === "queued" ? { ...t, status: "cancelled" } : t,
    ));

    // Wait briefly for in-flight to settle (handle.promise rejects with AbortError)
    await new Promise((r) => setTimeout(r, 200));

    // Step 2: roll back the "done" ones — DELETE each, set loaded back to 0
    // so the aggregate progress bar visibly rewinds.
    const completed = tasksRef.current.filter((t) => t.status === "done");

    for (const t of completed) {
      // mark rolling back
      setTasks((prev) => prev.map((x) => x.id === t.id
        ? { ...x, status: "rolling_back" } : x));

      const key = (prefix ? prefix.replace(/\/$/, "") + "/" : "") + t.relPath;
      try {
        await api(`/${bucket}/${encodeKeyPath(key)}`, { method: "DELETE" });
      } catch {
        // Best-effort — if the object was already gone, that's fine.
      }

      setTasks((prev) => prev.map((x) => x.id === t.id
        ? { ...x, status: "rolled_back", loaded: 0 } : x));
    }

    setPhase("done");
    onComplete?.();
  };

  const done = tasks.filter((t) => t.status === "done").length;
  const errored = tasks.filter((t) => t.status === "error").length;
  const cancelled = tasks.filter((t) => t.status === "cancelled").length;
  const rolledBack = tasks.filter((t) => t.status === "rolled_back").length;
  const rollingBack = tasks.filter((t) => t.status === "rolling_back").length;
  const totalLoaded = tasks.reduce((s, t) => s + t.loaded, 0);

  // ---------- render ----------

  if (!open) {
    return (
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="group flex items-start gap-3 p-3 w-full text-left border border-border rounded-md
                   bg-bg hover:bg-card hover:border-accent transition-colors"
      >
        <FolderUp size={18} className="shrink-0 mt-0.5 text-accent" />
        <div className="min-w-0">
          <div className="text-sm font-semibold">Upload a folder</div>
          <div className="text-[11px] text-muted leading-snug">
            Recursive picker with <span className="font-mono">.gitignore</span>-style filters.
          </div>
        </div>
      </button>
    );
  }

  return (
    <div className="space-y-3 border border-border rounded p-3 bg-panel/50">
      <div className="flex items-center justify-between">
        <p className="text-xs font-semibold">Upload folder</p>
        <div className="flex items-center gap-3">
          <button onClick={() => setShowSettings(!showSettings)}
                  className="text-xs text-muted hover:text-text inline-flex items-center gap-1">
            <Settings2 size={12} /> Patterns
          </button>
          <button onClick={() => { setOpen(false); setPicked([]); setTasks([]); }}
                  className="text-xs text-muted hover:text-text">cancel</button>
        </div>
      </div>

      {showSettings && (
        <PatternsEditor
          patterns={patterns}
          onChange={(p) => { setPatterns(p); savePatterns(bucket, p); }}
          onReset={() => { setPatterns(DEFAULT_PATTERNS); savePatterns(bucket, DEFAULT_PATTERNS); }}
        />
      )}

      {/* Picker */}
      {picked.length === 0 && !scanning && (
        <div
          onDragOver={(e) => { e.preventDefault(); setDragOver(true); }}
          onDragLeave={() => setDragOver(false)}
          onDrop={onDrop}
          onClick={() => inputRef.current?.click()}
          className={`border-2 border-dashed rounded p-6 cursor-pointer transition text-center
            ${dragOver ? "border-accent bg-blue-950/30" : "border-border hover:border-muted"}`}
        >
          <FolderUp className="mx-auto mb-2 text-muted" size={24} />
          <p className="text-sm font-medium">Drop a folder here or click to browse</p>
          <p className="text-[11px] text-muted mt-1">
            Recursively walks; filters applied before any upload starts
          </p>
          <input
            ref={inputRef}
            type="file"
            // @ts-expect-error — non-standard but widely supported
            webkitdirectory=""
            directory=""
            multiple
            className="hidden"
            onChange={onPickerChange}
          />
        </div>
      )}

      {/* Scanning indicator — keeps the user oriented while we walk a huge folder */}
      {scanning && (
        <div className="border border-border rounded p-6 text-center">
          <Loader2 className="mx-auto mb-2 text-accent animate-spin" size={24} />
          <p className="text-sm font-medium">Scanning folder...</p>
          <p className="text-[11px] text-muted mt-1">
            Reading file metadata. Big folders (10k+ files) can take a few seconds.
          </p>
        </div>
      )}

      {/* Preview */}
      {picked.length > 0 && phase === "idle" && tasks.length === 0 && (
        <div className="space-y-2">
          <div className="flex items-center justify-between bg-bg p-2 rounded text-xs">
            <span>
              <span className="text-success">✓ {included.length} files ({formatBytes(includedBytes)})</span>
              {excluded.length > 0 && (
                <>  ·  <span className="text-muted">{excluded.length} ignored ({formatBytes(excludedBytes)})</span></>
              )}
              {overrides.size > 0 && (
                <>  ·  <span className="text-warning">{overrides.size} manual override(s) <button onClick={clearAllOverrides} className="underline hover:text-text">clear</button></span></>
              )}
            </span>
            <button onClick={() => { setPicked([]); setOverrides(new Map()); }}
                    className="text-muted hover:text-text">change folder</button>
          </div>

          {excluded.length > 0 && (
            <PreviewList
              title={`Ignored (${excluded.length})`}
              files={excluded}
              kind="excluded"
              overrides={overrides}
              onToggle={toggleFile}
              matcher={matcher}
            />
          )}

          <PreviewList
            title={`To upload (${included.length})`}
            files={included}
            kind="included"
            overrides={overrides}
            onToggle={toggleFile}
            matcher={matcher}
          />

          <Button onClick={startUpload} disabled={included.length === 0} size="sm">
            Upload {included.length} files
          </Button>
        </div>
      )}

      {/* Progress */}
      {tasks.length > 0 && (
        <div className="space-y-2">
          <div className="flex items-center justify-between text-xs">
            <span>
              {phase === "cancelling" ? (
                <>
                  <span className="text-warning inline-flex items-center gap-1">
                    <RotateCcw size={11} className="animate-spin" /> Rolling back
                  </span>
                  {" "}{rolledBack + rollingBack}/{done + rollingBack + rolledBack} undone
                </>
              ) : (
                <>
                  <span className="text-success">{done}/{tasks.length} done</span>
                  {errored > 0 && <>  ·  <span className="text-danger">{errored} failed</span></>}
                  {cancelled > 0 && <>  ·  <span className="text-muted">{cancelled} cancelled</span></>}
                  {rolledBack > 0 && <>  ·  <span className="text-muted">{rolledBack} rolled back</span></>}
                </>
              )}
              {"  ·  "}{formatBytes(totalLoaded)} / {formatBytes(includedBytes)}
            </span>
            {phase === "uploading" && (
              <button onClick={cancelAll}
                      className="text-xs text-muted hover:text-danger inline-flex items-center gap-1">
                <X size={12} /> Cancel all
              </button>
            )}
            {phase === "cancelling" && (
              <span className="text-[11px] text-muted">
                Aborting uploads + deleting already-uploaded files...
              </span>
            )}
          </div>
          {/* Bar color flips amber during rollback; transition-all gives the
              shrink animation as each rolled-back file zeros its loaded bytes. */}
          <div className="h-1 bg-border rounded overflow-hidden">
            <div
              className={`h-full transition-all duration-500 ease-out ${
                phase === "cancelling" ? "bg-warning" : "bg-accent"
              }`}
              style={{ width: `${includedBytes > 0 ? (totalLoaded / includedBytes) * 100 : 0}%` }}
            />
          </div>

          <details className="text-[11px]">
            <summary className="text-muted cursor-pointer hover:text-text">
              Per-file detail
            </summary>
            <ul className="mt-1 pl-4 max-h-60 overflow-y-auto space-y-1 font-mono">
              {tasks.map((t) => (
                <li key={t.id}
                    className={`flex items-center gap-2 text-[11px] transition-opacity ${
                      t.status === "rolled_back" ? "opacity-40 line-through" : ""
                    }`}>
                  {t.status === "done"         && <CheckCircle2 size={11} className="text-success shrink-0" />}
                  {t.status === "error"        && <XCircle      size={11} className="text-danger shrink-0" />}
                  {t.status === "cancelled"    && <XCircle      size={11} className="text-muted shrink-0" />}
                  {t.status === "uploading"    && <Loader2      size={11} className="text-accent animate-spin shrink-0" />}
                  {t.status === "rolling_back" && <RotateCcw    size={11} className="text-warning animate-spin shrink-0" />}
                  {t.status === "rolled_back"  && <RotateCcw    size={11} className="text-muted shrink-0" />}
                  {t.status === "queued"       && <span className="w-[11px]" />}
                  <span className="truncate flex-1" title={t.relPath}>{t.relPath}</span>
                  <span className="text-muted whitespace-nowrap">{formatBytes(t.file.size)}</span>
                  {t.error && <span className="text-danger text-[10px] truncate max-w-[120px]" title={t.error}>{t.error}</span>}
                </li>
              ))}
            </ul>
          </details>
        </div>
      )}
    </div>
  );
}

// ---------- Pattern editor ------------------------------------------------

function PatternsEditor({
  patterns, onChange, onReset,
}: { patterns: PatternConfig; onChange: (p: PatternConfig) => void; onReset: () => void }) {
  const regexErrors = useMemo(() => validateRegex(patterns.regex), [patterns.regex]);

  return (
    <div className="bg-bg border border-border rounded p-3 space-y-3">
      <div className="flex items-center justify-between">
        <p className="text-xs text-muted">
          Patterns are saved per-bucket in your browser. They run BEFORE any upload.
        </p>
        <button onClick={onReset}
                className="text-[11px] text-accent hover:underline">Reset to defaults</button>
      </div>

      <div>
        <label className="text-[10px] text-muted uppercase">.gitignore-style patterns</label>
        <textarea
          value={patterns.gitignore}
          onChange={(e) => onChange({ ...patterns, gitignore: e.target.value })}
          className="w-full h-40 px-3 py-2 bg-panel border border-border rounded text-xs font-mono"
          spellCheck={false}
        />
        <p className="text-[11px] text-muted mt-1">
          Standard <code>.gitignore</code> rules: <code>*.log</code>,
          {" "}<code>node_modules/</code>, <code>**/.DS_Store</code>,
          {" "}<code>!important.log</code> to re-include, <code>#</code> for comments.
        </p>
      </div>

      <div>
        <label className="text-[10px] text-muted uppercase">Regex patterns (one per line, optional)</label>
        <textarea
          value={patterns.regex}
          onChange={(e) => onChange({ ...patterns, regex: e.target.value })}
          className="w-full h-20 px-3 py-2 bg-panel border border-border rounded text-xs font-mono"
          placeholder={'# Examples\n^backup_\\d+\\.tmp$\n/^old-.*\\.log$/i'}
          spellCheck={false}
        />
        <p className="text-[11px] text-muted mt-1">
          Wrap with <code>/.../flags</code> for flags (e.g. <code>/foo/i</code>),
          or just write a pattern. Tested against the file&apos;s path relative to the picked folder.
        </p>
        {regexErrors.length > 0 && (
          <p className="text-[11px] text-danger mt-1">
            {regexErrors.length} invalid regex line(s): {regexErrors.map((e) => `line ${e.line}`).join(", ")}
          </p>
        )}
      </div>
    </div>
  );
}

// ---------- helpers -------------------------------------------------------

function encodeKeyPath(k: string): string {
  return k.split("/").map(encodeURIComponent).join("/");
}

interface PreviewListProps {
  title: string;
  files: PreviewFile[];
  kind: "included" | "excluded";
  overrides: Map<string, "include" | "exclude">;
  onToggle: (path: string) => void;
  matcher: (path: string) => boolean;
}

/**
 * Scrollable list with a "move to other list" button per row.
 * Renders only the first 200 items but keeps the count accurate.
 */
function PreviewList({ title, files, kind, overrides, onToggle, matcher }: PreviewListProps) {
  return (
    <details className="text-[11px]" open={files.length <= 30}>
      <summary className="text-muted cursor-pointer hover:text-text select-none">
        {title}
      </summary>
      <ul className="mt-1 max-h-60 overflow-y-auto divide-y divide-border/50 font-mono border border-border/40 rounded">
        {files.slice(0, 200).map((f, i) => {
          const ov = overrides.get(f.relPath);
          const patternsExclude = matcher(f.relPath);
          // Reason for the row's current placement (helps the user understand
          // when a pattern would exclude something but they're overriding it).
          const reason = ov
            ? (ov === "include" ? "forced include" : "forced exclude")
            : (patternsExclude ? "matches a pattern" : "not matched");

          return (
            <li
              key={i}
              className={`flex items-center gap-2 px-2 py-1 ${
                kind === "excluded" ? "text-muted" : ""
              }`}
            >
              <span className="truncate flex-1" title={`${f.relPath}\n${reason}`}>
                {f.relPath}
              </span>
              <span className="text-[10px] text-muted whitespace-nowrap">
                {formatBytes(f.file.size)}
              </span>
              <button
                onClick={() => onToggle(f.relPath)}
                className={`shrink-0 px-1.5 py-0.5 rounded text-[10px] border transition ${
                  kind === "included"
                    ? "border-border hover:border-warning hover:text-warning"
                    : "border-border hover:border-success hover:text-success"
                }`}
                title={
                  kind === "included"
                    ? "Move to ignored — won't be uploaded"
                    : "Move to upload — will be uploaded despite pattern"
                }
              >
                {kind === "included"
                  ? <span className="flex items-center gap-0.5"><ArrowDown size={9} />ignore</span>
                  : <span className="flex items-center gap-0.5"><ArrowUp   size={9} />include</span>}
              </button>
            </li>
          );
        })}
        {files.length > 200 && (
          <li className="px-2 py-1 text-muted text-center">
            ... and {files.length - 200} more (use patterns to handle this many)
          </li>
        )}
      </ul>
    </details>
  );
}

async function walkEntry(entry: any, prefix: string, out: PreviewFile[]): Promise<void> {
  if (entry.isFile) {
    await new Promise<void>((resolve) => {
      entry.file((file: File) => {
        out.push({ file, relPath: prefix + file.name });
        resolve();
      });
    });
    return;
  }
  if (entry.isDirectory) {
    const reader = entry.createReader();
    let entries: any[];
    do {
      entries = await new Promise<any[]>((r) => reader.readEntries(r));
      for (const child of entries) {
        await walkEntry(child, prefix + entry.name + "/", out);
      }
    } while (entries.length > 0);
  }
}
