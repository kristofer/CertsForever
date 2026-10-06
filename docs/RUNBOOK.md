# CertsForever runbook

Operational procedures for a Docker Compose deployment on a single host.
Commands run from the repository checkout on the server (where
`compose.yaml` and `.env` live).

> **The database is the registry of record.** Every issued certificate
> lives in one SQLite file on the `certs-data` volume (`/data/certs.db` in
> the container). If it's lost without a backup, every certificate link
> breaks. Backups come first.

## Contents

1. [First deploy](#1-first-deploy)
2. [Check health](#2-check-health)
3. [Upgrade](#3-upgrade)
4. [Back up](#4-back-up)
5. [Restore](#5-restore)
6. [Roll back a bad release](#6-roll-back-a-bad-release)
7. [Rotate the admin token](#7-rotate-the-admin-token) · [People and access](#7a-people-and-access) · [Email](#7b-email)
8. [Logs](#8-logs)
9. [Releases and CI](#9-releases-and-ci)
10. [Configuration reference](#10-configuration-reference)

---

## 1. First deploy

Prerequisites: a Linux host with Docker Engine and the Compose plugin, a DNS
`A`/`AAAA` record for the certificate domain pointing at the host, and
ports 80 and 443 open.

```sh
git clone <repo> certsforever && cd certsforever
cp .env.example .env
$EDITOR .env          # CERTS_BASE_URL, CERTS_DOMAIN, CERTS_ADMIN_TOKEN, …
openssl rand -hex 32  # paste into CERTS_ADMIN_TOKEN
openssl rand -hex 32  # paste into CERTS_MASTER_KEY, and store a copy with your backups
# optional now, needed for emailed sign-in: CERTS_SMTP_URL and CERTS_MAIL_FROM

docker compose --profile tls up -d --build
docker compose ps     # certsforever should be "healthy"

# The first platform administrator; prints a one-time sign-in link (1 hour).
docker compose exec certsforever certsforever superadmin add you@example.org -name "Your Name"
```

Open the link, set up your authenticator app, and you're in `/super`. From
there, create clients and invite their administrators.

**Keep `CERTS_MASTER_KEY` with your backups.** It encrypts the two-step
secrets in the database. A restored database with a different key works, but
every administrator's two-step verification must be reset
(`certsforever user reset-2fa EMAIL`) and set up again.

`CERTS_BASE_URL` is permanent: it's baked into every certificate link
students post. Choose it carefully before issuing anything.

Without the `tls` profile, the app listens on `127.0.0.1:8080` only. Use that
when another reverse proxy (nginx, a load balancer, Cloudflare Tunnel)
terminates TLS.

To use a published image instead of building on the server, replace
`build:` with `image: ghcr.io/<owner>/<repo>:<version>` in `compose.yaml` (or
in a `compose.override.yaml`).

## 2. Check health

```sh
docker compose ps                               # STATUS shows (healthy)
curl -s http://127.0.0.1:8080/readyz            # {"status":"ok","schema_version":…,"version":…}
docker compose exec certsforever certsforever version
```

- `/healthz`: the process is up.
- `/readyz`: both database pools answer and the schema is migrated. Docker's
  `HEALTHCHECK` and external uptime monitors should use this one.

Point an external uptime monitor at `https://<domain>/readyz` **and** at one
real public certificate URL.

## 3. Upgrade

```sh
git pull
docker compose --profile tls up -d --build
docker compose logs certsforever | grep -E '"starting"|"database migrated"|"listening"'
```

On start, the server applies any pending migrations. Before applying them
to a database that already has data, it writes a snapshot next to it:
`/data/certs.pre-<version>-<timestamp>.db`. The log line `database migrated`
names the snapshot.

Upgrading a database created before Milestone 1 applies migration 002. It
moves all existing courses, students and certificates into a client named
`zcw` (Zip Code Wilmington, prefix `ZCW`). Existing certificate URLs don't
change. Afterwards, set the LinkedIn company ID on the client (§10), and remove
any `CERTS_ORG_*`, `CERTS_SITE_URL` and `CERTS_LINKEDIN_ORG_ID` lines from
`.env`. They're ignored now, and the server warns about them at startup.

Migrations are forward-only. The server refuses to start if:
- an already-applied migration file was edited (checksum mismatch);
- the database was migrated by a *newer* build (the downgrade guard).
  See §6.

Snapshots aren't pruned automatically. After an upgrade has run
cleanly for a while, remove old ones (see §4 for listing files).

## 4. Back up

**Online backup (safe while serving):**

```sh
docker compose exec certsforever certsforever backup /data/backups/certs-$(date -u +%Y%m%dT%H%M%SZ).db
```

**Copy it off the host.** A backup on the same disk isn't a backup:

```sh
docker compose cp certsforever:/data/backups/certs-<stamp>.db ./
# then upload to object storage / another machine
```

**List files on the volume** (the image has no shell, so use a throwaway container):

```sh
docker run --rm -v certsforever_certs-data:/data busybox ls -la /data /data/backups
```

Suggested schedule until Litestream lands (Milestone 7): a nightly cron on
the host that runs the backup command above, copies the file off-host, and
keeps 30 days. Test a restore (§5) at least monthly.

## 5. Restore

1. Stop the server. A restore must not race live writes.
   ```sh
   docker compose stop certsforever
   ```
2. Put the backup on the volume, if it isn't there already.
   ```sh
   docker compose cp ./certs-<stamp>.db certsforever:/data/backups/restore.db
   ```
3. Restore. The command checks the backup's integrity, keeps the current
   database as `/data/certs.db.before-restore-<stamp>`, removes stale WAL/SHM
   files and swaps the backup in.
   ```sh
   docker compose run --rm certsforever restore -yes /data/backups/restore.db
   ```
4. Start the server and check it.
   ```sh
   docker compose start certsforever
   curl -s http://127.0.0.1:8080/readyz
   ```

## 6. Roll back a bad release

1. Check out (or set the image tag to) the previous version.
2. If the bad release applied a migration, the old build will refuse to
   start (downgrade guard). Restore the pre-migration snapshot it wrote:
   ```sh
   docker compose stop certsforever
   docker run --rm -v certsforever_certs-data:/data busybox ls /data   # find certs.pre-NNN-*.db
   docker compose run --rm certsforever restore -yes /data/certs.pre-NNN-<stamp>.db
   ```
   Any certificates issued after the upgrade are lost, so export or note
   them first if there were any.
3. `docker compose up -d --build`

## 7. Rotate the admin token

```sh
openssl rand -hex 32         # new token
$EDITOR .env                 # replace CERTS_ADMIN_TOKEN
docker compose up -d         # recreates the container with the new env
```

To keep the token out of `.env`, mount it as a file and set
`CERTS_ADMIN_TOKEN_FILE=/run/secrets/admin_token` instead. Setting both
variables is a startup error.

## 7a. People and access

Everyday access is managed in the browser. Platform administrators work at
`/super`, and client administrators at `/admin/{client}`. Every change lands
in the audit log. These CLI commands are for the cases the browser can't
handle:

| Situation | Command |
|---|---|
| Email isn't set up, or a link didn't arrive | `docker compose exec certsforever certsforever login-link EMAIL` |
| Someone lost their phone (two-step) | `… certsforever user reset-2fa EMAIL`; they set it up again at next sign-in |
| Someone leaves, or an account may be compromised | `… certsforever user disable EMAIL`: ends their sessions, voids their unused links |
| Locked out of every platform admin account | `… certsforever superadmin add EMAIL` prints a fresh sign-in link |
| Check who's a platform admin | `… certsforever superadmin list` |

A printed sign-in link is as good as a password for an hour. Send it over a
channel you trust, or open it yourself.

## 7b. Email

**Setup**

1. Pick a transactional email provider: Postmark, Amazon SES, Resend,
   Mailgun, or your organization's SMTP relay.
2. Verify the sending domain with SPF, DKIM and DMARC records in DNS.
3. Set in `.env`:
   ```sh
   CERTS_SMTP_URL=smtp://USER:PASSWORD@smtp.provider.example:587   # STARTTLS; or smtps://…:465
   CERTS_MAIL_FROM=CertsForever <certs@your-domain.org>
   CERTS_BOUNCE_WEBHOOK_TOKEN=…   # openssl rand -hex 32
   ```
4. `docker compose up -d`, then check it end to end:
   ```sh
   docker compose exec certsforever certsforever email test you@your-domain.org
   ```
5. In the provider, point the bounce and spam-complaint webhook at
   `https://<domain>/hooks/email/bounce`. Authenticate with basic auth
   (password = the token), `Authorization: Bearer <token>`, or `?token=<token>`.

**How delivery works**
- **Queue:** every email is a row in the database, and the server's worker
  delivers it. Mail queued from the CLI (`import -email`) is picked up within
  about 15 seconds.
- **Temporary failures** retry after 1, 2, 4 … minutes, up to 8 attempts
  (about 2 hours). After that the message is marked failed.
- **Permanent rejections:** an SMTP 5xx refusing the *recipient*, a hard
  bounce, or a spam complaint suppresses the address. Nothing more is sent to
  it, and the student shows "email bounced" in their client's console. Other
  5xx errors, such as bad SMTP credentials, are treated as configuration
  problems and retried. A broken setup never suppresses good addresses.
- **Encrypted bodies:** message bodies are encrypted with `CERTS_MASTER_KEY`
  and wiped when delivered. Failed bodies are kept 7 days, so they can be
  retried, then wiped. Rows are deleted after 90 days.
- **At-least-once delivery:** a crash mid-send can send a message twice.
- **If the master key changes,** queued messages can't be decrypted and are
  marked failed.

| Situation | What to do |
|---|---|
| Is mail flowing? | `/super/system`, or `… certsforever email status` |
| Messages failing | Read the error on `/super/system`. Fix SMTP settings, then **Retry** (or `email retry ID`) |
| A student says they never got their link | Check whether they're suppressed. Fix the address or `email unsuppress`, then `claim-link -email CERT_ID` |
| Someone asks to never be emailed | **Suppress address** on `/super/system`, or `email suppress ADDRESS` |
| Stop publish reminders for a client | `… certsforever client update -slug zcw -reminders off` |

## 8. Logs

```sh
docker compose logs -f certsforever
```

Logs are JSON lines on stdout, rotated by Docker (10 MB × 5). Claim tokens
and sign-in tokens are never logged (paths are shown as `/claim/…` and
`/login/…`), and successful `/readyz` and `/healthz` probes are not logged.
In development without SMTP, emails, *including sign-in links*, are written
to the log. Production never does this.

## 9. Releases and CI

- **Every push and PR** runs `.github/workflows/ci.yml`:
  - gofmt and `go mod tidy` checks, `go mod verify`
  - `go vet`, staticcheck, govulncheck
  - tests with the race detector
  - a Docker build and `scripts/docker-smoke.sh`
  - Compose file validation
- **A tag push** (`git tag v0.1.0 && git push --tags`) runs `release.yml`:
  tests and the smoke test, then a multi-arch image (amd64 + arm64)
  published to `ghcr.io/<owner>/<repo>` as `0.1.0`, `0.1` and `latest`.
- **Dependabot** opens weekly PRs for Go modules, Actions and the base images.

Run the smoke test locally against any image:

```sh
docker build -t certsforever:local .
scripts/docker-smoke.sh certsforever:local
```

## 10. Configuration reference

| Variable | Default | Notes |
|---|---|---|
| `CERTS_ENV` | `development` (image: `production`) | Production requires an https `CERTS_BASE_URL` on a real domain |
| `CERTS_BASE_URL` | `http://localhost:8080` | Origin only, no path. Permanent. |
| `CERTS_ADDR` | `:8080` | |
| `CERTS_DB` | `certs.db` (image: `/data/certs.db`) | Its directory must exist |
| `CERTS_ADMIN_TOKEN` / `_FILE` | empty (admin API off) | 32+ characters |
| `CERTS_PLATFORM_NAME` | `CertsForever` | Name on pages that belong to no single client |
| `CERTS_CLIENT` | empty | Default `-client` for CLI commands |
| `CERTS_DOMAIN` | — | Compose `tls` profile only |
| `CERTS_MASTER_KEY` / `_FILE` | dev key (production: required) | 32 bytes hex/base64; encrypts two-step secrets. Back it up |
| `CERTS_SMTP_URL` / `_FILE` | empty | `smtp://user:pass@host:587` or `smtps://…:465` |
| `CERTS_MAIL_FROM` | empty | Required with SMTP |
| `CERTS_TRUST_PROXY` | `false` (compose: `true`) | Real client IP from `X-Forwarded-For` for rate limits and the audit log |
| `CERTS_BOUNCE_WEBHOOK_TOKEN` / `_FILE` | empty (webhook off) | 32+ chars; authenticates `POST /hooks/email/bounce` |

Invalid configuration stops startup with every problem listed at once.

Client branding (name, site, blurb, LinkedIn company ID) is data, not
configuration. Manage it with `certsforever client …`, inside the container:

```sh
docker compose exec certsforever certsforever client list
docker compose exec certsforever certsforever client update -slug zcw -linkedin-org 1234567
```
