#!/usr/bin/env bash
# =============================================================================
# Part 10 smoke test — AWS CLI / boto3 compatibility via SigV4
#
# Requires `aws` CLI installed locally. If you don't have it:
#   sudo apt install awscli       (Debian/Ubuntu)
#   brew install awscli            (macOS)
#
# Or run it inside a Docker container:
#   docker run --rm -it --network s3-net amazon/aws-cli ...
# =============================================================================

set -euo pipefail

API="${API_URL:-http://localhost:8080/api}"
ENDPOINT="${API}"            # aws cli's --endpoint-url
BUCKET="awstest-$(date +%s)"
PROFILE="ps3-test"

red()   { echo -e "\033[31m$*\033[0m"; }
green() { echo -e "\033[32m$*\033[0m"; }

# aws CLI: prefer local install, else fall back to the official Docker image.
if command -v aws >/dev/null 2>&1; then
  AWS_BIN="aws"
else
  echo "(aws CLI not found locally — using Docker fallback: amazon/aws-cli)"
  # Use host network so localhost:8080 reaches nginx, and mount $HOME/.aws so
  # the profile we create persists across invocations.
  mkdir -p "$HOME/.aws"
  AWS_BIN="docker run --rm --network host \
    -v $HOME/.aws:/root/.aws \
    -v /tmp:/tmp \
    amazon/aws-cli"
fi

# ---------- 1. Login → JWT → S3 credentials --------------------------------
echo "== 1. Create S3 credentials via dashboard API =============="
JWT=$(curl -s -X POST "$API/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"email":"admin@local","password":"admin"}' \
  | python3 -c "import json,sys;print(json.load(sys.stdin)['token'])")

CREDS=$(curl -s -X POST "$API/auth/s3-credentials" \
  -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/json" \
  -d '{"name":"part10-test"}')

AKID=$(echo "$CREDS"   | python3 -c "import json,sys;print(json.load(sys.stdin)['accessKeyId'])")
SECRET=$(echo "$CREDS" | python3 -c "import json,sys;print(json.load(sys.stdin)['secretAccessKey'])")
green "OK got AKID=${AKID:0:14}... secret=${SECRET:0:10}..."

# ---------- 2. Configure aws-cli profile ------------------------------------
echo "== 2. Configure aws-cli profile '$PROFILE' ================="
# Write the config files directly — avoids Docker UID issues from `aws configure`
mkdir -p "$HOME/.aws"
cat > "$HOME/.aws/credentials" <<EOF
[$PROFILE]
aws_access_key_id = $AKID
aws_secret_access_key = $SECRET
EOF
cat > "$HOME/.aws/config" <<EOF
[profile $PROFILE]
region = us-east-1
output = json
EOF

AWS="$AWS_BIN --profile $PROFILE --endpoint-url=$ENDPOINT"

# ---------- 3. List buckets (should be at least 0) -------------------------
echo "== 3. aws s3 ls =============================================="
$AWS s3 ls
green "OK"

# ---------- 4. Create bucket -----------------------------------------------
echo "== 4. aws s3 mb s3://$BUCKET ================================"
$AWS s3 mb "s3://$BUCKET"
green "OK"

# ---------- 5. Upload a small file -----------------------------------------
echo "== 5. aws s3 cp (small file) ================================"
echo "hello from aws cli" > /tmp/sigv4-test.txt
$AWS s3 cp /tmp/sigv4-test.txt "s3://$BUCKET/hello.txt"
green "OK"

# ---------- 6. List bucket --------------------------------------------------
echo "== 6. aws s3 ls s3://$BUCKET ================================"
$AWS s3 ls "s3://$BUCKET"
green "OK"

# ---------- 7. Download + verify ------------------------------------------
echo "== 7. aws s3 cp (download) =================================="
$AWS s3 cp "s3://$BUCKET/hello.txt" /tmp/sigv4-down.txt
diff /tmp/sigv4-test.txt /tmp/sigv4-down.txt && green "OK contents match" || { red "mismatch"; exit 1; }

# ---------- 8. Multipart upload via sync (forces multipart on >8MB) -------
echo "== 8. aws s3 cp (multipart, 20 MiB) ========================="
dd if=/dev/urandom of=/tmp/sigv4-big.bin bs=1M count=20 status=none
$AWS s3 cp /tmp/sigv4-big.bin "s3://$BUCKET/big.bin" \
  --expected-size $((20*1024*1024))
green "OK multipart upload"

# ---------- 9. Delete -----------------------------------------------------
echo "== 9. aws s3 rm + rb ========================================"
$AWS s3 rm "s3://$BUCKET/hello.txt"
$AWS s3 rm "s3://$BUCKET/big.bin"
$AWS s3 rb "s3://$BUCKET"
green "OK"

# ---------- 10. Cleanup ----------------------------------------------------
rm -f /tmp/sigv4-test.txt /tmp/sigv4-down.txt /tmp/sigv4-big.bin

# Revoke credential
curl -s -X DELETE "$API/auth/s3-credentials/$AKID" \
  -H "Authorization: Bearer $JWT" -o /dev/null

echo
green "All Part 10 tests passed."
