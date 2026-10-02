#!/bin/sh
# Fill this node's model cache from MinIO bucket `models` (ADR 0019): fetch
# every file models/checksums.txt lists for <model>, verify all of them, then
# publish the model under /cache. A cache whose recorded checksums match
# returns at once. Runs as the engine pod's initContainer.
#   model-cache-fill.sh <model>
set -eu
. /scripts/model-sums.sh
model=$1
cache=/cache
model_sums "$model" > /tmp/sums
if [ -e "$cache/$model" ] && [ "$(cat "$cache/.$model.sha256" 2>/dev/null)" = "$(cat /tmp/sums)" ]; then
  echo "model-cache: $model is cached"
  exit 0
fi
# Anonymous read: the upload made bucket `models` downloadable.
export RCLONE_CONFIG_STORE_TYPE=s3 RCLONE_CONFIG_STORE_PROVIDER=Minio
export RCLONE_CONFIG_STORE_ENDPOINT="$MINIO_URL"
tmp="$cache/.$model.partial"
rm -rf "$tmp"
mkdir -p "$tmp"
while read -r sum path; do
  rclone -q copyto "store:models/$path" "$tmp/$path" </dev/null
done < /tmp/sums
(cd "$tmp" && sha256sum -c /tmp/sums)
rm -rf "$cache/$model" "$cache/.$model.sha256"
mv "$tmp/$model" "$cache/$model"
cp /tmp/sums "$cache/.$model.sha256"
rm -rf "$tmp"
echo "model-cache: $model filled and verified"
