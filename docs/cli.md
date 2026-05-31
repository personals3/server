# `ps3` — command-line client

A native Go CLI for PersonalS3. Single binary, JWT auth, no SigV4 setup
required. Built specifically for the personal features the dashboard
exposes — folders, search, trash, versions, presigned URLs — so you don't
have to write S3 API calls in shell.

> If you just want a generic S3 client (e.g., for backups), `aws-cli` and
> `rclone` both work against the same server via S3 SigV4 — see
> [clients/aws-cli.md](./clients/aws-cli.md) and
> [clients/rclone.md](./clients/rclone.md). The `ps3` CLI is the
> first-class option for everything else.

---

## Install

```bash
cd cli
go install ./cmd/ps3
# adds $GOPATH/bin/ps3 — make sure $GOPATH/bin is in your PATH
```

Or clone-and-build if you don't have a Go toolchain:

```bash
cd cli && go build -o ps3 ./cmd/ps3
sudo mv ps3 /usr/local/bin/
```

## First-time auth

Three ways to authenticate, in increasing security-friendliness:

### Mode 1 — Email + password (interactive)

```bash
ps3 login --server http://localhost:8080 --email you@example.com
# password prompt is silent (terminal hides input)
# If your account has 2FA on, also prompts for the 6-digit code
```

### Mode 2 — API key (recommended for scripts + 2FA accounts)

In the dashboard, go to **API Keys**, click "Create key", copy it
(shown once). Then:

```bash
ps3 login --server http://localhost:8080 --token ps3_abc123...
```

Why prefer this:
- **Bypasses 2FA** — the key itself is the credential
- **Per-device** — revoke from one laptop without re-authenticating others
- **Long-lived** — survives password rotations
- **Visible in dashboard** — see exactly what's authenticated where

Mirrors the GitHub personal-access-token model.

### Mode 3 — Already logged in

```bash
ps3 login
# already logged in as you@example.com (admin) @ http://localhost:8080
# use `ps3 login --force` to re-authenticate, or `ps3 logout` first
```

`ps3 login` with no args is a no-op confirmation when a valid session
exists. Pass `--force` to overwrite anyway.

Credentials persist in `~/.ps3/config.json` (mode 0600). Token survives
across shells until you `ps3 logout` or the server-side expiry passes.

Verify:

```bash
ps3 whoami
# you@example.com (admin) — used 12.3 MB / quota 10.0 GB
# server: http://localhost:8080
```

---

## Commands

Every command supports `--help` for full flags.

### `ps3 ls [bucket[/prefix/]]`

List buckets or objects.

```bash
ps3 ls                                # list buckets
ps3 ls -l                             # long format (creation date, flags)
ps3 ls my-bucket                      # top-level of bucket (folders + files)
ps3 ls my-bucket/photos/              # contents of photos/
ps3 ls my-bucket --recursive          # flat listing of every object
ps3 ls my-bucket/photos/ -l           # with size + last-modified
```

Folder rows show as `photos/`, files show their full key. With `--long`,
buckets show their public/versioning/archived flags inline.

### `ps3 cp <src> <dst>`

Copy between local and remote. Direction inferred from which side looks
local vs. remote (presence of a file system entry, or `@` prefix to force).

```bash
ps3 cp ./cat.jpg my-bucket/photos/cat.jpg          # upload
ps3 cp my-bucket/photos/cat.jpg ./out.jpg          # download
ps3 cp my-bucket/photos/cat.jpg -                  # download to stdout
echo "hello" | ps3 cp - my-bucket/notes/hi.txt     # upload from stdin
ps3 cp ./video.mp4 my-bucket/v/video.mp4 \
    --content-type=video/mp4                       # override MIME
```

### `ps3 rm <bucket/key> [bucket/key ...]`

Default = trash (soft delete). `--purge` skips trash. `--recursive` treats
the target as a prefix and bulk-deletes everything under it.

```bash
ps3 rm my-bucket/old.txt                    # to trash
ps3 rm --purge my-bucket/old.txt            # permanent
ps3 rm -r my-bucket/photos/2019/            # bulk-trash entire folder
ps3 rm --purge my-bucket/file1 my-bucket/file2  # multiple at once
```

### `ps3 search [query]`

Cross-bucket search. Mirrors the dashboard's Search page.

```bash
ps3 search vacation                                # key contains "vacation"
ps3 search --bucket=photos --ext=jpg               # all JPGs in one bucket
ps3 search --type=image --min-size=1000000         # images > 1 MB
ps3 search --type=video                            # all videos
ps3 search "" --bucket=my-bucket --limit=500       # list 500 objects in a bucket
```

Output is TAB-separated, pipe-friendly:

```
my-bucket/photos/cat.jpg    24.3 KB    2026-05-29 21:00    image/jpeg
my-bucket/photos/dog.jpg     1.2 MB    2026-05-29 21:00    image/jpeg
```

### `ps3 share <bucket/key>`

Generate a presigned URL. Default is GET, valid 24h, max 30d.

```bash
ps3 share my-bucket/cat.jpg                           # GET, 24h
ps3 share --expires 1h my-bucket/cat.jpg              # GET, 1h
ps3 share --expires 7d my-bucket/cat.jpg --download   # forces Save-As
ps3 share --upload my-bucket/inbox/upload.bin         # PUT URL — anyone can write
                                                       # bytes to this exact key
```

The URL is printed to stdout (one line, pipeable). Method + expiry info
go to stderr.

### `ps3 trash [list|restore|purge|empty]`

```bash
ps3 trash                                        # implicit list
ps3 trash list
ps3 trash restore my-bucket/oops.txt             # bring back
ps3 trash purge my-bucket/old.bin                # permanent delete one
ps3 trash empty                                  # permanent delete ALL (asks)
ps3 trash empty --yes                            # skip confirm
```

### `ps3 bucket <subcommand>`

```bash
ps3 bucket list                                  # same as `ps3 ls`
ps3 bucket create archive --mode none            # plain bucket
ps3 bucket create media --mode media             # auto-transcode on upload
ps3 bucket patch archive --versioning --archived # update flags
ps3 bucket patch media --public                  # serve at /public/media/...
ps3 bucket delete archive                        # only if empty
ps3 bucket delete archive --force                # nuke everything inside
```

### Auth & config

```bash
ps3 whoami           # who you're logged in as
ps3 logout           # clear local token
```

Config file at `~/.ps3/config.json` (chmod 600):

```json
{
  "server": "http://localhost:8080",
  "token": "eyJhbG...",
  "email": "you@example.com",
  "defaultBucket": ""
}
```

You can edit it by hand or use `ps3 login` to reset.

---

## Common workflows

### Daily backup of a local folder to PersonalS3

```bash
ps3 bucket create backups --mode none
# upload one file:
ps3 cp ~/Documents/important.zip backups/important.zip

# upload every file recursively (until a sync command lands):
find ~/Documents -type f | while read f; do
  ps3 cp "$f" "backups/$(basename "$f")"
done
```

> A proper `ps3 sync` command (analog of `rsync` / `aws s3 sync`) is on
> the roadmap. For now, `rclone` against the S3 endpoint is the best
> option for true sync semantics — see [clients/rclone.md](./clients/rclone.md).

### Generate a one-shot upload URL and send it to someone

```bash
ps3 share --upload --expires 1h my-bucket/from-friends/$(date +%s).bin
# → http://...  (paste in chat)
```

They run:

```bash
curl -X PUT --data-binary @whatever.bin "<the URL>"
```

The file lands in your bucket and counts against YOUR quota (not theirs).

### Empty trash + see what was reclaimed

```bash
ps3 trash empty --yes
# purged 12 items, reclaimed 1.2 GB
```

### Find every video over 100 MB across all buckets

```bash
ps3 search "" --type=video --min-size=$((100*1024*1024)) --limit=500
```

### Pipe search results to bulk-delete

```bash
ps3 search vacation_2019 --bucket=photos --limit=500 \
  | cut -f1 | xargs -I{} ps3 rm "{}"
```

---

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | API error (e.g. 404, 403, network failure) |
| 2 | usage error (missing args, bad flag) |

Errors print the API's `code`+`message` to stderr, e.g.:

```
ps3: NO_SUCH_BUCKET: bucket not found (HTTP 404)
```

---

## Shell tab-completion

```bash
# Source on every shell start
echo 'eval "$(ps3 completion zsh)"'  >> ~/.zshrc
# or for bash:
echo 'eval "$(ps3 completion bash)"' >> ~/.bashrc
# or for fish:
ps3 completion fish > ~/.config/fish/completions/ps3.fish
```

Once installed, tab through buckets and keys without typing them:

```bash
ps3 ls m<TAB>                  → expands to "my-bucket/"
ps3 cp ~/cat.jpg my-bucket/p<TAB>  → completes from existing folders
ps3 share my-bucket/photos/cat<TAB>  → all keys starting with "cat"
```

Works on: `ls`, `cp`, `rm`, `share`, `cat`, `sync`. The completion script
calls `ps3 __complete-path <current>` under the hood, which queries the
server's bucket/object listing. **You must be logged in for tab-completion
to find anything** — it uses your saved token.

## What's NOT in the CLI yet

These features are dashboard-only today; PRs welcome:

- `ps3 mv` (rename / move between buckets)
- `ps3 versions <bucket/key>` + restore
- Multipart progress bar on big uploads (current upload is a single PUT)
- `ps3 import <url> <bucket/key>` for server-side fetches

For multipart workflows on very large files, [`rclone`](./clients/rclone.md)
remains the right tool — it speaks S3 SigV4 against the same endpoints and
handles 100GB+ uploads natively.
