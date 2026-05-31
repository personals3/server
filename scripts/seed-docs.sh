#!/usr/bin/env bash
# Populate the ps3-docs bucket with the docs/ tree, then mark it public.
# Idempotent — re-run any time you edit a .md file. Re-uploads only
# overwrite the affected key; the dashboard's /dashboard/docs page polls
# the listing and fetches each doc from the public URL.
#
# Requires `ps3` on PATH (or set PS3=/path/to/ps3) and a logged-in session.

set -euo pipefail

PS3="${PS3:-ps3}"
BUCKET="${BUCKET:-ps3-docs}"
DOCS_DIR="${DOCS_DIR:-docs}"

cd "$(dirname "$0")/.."

if ! "$PS3" bucket list 2>/dev/null | grep -qx "$BUCKET"; then
  echo "creating bucket $BUCKET"
  "$PS3" bucket create "$BUCKET"
fi

echo "marking $BUCKET public (via DB — there's no CLI flag for this yet)"
docker compose exec -T postgres psql -U s3admin -d personals3 -c \
  "UPDATE buckets SET is_public = true WHERE name = '$BUCKET';" >/dev/null

echo "uploading every .md under $DOCS_DIR/"
find "$DOCS_DIR" -name '*.md' -print0 | while IFS= read -r -d '' f; do
  rel="${f#$DOCS_DIR/}"
  "$PS3" cp "$f" "$BUCKET/$rel"
done

echo ""
echo "done — open /dashboard/docs to browse"
