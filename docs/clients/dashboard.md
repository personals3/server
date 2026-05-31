# Dashboard (Browser)

Open `http://localhost:8080` (or `https://s3.yourdomain.com` if you've
configured a tunnel).

## Pages

### `/login`
Email + password. Token stored in `localStorage` under `ps3_token`.
Survives reload, cleared on logout.

### `/dashboard` — Overview
Your storage usage bar + recent buckets.

### `/dashboard/buckets` — List & create buckets
- **Name rules:** 3–63 chars, lowercase letters/digits/dots/hyphens, must
  start and end with letter or digit. No underscores, no spaces.
- **Reserved names:** `admin`, `auth`, `healthz`, `stream`, `static`, `public`
- Deleting a bucket requires it to be empty.

### `/dashboard/buckets/{name}` — File browser
- **Upload:** drag-drop or click to browse. Files >8 MiB use multipart
  upload (5 MiB chunks, 4 parallel) with a progress bar.
- **Click a file** to preview:
  - Image: shows transcoded WebP (or original if transcoding hasn't finished)
  - Video: embedded video.js player with HLS adaptive bitrate
  - Audio: HTML5 audio element on the transcoded HLS stream
  - Other: download link
- **Transcode status indicator** next to media files: `pending` /
  `processing` / `done` / `failed`. Refresh to update.
- **Download** and **delete** buttons per object.

### `/dashboard/keys` — Credentials
Two sections, both create-once-show-once:

1. **Bearer API Keys** — for curl, scripts, the native API
2. **S3 Credentials (AWS SigV4)** — for boto3, aws-cli, rclone

Each has a Create form, a list of existing keys (showing prefix only),
and a revoke button.

### `/dashboard/admin/*` — Admin only
- **Users** — see all users, create new, edit quotas inline, activate/deactivate
- **Audit Log** — every authenticated request (last 200 by default), filter
  by user/action
- **System** — live stats every 5s + Storage panel (physical disk, user
  allocations, configuration form)

## Keyboard shortcuts

| | |
|---|---|
| `Ctrl+R` | Reload current page (default browser behavior — works fine since
JWT is in localStorage) |
| (Drag a file onto the bucket page) | Triggers upload zone |

## What the dashboard *doesn't* do (yet)

- Folder/prefix navigation (you see flat key list)
- Bulk select / bulk delete
- Search
- Password change (use the admin panel or SQL — see [user-management.md](../user-management.md))
- Resume an interrupted upload (refresh = re-upload from scratch)
