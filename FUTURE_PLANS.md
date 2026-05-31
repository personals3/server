# Future Plans

Rolling list of things we've decided to do but haven't. Big features at the
top, small maintenance at the bottom. Edit freely — this is a working doc,
not a contract.

---

## Client-side transcoding ("commodity borrower")

**Why.** The worker container is currently the single most expensive thing
in the stack. Every uploaded video gets transcoded server-side to 3 HLS
rungs + thumbnails + audio fallbacks — at scale this means a server-grade
GPU or a long CPU queue. Meanwhile every user's laptop / phone already has
a GPU that's idle 90% of the time. If the upload page transcodes locally
before sending, the server stops being the bottleneck.

**Mechanism.** WebAssembly, not native binaries.
- `ffmpeg.wasm` is mature and runs in every browser, no install
- Modern browsers also expose `WebCodecs` for hardware-accelerated encode
  via the device's GPU (Chrome / Edge / Safari)
- The existing multipart upload API can accept the resulting HLS segments
  verbatim — no protocol changes needed, the server just stores them

**Tradeoffs we get:**
- Server load: drops drastically for the transcode pipeline
- Time-to-first-playable: faster for the user (no server queue wait)
- Per-user cost: their CPU, their battery, their bandwidth (transcoded
  output is *larger* than the source — multiple rungs)

**Open questions to decide before building:**

1. **Trust model** — can we trust user-transcoded output as canonical?
   - a) Spot-check N random segments server-side
   - b) Re-transcode in the background anyway (canonical = server-side,
        client output is just "first playable")
   - c) Trust completely; mark `transcode_origin = client`
   - For friends-and-family scale, (c) is fine. Wider audience → (a).

2. **Fallback heuristic** — old phones, low-end browsers will transcode
   slower than the server. Need a quick capability probe at upload start:
   measure ~5 sec of encode throughput, compare to server queue ETA,
   pick the faster path. User-visible toggle.

3. **Network cost** — output is larger than input. On metered connections
   this is brutal. Opt-in by default; auto-detect slow networks and offer
   to fall back to "ship raw, server transcodes."

4. **Resume semantics** — user closes the tab mid-transcode. Either:
   - Commit segments as we produce them (recoverable, more API calls)
   - Treat abandoned client-side jobs as lost work; user re-uploads

5. **New object lifecycle** — `transcode_status` gains a `client_provided`
   value. May want to track `transcode_origin = client | worker | none`
   separately for analytics + verification.

**Sequencing.** Defer until v0.1 has real users and we've actually seen
the worker get backlogged. Premature otherwise.

---

## Off-disk backups (restic → B2)

**Why.** Right now a single disk loss = all user data gone. Capacity caps
don't help that. We deferred this when launching v0.1 because paid storage
was off the table, but ~$6/TB/month at Backblaze B2 is the cheapest serious
option.

**Plan when we revisit.**
- `restic` repo to Backblaze B2 (encrypted, deduplicated, off-site)
- Nightly: `pg_dump` + restic snapshot of `/srv/personals3/storage`
- Retention: 7 daily / 4 weekly / 6 monthly
- Restore-drill `scripts/test-backup-restore.sh` already exists — use it
  monthly so we know a real restore works
- Repository password lives in `.env` as `RESTIC_PASSWORD`, also in a
  password manager (losing it = losing the backup)

**Open:** B2 vs Cloudflare R2 (free egress for backups, but slower
restore). Tilburg-class tradeoff; decide when we sign up.

---

## Domain migration off `abckvault.online`

**Why.** `abckvault.online` expires **2026-11-12** and the plan is to
not renew. Currently both the SMTP `From` / `Reply-To` and earlier
references in older code/docs still touch the old domain.

**Status of personals3.tech adoption.**
- ✅ Dashboard / API live at `personals3.tech` (Cloudflare Tunnel)
- ✅ Docs at `developers.personals3.tech` (Cloudflare Pages)
- ✅ Email routing: `support@personals3.tech` → Gmail
- ✅ SMTP sender: `noreply@personals3.tech` via Brevo, DKIM verified
- ⚠️  No lingering references to `abckvault.online` in active code, but
  external links (Brevo dashboard, Cloudflare Email Routing rules, any
  saved bookmarks) still need cleanup

**Action items before Nov 12:**
- Delete the `abckvault.online` zone from Cloudflare once nothing depends on it
- Remove email routing rules tied to that domain
- Confirm Brevo no longer needs the old domain verified

**`personals3.tech` itself renews ~12 months from registration** —
calendar reminder for that too. If broke at renewal time, the
abhishek.me subdomain (`s3.abhishek.me`) is a free fallback.

---

## CI lint for unrooted `.gitignore` patterns

**Why.** We've hit the same bug **three times** this session:
- `storage/` rule silently excluded `api/internal/storage/`
- `ps3` rule silently excluded `cli/cmd/ps3/`
- `logs/` rule silently excluded `dashboard/app/dashboard/admin/logs/`

Each one produced a confusing "page exists locally but 404s in
production" or "go build can't find main file" failure. None caught
in code review.

**Plan.** Add a tiny check to `scripts/`:

```bash
# scripts/check-gitignore.sh
# Warns about .gitignore entries without a leading slash that could
# accidentally match nested files.
grep -nE '^[a-z][a-z_-]+/$' .gitignore | while read line; do
  echo "WARN: $line should probably be /<path>/ to anchor to root"
done
```

Wire into a pre-commit hook or a GitHub Actions step on PR. Optional
but cheap insurance.

---

## SMTP key rotation

**Why.** The Brevo SMTP password was shared in chat transcripts
multiple times during the initial setup. User said they'd rotate at
the end. Now is the end.

**Plan.**
1. Brevo dashboard → SMTP & API → rotate the SMTP key
2. Update `SMTP_PASS` in `/opt/personals3/.env`
3. `docker compose up -d api` (worker reads SMTP via env, doesn't need rebuild)
4. Send a test email through `/api/auth/request-account` to confirm

Should take ~5 minutes. Do this before any wider invite.

---

## Caddy removal from docker-compose

**Why.** Cloudflare Tunnel handles all public TLS now. The `caddy/`
service in docker-compose is dead weight — it used to be the
public-facing reverse proxy but the named tunnel goes direct to nginx.

**Plan.**
- Remove the `caddy` service block from `docker-compose.yml`
- Delete `caddy/` directory
- Update `docs/deployment.md` to drop the "with Caddy in front" branch
  (already partially done, finish it)

Risk: zero. The container isn't started in normal compose runs (it was
behind a profile). Just deletion + docs.

---

## Scrub the 170 MB postgres dump from `server` repo history

**Why.** The initial commit (`a092531`) to `personals3/server` included
`backups/2026-05-30T15-27-59Z/postgres.dump.gz` — 170 MB of test dump
that we've since `git rm --cached`'d. The blob still exists in git
history, taking up space in every clone forever.

The repo is private so the data isn't publicly leaked, but:
- Cloning is slower than it should be
- The dump may contain hashed passwords + user emails from dev DB

**Plan.** One-time `git filter-repo` (or `git-filter-branch` if
filter-repo isn't installed):

```bash
git filter-repo --strip-blobs-bigger-than 10M
git push origin main --force-with-lease
```

Caveats:
- Rewrites every commit hash → existing clones diverge
- Anyone with the repo cloned needs to re-clone (it's just you)
- Must do this when nothing else is in flight (no pending PRs, etc.)

Low priority. Do it next time you're already doing a force-push for
some other reason.

---

## Delete unused in-app `/dashboard/docs/*` route

**Why.** The dashboard has a `/dashboard/docs` Next.js page that
renders markdown from the `ps3-docs` bucket via the API's `DocsHandler`.
We migrated to a separate `developers.personals3.tech` site and unlinked
this route from the nav. But the page + handler still exist on disk and
in code.

**Status:** harmless — nothing links to it, but it adds maintenance
surface and confusion for future readers.

**Plan when we revisit:**
- `rm -rf dashboard/app/dashboard/docs/` (the Next.js page)
- Remove `DocsHandler` routes from `api/cmd/server/main.go`
- Delete `api/internal/handlers/docs.go`
- Drop the `ps3-docs` bucket from production DB (it's been replaced
  by Cloudflare Pages serving from git)
- Drop `scripts/seed-docs.sh` (no longer needed)

About 15 minutes of removals. Do alongside another cleanup PR.

---

## Things considered and rejected

Recording these so we don't re-litigate the same questions.

- **Static-site generator other than Astro Starlight** (VitePress,
  Docusaurus, MkDocs Material). Starlight won on out-of-box quality
  and tight match with the existing Next/React toolchain.
- **Hosting docs on the main stack** instead of a separate Cloudflare
  Pages site. Rejected because: server downtime would also kill the
  docs (the thing users go to when something's broken). Pages is free
  and decoupled.
- **Bundling docs inside the CLI repo**. Considered briefly because
  the CLI is the only public-facing repo. Rejected: docs change much
  more often than the CLI; coupling release cycles wastes goreleaser
  runs.
- **301 instead of 302 for the `/install` redirect**. Rejected because
  301 is aggressively cached and we want the freedom to move the
  install.sh host later without breaking already-installed clients.
