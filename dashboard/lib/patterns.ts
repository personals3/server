// Folder-upload exclusion engine.
//
// Two pattern types, both optional, both combined:
//   1. gitignore-style globs  — uses the battle-tested `ignore` library
//   2. JavaScript regex       — one per line, optional / surrounding slashes
//
// All filtering happens in the browser BEFORE upload — saves bandwidth and
// gives the user instant feedback about what will/won't be sent.

import ignore from "ignore";

export interface PatternConfig {
  /** gitignore-style patterns, one per line */
  gitignore: string;
  /** JS regex patterns, one per line. Optionally wrapped in /.../flags */
  regex: string;
}

/** Reasonable defaults — covers the universally annoying files. */
export const DEFAULT_PATTERNS: PatternConfig = {
  gitignore: `# Universally noisy files — drop the defaults if you don't want them
.DS_Store
Thumbs.db
desktop.ini
*.tmp
*.swp
*.swo
*~
*.bak

# Build artifacts
node_modules/
.next/
.nuxt/
.cache/
dist/
build/
target/
__pycache__/
*.pyc
.venv/
venv/

# Version control / editors
.git/
.svn/
.hg/
.idea/
.vscode/
*.iml

# OS metadata
$RECYCLE.BIN/
System Volume Information/
`,
  regex: ``,
};

/** Storage key for per-bucket pattern overrides. */
export function patternsStorageKey(bucket: string): string {
  return `ps3_patterns_${bucket}`;
}

export function loadPatterns(bucket: string): PatternConfig {
  if (typeof window === "undefined") return DEFAULT_PATTERNS;
  const raw = localStorage.getItem(patternsStorageKey(bucket));
  if (!raw) return DEFAULT_PATTERNS;
  try {
    const parsed = JSON.parse(raw);
    return {
      gitignore: typeof parsed.gitignore === "string" ? parsed.gitignore : DEFAULT_PATTERNS.gitignore,
      regex: typeof parsed.regex === "string" ? parsed.regex : DEFAULT_PATTERNS.regex,
    };
  } catch {
    return DEFAULT_PATTERNS;
  }
}

export function savePatterns(bucket: string, p: PatternConfig) {
  if (typeof window === "undefined") return;
  localStorage.setItem(patternsStorageKey(bucket), JSON.stringify(p));
}

/**
 * Compiles a PatternConfig into a fast `(path) => boolean` matcher.
 * Returns true when the path should be EXCLUDED.
 */
export function compileMatcher(p: PatternConfig): (path: string) => boolean {
  const ig = ignore().add(splitNonEmpty(p.gitignore));

  const regexes: RegExp[] = [];
  for (const line of splitNonEmpty(p.regex)) {
    try {
      regexes.push(parseRegexLine(line));
    } catch {
      // skip bad regex lines silently; the UI shows a hint
    }
  }

  return (path: string) => {
    if (ig.ignores(path)) return true;
    for (const r of regexes) {
      if (r.test(path)) return true;
    }
    return false;
  };
}

/** Returns parsed RegExp from a line like `/foo/i` or just `foo` (treated literal-RE). */
function parseRegexLine(line: string): RegExp {
  line = line.trim();
  if (line.startsWith("/")) {
    const close = line.lastIndexOf("/");
    if (close > 0) {
      const body = line.slice(1, close);
      const flags = line.slice(close + 1);
      return new RegExp(body, flags);
    }
  }
  return new RegExp(line);
}

function splitNonEmpty(s: string): string[] {
  return s.split(/\r?\n/)
    .map((l) => l.trim())
    .filter((l) => l && !l.startsWith("#"));
}

/** Quick sanity-check used by the UI to flag malformed regex lines. */
export function validateRegex(text: string): { line: number; error: string }[] {
  const errors: { line: number; error: string }[] = [];
  text.split(/\r?\n/).forEach((line, i) => {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith("#")) return;
    try {
      parseRegexLine(trimmed);
    } catch (e) {
      errors.push({ line: i + 1, error: e instanceof Error ? e.message : "invalid" });
    }
  });
  return errors;
}
