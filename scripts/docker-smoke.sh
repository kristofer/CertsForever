#!/usr/bin/env bash
# Smoke-test a built CertsForever image the way production runs it:
# read-only root filesystem, non-root user, data on a named volume.
#
#   scripts/docker-smoke.sh certsforever:local
#
# Checks: becomes healthy, /readyz ok, runs as uid 65532, issues and serves a
# certificate and its share image, takes a backup, survives a restart with
# data intact, and rejects an invalid production config.
set -euo pipefail

IMAGE="${1:?usage: $0 IMAGE}"
NAME="certsforever-smoke-$$"
VOL="certsforever-smoke-$$"
TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"

cleanup() {
  status=$?
  if [ $status -ne 0 ]; then
    echo "--- container logs ---"
    docker logs "$NAME" 2>&1 | tail -50 || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker volume rm "$VOL" >/dev/null 2>&1 || true
  exit $status
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok   $*"; }

start() {
  docker run -d --name "$NAME" \
    --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges:true \
    -v "$VOL:/data" -p 127.0.0.1::8080 \
    -e CERTS_BASE_URL=https://certs.example.test \
    -e CERTS_ADMIN_TOKEN="$TOKEN" \
    --health-interval 1s --health-start-period 2s \
    "$IMAGE" >/dev/null
}

wait_healthy() {
  for _ in $(seq 1 60); do
    state="$(docker inspect -f '{{.State.Health.Status}}' "$NAME" 2>/dev/null || echo missing)"
    [ "$state" = healthy ] && return 0
    [ "$(docker inspect -f '{{.State.Running}}' "$NAME")" = true ] || fail "container exited"
    sleep 1
  done
  fail "container not healthy after 60s (last: $state)"
}

url() { echo "http://127.0.0.1:$(docker port "$NAME" 8080/tcp | head -1 | sed 's/.*://')$1"; }

# 1. Invalid production config fails fast with a clear message.
if out="$(docker run --rm -e CERTS_BASE_URL=http://localhost:8080 "$IMAGE" serve 2>&1)"; then
  fail "server started with an http/localhost base URL in production"
fi
grep -q "must use https in production" <<<"$out" || fail "unexpected config error output: $out"
ok "rejects invalid production config"

# 2. Starts healthy as non-root.
start
wait_healthy
ok "healthy"
[ "$(docker inspect -f '{{.Config.User}}' "$NAME")" = "65532:65532" ] || fail "not running as 65532"
ok "runs as uid 65532 with read-only root fs"

ready="$(curl -fsS "$(url /readyz)")"
grep -q '"status": "ok"' <<<"$ready" || fail "readyz: $ready"
grep -q '"schema_version": 1' <<<"$ready" || fail "readyz schema: $ready"
ok "readyz: $(tr -d '\n ' <<<"$ready")"

# 3. Issue a certificate inside the container and fetch it from outside.
demo="$(docker exec "$NAME" certsforever demo)"
id="$(grep -o 'ZCW-[0-9A-Z]\{10\}' <<<"$demo" | head -1)"
[ -n "$id" ] || fail "demo printed no certificate id: $demo"
code="$(curl -s -o /dev/null -w '%{http_code}' "$(url "/c/$id")")"
[ "$code" = 200 ] || fail "certificate page returned $code"
ctype="$(curl -s -o /dev/null -w '%{content_type}' "$(url "/c/$id/og.png")")"
[ "$ctype" = image/png ] || fail "og.png content type $ctype"
curl -fsS "$(url "/c/$id")" | grep -q 'https://certs.example.test/c/'"$id" || fail "canonical URL not from CERTS_BASE_URL"
ok "issued and served $id"

# 4. Admin API answers with the token and refuses without it.
[ "$(curl -s -o /dev/null -w '%{http_code}' "$(url /admin/api/stats)")" = 401 ] || fail "admin API open without token"
curl -fsS -H "Authorization: Bearer $TOKEN" "$(url /admin/api/stats)" | grep -q '"certificates": 1' || fail "admin stats"
ok "admin API auth"

# 5. Online backup into the volume.
docker exec "$NAME" certsforever backup /data/backups/smoke.db | grep -q "backup written" || fail "backup"
ok "backup"

# 6. Restart: data persists, no re-migration.
docker restart "$NAME" >/dev/null
wait_healthy
code="$(curl -s -o /dev/null -w '%{http_code}' "$(url "/c/$id")")"
[ "$code" = 200 ] || fail "certificate missing after restart ($code)"
docker logs "$NAME" 2>&1 | grep -c '"database migrated"' | grep -qx 1 || fail "migrations re-ran on restart"
ok "restart preserved data"

echo "all smoke checks passed for $IMAGE"
