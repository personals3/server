#!/usr/bin/env bash
# check-gitignore.sh — warn about .gitignore entries that aren't anchored
# to the repo root and could silently match nested files.
#
# We've been bitten three times: `storage/` excluded api/internal/storage/,
# `ps3` excluded cli/cmd/ps3/, `logs/` excluded a dashboard route dir. Each
# produced a confusing "exists locally but missing from git" failure.
#
# Only bare names ([A-Za-z0-9_-], optional trailing slash) are flagged —
# that's the foot-gun shape. Rooted (`/x`), negation (`!x`), glob (`*x`,
# `**/x`), pathed (`a/b`), and dotfile (`.env`, `.DS_Store`) patterns are
# all either anchored already or unanchored on purpose.
#
# Exits 1 if any suspect line is found, so it can gate a pre-commit hook
# or CI step. Usage: scripts/check-gitignore.sh [path-to-.gitignore]

set -euo pipefail

file="${1:-.gitignore}"
[ -f "$file" ] || { echo "check-gitignore: $file not found" >&2; exit 2; }

warnings=0
lineno=0
while IFS= read -r line; do
  lineno=$((lineno + 1))
  # Warn only on bare names like `storage/`, `logs/`, `ps3` — everything
  # else (rooted, negation, glob, pathed, dotfile) passes through.
  body="${line%/}"
  case "$body" in
    *[!A-Za-z0-9_-]*|"") continue ;;
  esac
  echo "WARN: $file:$lineno: '$line' is unanchored — matches at ANY depth." \
       "Use '/$line' to anchor it to the repo root (or '**/$line' if any-depth is intended)."
  warnings=$((warnings + 1))
done < "$file"

if [ "$warnings" -gt 0 ]; then
  echo "check-gitignore: $warnings unanchored pattern(s) found in $file" >&2
  exit 1
fi
echo "check-gitignore: $file OK"
