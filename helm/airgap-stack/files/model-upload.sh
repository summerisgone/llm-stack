#!/bin/sh
# Upload models from a node directory (/src) to MinIO bucket `models` after
# verifying every file against models/checksums.txt (ADR 0019). Run by
# scripts/models-upload as a Job on the node that holds the source copy;
# the bucket and its anonymous-read policy exist already.
#   model-upload.sh <model>...
set -eu
. /scripts/model-sums.sh
. /minio/config.env
export RCLONE_CONFIG_STORE_TYPE=s3 RCLONE_CONFIG_STORE_PROVIDER=Minio
export RCLONE_CONFIG_STORE_ENDPOINT="$MINIO_URL"
export RCLONE_CONFIG_STORE_ACCESS_KEY_ID="$MINIO_ROOT_USER"
export RCLONE_CONFIG_STORE_SECRET_ACCESS_KEY="$MINIO_ROOT_PASSWORD"
for model in "$@"; do
  model_sums "$model" > /tmp/sums
  (cd /src && sha256sum -c /tmp/sums)
  while read -r sum path; do
    rclone -q copyto "/src/$path" "store:models/$path" </dev/null
  done < /tmp/sums
  echo "model-upload: $model uploaded"
done
