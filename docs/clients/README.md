# Clients

How to use PersonalS3 from various tools.

Pick by audience:

| Tool | Auth method | Doc |
|---|---|---|
| **Browser** | Email + password (→ JWT) | [dashboard.md](./dashboard.md) |
| **curl / shell scripts** | Bearer API key | [curl.md](./curl.md) |
| **AWS CLI** | AWS SigV4 (AKID + secret) | [aws-cli.md](./aws-cli.md) |
| **Python (boto3)** | AWS SigV4 | [boto3.md](./boto3.md) |
| **rclone** (backups/sync) | AWS SigV4 | [rclone.md](./rclone.md) |
| **Any AWS SDK** (Node/Java/Go) | AWS SigV4 | See aws-cli.md — same credentials |

## Endpoint URLs by tool

For **same-origin tools** (just the URL, no path): use `https://s3.yourdomain.com`

For **path-prefixed tools** (most AWS clients work this way): use `https://s3.yourdomain.com/api`

When in doubt, the SigV4 ones need `/api` at the end of the endpoint URL,
because SigV4 traffic shares the same nginx as the dashboard.
