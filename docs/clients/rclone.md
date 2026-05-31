# rclone

[rclone](https://rclone.org/) is the best tool for serious sync / backup workloads —
better progress bars, smarter retry, mount support, bandwidth limits, and
filters than aws-cli.

## Install

```bash
# Debian/Ubuntu
sudo apt install rclone

# Or upstream (newer)
curl https://rclone.org/install.sh | sudo bash
```

## Configure a remote (one-time)

```bash
rclone config
```

Walk through:

```
n) New remote
name> personals3
Type of storage> s3
provider> Other                        ← important: not AWS
env_auth> false
access_key_id> AKIAXXXXXXXXXXXXXXXX
secret_access_key> ********************
region> us-east-1
endpoint> https://s3.yourdomain.com/api
location_constraint> us-east-1
acl> private
no_check_bucket> true                  ← skip a check that would fail
y) Yes, save
q) Quit
```

Or write the config directly:

```bash
mkdir -p ~/.config/rclone
cat > ~/.config/rclone/rclone.conf <<EOF
[personals3]
type = s3
provider = Other
access_key_id = AKIAXXXXXXXXXXXXXXXX
secret_access_key = xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
region = us-east-1
endpoint = https://s3.yourdomain.com/api
acl = private
force_path_style = true
EOF
chmod 600 ~/.config/rclone/rclone.conf
```

## Daily commands

```bash
# List buckets
rclone lsd personals3:

# List a bucket
rclone ls personals3:my-bucket

# Tree view
rclone tree personals3:my-bucket

# Upload a single file
rclone copyto local.txt personals3:my-bucket/remote.txt

# Mirror a directory (preserves structure)
rclone copy /home/me/photos personals3:photos/

# Sync (deletes destination files that no longer exist in source)
rclone sync /home/me/photos personals3:photos/

# Move (= copy + delete source)
rclone move /tmp/old-data personals3:archive/

# Delete
rclone delete personals3:my-bucket/file.txt
rclone purge personals3:my-bucket           # deletes bucket + contents
```

## Cool things rclone does that aws-cli can't

### Show real-time progress for big transfers

```bash
rclone copy huge.mp4 personals3:videos/ --progress
# 412.345 MiB / 4.321 GiB,  9%, 25.412 MiB/s,  ETA  2m31s
```

### Bandwidth limit (don't saturate your home upload)

```bash
rclone sync ~/Documents personals3:docs/ --bwlimit 5M
# Cap at 5 MB/s. Or 10M:08,off:18 (10MB/s 8am-6pm, unlimited otherwise)
```

### Resume an interrupted sync

```bash
rclone sync ~/big-data personals3:big-data/ --retries 10 --low-level-retries 50
# Hit Ctrl+C, rerun the same command — it picks up only what's missing
```

### Encrypt your backups (rclone-side, transparent)

```bash
rclone config       # add new remote, type "crypt"
# Wraps a real remote (personals3:secure) — every file is encrypted client-side
# Filenames also obfuscated. Server can't read your data.
```

### Mount as a filesystem

```bash
mkdir ~/mnt/s3
rclone mount personals3:my-bucket ~/mnt/s3 --vfs-cache-mode full --daemon

# Now you can use it like any local dir
ls ~/mnt/s3
cp file.txt ~/mnt/s3/uploads/

# Unmount
fusermount -u ~/mnt/s3
```

This makes any tool that doesn't speak S3 (text editor, video player,
backup tool) work transparently against PersonalS3.

### Dry-run before syncing

```bash
rclone sync ~/photos personals3:photos/ --dry-run
# Shows what WOULD happen without actually doing it
```

## Sample backup with rclone

`/etc/systemd/system/personals3-backup.service`:
```ini
[Unit]
Description=Nightly rclone backup to PersonalS3
After=network-online.target

[Service]
Type=oneshot
User=me
EnvironmentFile=/home/me/.personals3.env
ExecStart=/usr/bin/rclone sync /home/me/important personals3:nightly/ \
  --delete-during \
  --bwlimit 10M \
  --transfers 4 \
  --checkers 8 \
  --exclude "**/.cache/**" \
  --exclude "*.tmp" \
  --log-file /var/log/personals3-backup.log
```

`/etc/systemd/system/personals3-backup.timer`:
```ini
[Unit]
Description=Nightly PersonalS3 backup

[Timer]
OnCalendar=*-*-* 03:00:00
Persistent=true

[Install]
WantedBy=timers.target
```

Enable:
```bash
sudo systemctl daemon-reload
sudo systemctl enable --now personals3-backup.timer
```

## See also

- https://rclone.org/s3/#other — official rclone docs for non-AWS S3 providers
- [aws-cli.md](./aws-cli.md) — simpler alternative
