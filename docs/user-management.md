# User Management

How to create accounts, give people the right kind of credentials, send
those credentials securely, and revoke access.

---

## Concepts: three credential types

Every account can hold all three. Each is for a different audience:

| Credential | Format | For | Stored |
|---|---|---|---|
| **Password** (+ email) | text | logging into the dashboard in a browser | bcrypt hash in `users.password_hash` |
| **API key** (Bearer) | `psk_XXXXXXXX.YYY...` | curl / shell scripts / your own apps | SHA-256 hash in `api_keys.key_hash` |
| **S3 credentials** | AKID + secret | aws-cli / boto3 / rclone / aws-sdk-* | plaintext in `s3_credentials.secret_key` |

A user might have:
- 1 password
- 3 API keys (one per laptop/CI/script)
- 2 S3 credentials (one for backup tool, one for some integration)

---

## Creating a new account

### Option A — Dashboard (easiest)

1. Log in as an admin user (default: `admin@local` / `admin`)
2. Sidebar → **Admin → Users**
3. Click **+ New user**
4. Fill in email, name, password, quota (in GB), role
5. **Create user**

The new user can now log in to the dashboard with the email + password
you set, and create their own API keys / S3 credentials.

### Option B — API (for scripting bulk creation)

```bash
# As admin, get a JWT
JWT=$(curl -s -X POST http://localhost:8080/api/auth/login \
       -H "Content-Type: application/json" \
       -d '{"email":"admin@local","password":"YOUR-PASSWORD"}' \
     | python3 -c "import json,sys;print(json.load(sys.stdin)['token'])")

# Create a user
curl -X POST http://localhost:8080/api/admin/users \
  -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/json" \
  -d '{
    "email":      "alice@example.com",
    "name":       "Alice Smith",
    "password":   "set-by-admin-temporary-password",
    "role":       "user",
    "quotaBytes": 53687091200
  }'
# → 201 {"id":"...","email":"alice@example.com",...}
```

Quota in bytes: 50 GB = 50 × 1024³ = 53687091200.

---

## Sending credentials to a remote server / another person

Once you've created the account, you need to get credentials to the user
(or to a remote machine). Three patterns depending on the situation:

### Pattern 1 — Person, who will use the dashboard

Send them in a 1Password / Bitwarden / signal/keybase share:

```
Login URL:  https://s3.your-domain.com/
Email:      alice@example.com
Password:   set-by-admin-temporary-password
```

Tell them to change the password on first login.

(Password change UI is on the user's profile page — *coming in a future
update*; for now they can ask you to change it via the admin panel.)

### Pattern 2 — Remote server / CI, using the native API

Generate an API key in the dashboard as the target user (or via API as
admin acting on their behalf):

**Manual (as the target user, in the dashboard):**
1. Log in as the user
2. Sidebar → **API Keys**
3. **Bearer API Keys** → name it (e.g., `backup-server`) → **Create**
4. Copy the `psk_xxxx.yyy...` value — it's shown ONCE

**Then on the remote server:**
```bash
# Store the API key in an env file or systemd EnvironmentFile
ssh user@remote 'cat > ~/.personals3.env' <<'EOF'
PS3_URL=https://s3.your-domain.com
PS3_KEY=psk_abc12345.xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
EOF

ssh user@remote 'chmod 600 ~/.personals3.env'
```

**Test from the remote server:**
```bash
source ~/.personals3.env
curl -H "Authorization: Bearer $PS3_KEY" "$PS3_URL/api/auth/me"
# → {"email":"alice@example.com","quotaBytes":...,"usedBytes":...}
```

### Pattern 3 — Remote server / CI, using AWS S3 tooling

Generate S3 credentials (AKID + secret) instead:

1. Log in as the target user
2. Sidebar → **API Keys**
3. **S3 Credentials (AWS SigV4)** → name it → **Create**
4. Copy BOTH the Access Key ID and Secret Access Key — shown ONCE

**On the remote server, configure aws-cli:**
```bash
# Write directly to credentials file (most reliable cross-version method)
mkdir -p ~/.aws
cat > ~/.aws/credentials <<EOF
[personals3]
aws_access_key_id = AKIA....
aws_secret_access_key = xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
EOF
cat > ~/.aws/config <<EOF
[profile personals3]
region = us-east-1
output = json
EOF

# Test
aws --profile personals3 --endpoint-url=https://s3.your-domain.com/api s3 ls
```

For boto3 see [clients/boto3.md](./clients/boto3.md).
For rclone see [clients/rclone.md](./clients/rclone.md).

---

## Quotas

Each user has a `quota_bytes` cap. Uploads that would exceed it return
**HTTP 507 Insufficient Storage**.

**Adjust a quota:**

Dashboard → Admin → Users → click "edit quota" → enter GB → Enter.

Or via API:
```bash
curl -X PATCH http://localhost:8080/api/admin/users/$USER_ID \
  -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/json" \
  -d '{"quotaBytes": 107374182400}'   # 100 GB
```

**The system prevents overcommit by default** — admin can't grant more
total quota than the disk can physically hold (minus reserved bytes). To
allow over-allocation (useful when most users underutilize), toggle
`overcommit_allowed: true` in Admin → System → Storage Configuration.

---

## Disabling / deleting an account

**Soft delete** (recommended — preserves audit log):

Dashboard → Admin → Users → "deactivate"

Or:
```bash
curl -X PATCH http://localhost:8080/api/admin/users/$USER_ID \
  -H "Authorization: Bearer $JWT" \
  -d '{"isActive": false}'
```

The user can't log in, can't use API keys, can't use SigV4. Their data
remains; their buckets remain; the audit log shows what they did.

**Hard delete** (cascades to buckets, objects, keys, credentials):

```bash
docker compose exec postgres psql -U s3admin -d personals3 \
  -c "DELETE FROM users WHERE email = 'alice@example.com';"
```

Be careful — this is irreversible and wipes the user's data.

---

## Revoking individual credentials (less drastic than deactivating)

If a single API key was leaked, revoke just that key without touching the
account:

**Dashboard:**
API Keys → click the trash icon next to the key.

**API:**
```bash
curl -X DELETE http://localhost:8080/api/auth/keys/$KEY_ID \
  -H "Authorization: Bearer $JWT"
```

Same for S3 credentials:
```bash
curl -X DELETE http://localhost:8080/api/auth/s3-credentials/$AKID \
  -H "Authorization: Bearer $JWT"
```

The next request from that credential gets **401 Unauthorized** within ms.

---

## Auditing who-did-what

Every authenticated request lands in the `audit_log` table:

Dashboard → Admin → Audit Log → filter by user email or action.

Or via SQL:
```bash
docker compose exec postgres psql -U s3admin -d personals3 -c "
  SELECT ts, action, bucket_name, object_key, status_code
    FROM audit_log
   WHERE user_id = (SELECT id FROM users WHERE email='alice@example.com')
   ORDER BY ts DESC LIMIT 50;"
```

---

## Recipes

### Bulk-create 10 users from a CSV

```bash
JWT=$(...)   # as above

while IFS=, read -r email name quota_gb; do
  curl -X POST http://localhost:8080/api/admin/users \
    -H "Authorization: Bearer $JWT" \
    -H "Content-Type: application/json" \
    -d "$(jq -n --arg e "$email" --arg n "$name" --argjson q "$((quota_gb * 1024 * 1024 * 1024))" \
          '{email:$e, name:$n, password:"changeme", quotaBytes:$q, role:"user"}')"
done < users.csv
```

### "Rotate everyone's API keys"

```bash
# As each user, delete all of their keys and create a fresh one.
# Or as admin, mass-revoke via SQL then ask users to recreate:
docker compose exec postgres psql -U s3admin -d personals3 \
  -c "DELETE FROM api_keys WHERE created_at < now() - INTERVAL '90 days';"
```

### Reset a forgotten admin password

Without the password, you can't log in via the dashboard to change it. Reset
directly in the DB:

```bash
docker compose exec postgres psql -U s3admin -d personals3 -c "
  UPDATE users
     SET password_hash = crypt('new-password-here', gen_salt('bf', 10))
   WHERE email = 'admin@local';"
```
