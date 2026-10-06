#!/usr/bin/env bash
# Smoke-test a built CertsForever image the way production runs it:
# read-only root filesystem, non-root user, data on a named volume.
#
#   scripts/docker-smoke.sh certsforever:local
#
# Checks: rejects an invalid production config, becomes healthy, /readyz ok,
# runs as uid 65532, issues and serves a certificate and its share image,
# admin-token auth, platform-admin bootstrap and sign-in links, email
# reported off without SMTP, takes a backup, and survives a restart with
# data intact.
set -euo pipefail

IMAGE="${1:?usage: $0 IMAGE}"
NAME="certsforever-smoke-$$"
VOL="certsforever-smoke-$$"
TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
MASTER_KEY="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"

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
    -e CERTS_MASTER_KEY="$MASTER_KEY" \
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
grep -q "CERTS_MASTER_KEY is required" <<<"$out" || fail "missing master key not reported: $out"
ok "rejects invalid production config"

# 2. Starts healthy as non-root.
start
wait_healthy
ok "healthy"
[ "$(docker inspect -f '{{.Config.User}}' "$NAME")" = "65532:65532" ] || fail "not running as 65532"
ok "runs as uid 65532 with read-only root fs"

ready="$(curl -fsS "$(url /readyz)")"
grep -q '"status": "ok"' <<<"$ready" || fail "readyz: $ready"
grep -Eq '"schema_version": [1-9]' <<<"$ready" || fail "readyz schema: $ready"
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
curl -fsS "$(url "/c/$id")" | grep -q 'href="/theme/1f8f81.css"' || fail "certificate page lacks its design stylesheet"
curl -fsS "$(url /theme/1f8f81.css)" | grep -q -- '--cert-accent: #1f8f81' || fail "theme stylesheet"
ok "issued and served $id (default design)"

# 4. Admin API answers with the token and refuses without it; the client
#    API refuses the platform token.
[ "$(curl -s -o /dev/null -w '%{http_code}' "$(url /admin/api/clients)")" = 401 ] || fail "admin API open without token"
curl -fsS -H "Authorization: Bearer $TOKEN" "$(url /admin/api/clients/zcw/stats)" | grep -q '"certificates": 1' || fail "admin stats"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$(url /api/v1/certificates)")" = 401 ] || fail "client API accepted the platform token"
ok "admin API auth"

# 4b. Caddy's on-demand TLS check: yes for the platform host, no for others.
[ "$(curl -s -o /dev/null -w '%{http_code}' "$(url '/internal/tls-ask?domain=certs.example.test')")" = 200 ] || fail "tls-ask refused the platform host"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$(url '/internal/tls-ask?domain=evil.example')")" = 404 ] || fail "tls-ask allowed an unknown domain"
ok "tls-ask"

# 5. Accounts: bootstrap a platform admin from the CLI and check the link.
link_out="$(docker exec "$NAME" certsforever superadmin add ops@example.test)"
link="$(grep -o 'https://certs.example.test/login/[A-Za-z0-9_-]*' <<<"$link_out")"
[ -n "$link" ] || fail "superadmin add printed no sign-in link: $link_out"
path="${link#https://certs.example.test}"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$(url /login)")" = 200 ] || fail "login page"
curl -fsS "$(url "$path")" | grep -q "Sign in as ops@example.test" || fail "sign-in link page"
curl -fsS "$(url "$path")" | grep -q "Sign in as ops@example.test" || fail "opening the link used it up"
# Capture the logs first: with pipefail, `docker logs | grep -q` can fail
# (or wrongly pass) when grep exits early and docker logs gets SIGPIPE.
logs="$(docker logs "$NAME" 2>&1)"
grep -q -- "${path#/login/}" <<<"$logs" && fail "sign-in token written to the logs"
docker exec "$NAME" certsforever superadmin list | grep -q "ops@example.test" || fail "superadmin list"
ok "sign-in link works, isn't consumed by GET, isn't logged"

# 6. Email: without SMTP in production it's reported as off, not silently dropped.
docker exec "$NAME" certsforever email status | grep -q "queued 0" || fail "email status"
if out="$(docker exec "$NAME" certsforever email test ops@example.test 2>&1)"; then
  fail "email test claimed success without SMTP: $out"
fi
grep -q "email is off" <<<"$out" || fail "email test didn't explain: $out"
logs="$(docker logs "$NAME" 2>&1)"
grep -q "CERTS_SMTP_URL not set: email is off" <<<"$logs" || fail "no startup warning about email being off"
ok "email reported off without SMTP"

# 7. Online backup into the volume.
docker exec "$NAME" certsforever backup /data/backups/smoke.db | grep -q "backup written" || fail "backup"
ok "backup"

# 8. Restart: data persists, no re-migration.
docker restart "$NAME" >/dev/null
wait_healthy
code="$(curl -s -o /dev/null -w '%{http_code}' "$(url "/c/$id")")"
[ "$code" = 200 ] || fail "certificate missing after restart ($code)"
docker logs "$NAME" 2>&1 | grep -c '"database migrated"' | grep -qx 1 || fail "migrations re-ran on restart"
ok "restart preserved data"

echo "all smoke checks passed for $IMAGE"
