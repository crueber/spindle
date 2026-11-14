#!/usr/bin/env bash
# ci-s3-bucket.sh — make the S3 contract test bucket exist on the job's rustfs.
#
# The store never creates buckets (docs/go/03_store_backends.md: the bucket is
# provisioned out of band; `compose.yaml` does it with an aws-cli service), and
# a CI service container starts empty. This is the same provisioner with no
# extra dependency: curl's --aws-sigv4 signs the PUT (curl >= 7.75).
#
# Idempotent and retrying: the service may still be starting when the job's
# first step runs. 200 = created, 409 = already ours, 204 = accepted.
#
#   scripts/ci-s3-bucket.sh                    # rig defaults
#   WALHUB_TEST_S3_ENDPOINT=http://rustfs:9000 scripts/ci-s3-bucket.sh
set -Eeuo pipefail

endpoint=${WALHUB_TEST_S3_ENDPOINT:-http://127.0.0.1:9000}
ak=${WALHUB_TEST_S3_ACCESS_KEY:-walgit-dev}
sk=${WALHUB_TEST_S3_SECRET_KEY:-walgit-dev-secret}
bucket=${WALHUB_TEST_S3_BUCKET:-walhub-test}
region=${WALHUB_TEST_S3_REGION:-us-east-1}

url="${endpoint%/}/${bucket}"
code=""
for _ in $(seq 1 30); do
  code=$(curl -sS -o /tmp/s3-bucket.out -w '%{http_code}' \
    --aws-sigv4 "aws:amz:${region}:s3" --user "${ak}:${sk}" \
    -X PUT "$url" 2>/dev/null || true)
  case "$code" in
    200|204|409) echo "s3 bucket ${bucket} ready at ${endpoint} (HTTP ${code})"; exit 0 ;;
  esac
  sleep 2
done

echo "could not create s3 bucket ${bucket} at ${endpoint} (last HTTP ${code})" >&2
[ -s /tmp/s3-bucket.out ] && cat /tmp/s3-bucket.out >&2
exit 1
