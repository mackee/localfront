#!/usr/bin/env bash
# Verify one docker-compose-backed example end to end:
#   1. bring up its dependencies (object store, optional backend) with compose,
#   2. seed the object store,
#   3. start localfront on the host against the example template,
#   4. run the example's runn scenario,
#   5. tear everything down.
#
# Usage: scripts/verify-example.sh <example-name> <ready-host>
set -euo pipefail

example="${1:?usage: verify-example.sh <example-name> <ready-host>}"
ready_host="${2:?usage: verify-example.sh <example-name> <ready-host>}"

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
dir="$repo_root/examples/$example"
compose="docker compose -f $dir/compose.yaml"
bin="$repo_root/bin/localfront"
endpoint="http://127.0.0.1:8080"

lf_pid=""
cleanup() {
  [ -n "$lf_pid" ] && kill "$lf_pid" 2>/dev/null || true
  $compose down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo ">> [$example] starting dependencies"
$compose up -d

echo ">> [$example] seeding the object store"
$compose run --rm seed

echo ">> [$example] starting localfront"
"$bin" serve \
  --template "$dir/template.yaml" \
  --listen 127.0.0.1:8080 \
  --public-host 127.0.0.1:8080 \
  --s3-endpoint http://127.0.0.1:9000 \
  --s3-access-key rustfsadmin \
  --s3-secret-key rustfsadmin &
lf_pid=$!

echo ">> [$example] waiting for localfront"
ready=false
for _ in $(seq 1 50); do
  if curl -s -o /dev/null -H "Host: $ready_host" "$endpoint/"; then ready=true; break; fi
  sleep 0.2
done
if [ "$ready" != true ]; then
  echo "localfront did not become ready" >&2
  exit 1
fi

echo ">> [$example] running runn scenario"
( cd "$dir" && LF_ENDPOINT="$endpoint" runn run scenario.yaml )
