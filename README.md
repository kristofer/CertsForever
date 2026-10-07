# CertsForever

Self-hosted, verifiable course-completion certificates for Zip Code Wilmington
students: a permanent public page per certificate, one-click "Add to LinkedIn
profile", a generated share image, and referral tracking back to the program
site.

Design, schema and routes are in [DESIGN.md](DESIGN.md). The path to a
multi-client production service is [docs/PRODUCTION_PLAN.md](docs/PRODUCTION_PLAN.md);
operations are in [docs/RUNBOOK.md](docs/RUNBOOK.md).

## Run it with Docker

```sh
cp .env.example .env     # for a local try: CERTS_ENV=development, CERTS_BASE_URL=http://localhost:8080
docker compose up -d --build
docker compose exec certsforever certsforever demo
```

On a server with a domain, `docker compose --profile tls up -d --build` adds
Caddy with automatic HTTPS. Backups, upgrades and restores are covered in
the [runbook](docs/RUNBOOK.md).

## Quick start

Requires Go 1.23+ and a C compiler (the SQLite driver uses cgo; on macOS the
Xcode command-line tools are enough).

```sh
go test ./...
go run ./cmd/certsforever demo    # seeds certs.db with one public sample certificate, prints its URLs
go run ./cmd/certsforever serve   # serves on http://localhost:8080
```

Open the "public page" URL that `demo` printed. Then open the "claim page"
URL to see what a student sees.

## Administrators and sign-in

People manage CertsForever in the browser. There are two roles:

- **Platform administrators** run the whole service: clients, people,
  custom domains, the audit log and system health. They can act as any
  client to help it. That's a deliberate step: a banner shows until they
  stop, and the client's audit log records everything they do.
- **Client administrators** manage only the clients they've been invited to.
  To them, other clients look as if they don't exist.

There are no passwords. Each time you sign in at `/login`, you're emailed a
single-use link that expires in 15 minutes. Platform administrators also need
an authenticator app (two-step verification, TOTP). They set it up at first
sign-in. Client administrators can turn it on from their account page.

Bootstrap the first platform administrator from the CLI. It prints a one-time
sign-in link, so no email setup is needed to get started:

```sh
certsforever superadmin add you@example.org -name "Your Name"
```

From there, everything is in the browser:
- `/super`: the platform console.
  - **Clients:** create (optionally inviting the first admin), edit, suspend
    with a reason, manage admins and custom domains, and act as a client.
  - **People:** disable, reset two-step, sign out everywhere, and platform
    admin rights.
  - **Audit log:** filterable.
  - **System:** database, migrations, snapshots and the email queue.
- `/admin/{client}`: the client console (see below).
- `/account`: two-step setup, and signing out other devices.

Other commands:

```sh
certsforever login-link EMAIL          # one-time sign-in link, when email isn't set up
certsforever superadmin list | remove EMAIL
certsforever user reset-2fa EMAIL      # lost phone: remove two-step verification, sign out everywhere
certsforever user disable EMAIL        # block sign-in and end all sessions
```

Every change is recorded in an append-only audit log, whether it's made in the
consoles, through the admin API (`admin-token`) or from the CLI (`cli:$USER`).
The database itself refuses edits and deletions of audit entries.

## Clients

CertsForever serves several organizations ("clients"), such as Zip Code
Wilmington and TwinArrows. Each client has its own courses, students,
branding, and certificate ID prefix (`ZCW-…`, `TWA-…`). Every client's data is
kept apart by the code and by the database schema.

```sh
certsforever client add -slug zcw -name "Zip Code Wilmington" -prefix ZCW \
  -site https://zipcodewilmington.com -linkedin-org 1234567 \
  -blurb "Zip Code Wilmington is a nonprofit coding bootcamp in Wilmington, Delaware."
certsforever client list
certsforever client update -slug zcw -linkedin-org 7654321   # only the flags given change
certsforever client suspend -slug zcw -reason "…"           # can't issue; certificates still resolve
```

The prefix is part of every certificate URL, so it can't change once the
client has issued a certificate.

## The landing page

The platform domain's home page (`/`) introduces the service to schools: what
graduates' posts look like, how issuing works, the features, and a box for
employers to verify a credential ID. It uses `CERTS_PLATFORM_NAME`, and
`CERTS_CONTACT_EMAIL` if set. On a client's own domain, `/` is that client's
verify page instead. The page is `internal/web/templates/landing.html` with
its own stylesheet, `static/landing.css`. Its example share image
(`static/landing-share.png`) was made with the app's share-image renderer.

The Laurel Posts logo is in `docs/brand/laurel-posts-logo.jpg`. The web
assets cut from it are `static/emblem.png` and `static/emblem-sm.png` (the
signpost and wreath, on a transparent background), `favicon.png`,
`apple-touch-icon.png` and `logo-og.jpg` (the link preview for the home
page). `docs/brand/make-assets.py` remakes them from the logo:

```sh
python3 docs/brand/make-assets.py docs/brand/laurel-posts-logo.jpg internal/web/static
```

Afterwards it quantizes the PNGs to 256 colors to keep them small (see the
script's notes). The sign-in page, consoles and verify page show the emblem
and use the logo's green. A school's certificate pages show only the school.

## Custom domains

A client can have its certificates on its own domain, e.g.
`certs.zipcodewilmington.com`:

1. The client adds a CNAME pointing it at the platform domain.
2. A platform admin verifies it on the client's page.
3. The admin chooses *Use for links*.

From then on, every certificate link uses that domain, and certificate URLs
on the platform domain answer with a 301 there, so links already on LinkedIn
keep working. Only public pages are served on a client's domain. Sign-in and
the consoles stay on the platform domain.

With `docker compose --profile tls`, Caddy gets TLS certificates for client
domains on their first request. It asks CertsForever first, so only verified
domains get one. See `deploy/Caddyfile` and runbook §7a.

## The client console

Client admins work in the browser at `/admin/{client}`, with no terminal:

- **Overview:** issued, opened and public rates, page views and visits to
  your site, plus recent activity.
- **Issue:** upload a CSV. A dry run marks every row (new, already issued,
  repeated, error) before anything is issued, and students can be emailed in
  the same step.
- **Certificates:** search and filter. Each certificate has a name
  correction (in place, so the link stays valid), a new-link email, revoke,
  and its email and audit history. Export to CSV or JSON.
- **Cohorts:** roster with who has opened their certificate; remind those
  who haven't.
- **Courses and Designs:** each course picks a design (heading, wording,
  accent color, logo, up to two signatures) with a live preview of the page
  and the LinkedIn share image. Certificates keep the design they were issued
  with.
- **Team, Settings, API:** invite admins and read the audit log; set the
  site, blurb, LinkedIn ID, reply-to and reminders; create API tokens.

The CLI below does the same jobs for operators and scripts.

## Issuing certificates to a cohort

```sh
# once per course
certsforever course -client zcw -slug java-fullstack \
  -title "Java Full-Stack Developer" -skills "Java,Spring Boot,SQL,REST APIs,Git"

# each cohort: CSV with email,full_name,course,cohort,completed_on
certsforever import -client zcw -email examples/cohort.csv      # emails each student their link
certsforever import -client zcw -out links.csv examples/cohort.csv   # or: write the links to a CSV yourself
```

`-client` can be left out when `CERTS_CLIENT` is set or only one client
exists.

With `-email`, each new student gets a "your certificate is ready" email:
- The sender's name is the client's, and Reply-To is the client's `-reply-to` address.
- It contains the student's private claim link.
- Without `-email`, `links.csv` contains the claim links for you to send.

Either way, certificates stay private until the student publishes them, and
re-importing the same file is safe.

If a student hasn't published after 7 days, they get one reminder with a
fresh link. Turn reminders off per client with
`certsforever client update -slug zcw -reminders off`.

## Email

All email goes through a queue in the database. The running server delivers
it in the background, retrying temporary failures with backoff (1, 2, 4 …
minutes, up to 8 attempts).
- **Encrypted bodies:** bodies contain sign-in and claim links, so they're
  stored encrypted with the master key and wiped once delivered.
- **Suppression:** if a receiving server permanently rejects an address
  (SMTP 5xx), the address is suppressed and nothing more is sent to it. The
  client console marks that student "email bounced".
- **Provider webhook:** set `CERTS_BOUNCE_WEBHOOK_TOKEN` and point your email
  provider's bounce and complaint webhook at `POST /hooks/email/bounce`. It
  accepts Postmark's format, or a generic
  `{"email": "…", "type": "bounce"|"complaint", "reason": "…"}`.

Platform administrators see the queue, failures (with Retry) and suppressed
addresses at `/super/system`. From the CLI:

```sh
certsforever email test you@example.org    # send now and report the result: checks your SMTP settings
certsforever email status                  # counts, failures, suppressed addresses
certsforever email retry 42
certsforever email unsuppress student@example.com
```

Other commands:

```sh
certsforever revoke -reason "issued in error" ZCW-XXXXXXXXXX   # client inferred from the prefix
certsforever claim-link ZCW-XXXXXXXXXX     # replace a lost claim link (prints it)
certsforever claim-link -email ZCW-XXXXXXXXXX   # replace it and email it to the student

certsforever backup backups/certs-2026-10-06.db   # consistent online backup
certsforever restore -yes backups/certs-2026-10-06.db   # server stopped first
certsforever migrate                       # apply migrations without serving
certsforever healthcheck                   # exit 0 if the local server is ready
certsforever version
```

## Configuration

| variable | default | |
|---|---|---|
| `CERTS_ENV` | `development` | `production` requires an https `CERTS_BASE_URL` on a real domain (the Docker image defaults to `production`) |
| `CERTS_ADDR` | `:8080` | listen address |
| `CERTS_DB` | `certs.db` | SQLite file |
| `CERTS_BASE_URL` | `http://localhost:8080` | public origin. **Set this in production**; it goes into every link and share image |
| `CERTS_ADMIN_TOKEN` | *(empty: admin API off)* | bearer token for `/admin/api/*`, 32+ chars; or `CERTS_ADMIN_TOKEN_FILE` |
| `CERTS_PLATFORM_NAME` | `CertsForever` | shown on pages that belong to no single client (the landing page, verify, errors) |
| `CERTS_CONTACT_EMAIL` | (none) | the landing page's "Email us about your program" address; empty hides the button |
| `CERTS_CLIENT` | *(empty)* | default `-client` for CLI commands |
| `CERTS_MASTER_KEY` | *(dev key)* | 32 bytes, hex or base64; encrypts two-step secrets. **Required in production**; or `CERTS_MASTER_KEY_FILE` |
| `CERTS_SMTP_URL` | *(empty)* | `smtp://user:pass@host:587` (STARTTLS) or `smtps://…:465`; or `CERTS_SMTP_URL_FILE` |
| `CERTS_MAIL_FROM` | *(empty)* | e.g. `CertsForever <certs@example.org>`; required with SMTP |
| `CERTS_TRUST_PROXY` | `false` | use the last `X-Forwarded-For` hop as the client IP (set by `compose.yaml`) |
| `CERTS_BOUNCE_WEBHOOK_TOKEN` | *(empty: webhook off)* | 32+ chars; authenticates `POST /hooks/email/bounce` (Bearer, basic-auth password, or `?token=`); or `_FILE` |

Without SMTP, development mode writes emails, including sign-in links, to the
server log, and production sends nothing (use `certsforever login-link`).

Configuration is validated at startup, and every problem is reported at once.
Organization branding (name, blurb, site, LinkedIn ID) used to be set by
`CERTS_ORG_*` variables. It now lives on each client; the old variables are
ignored, and the server logs a warning if they're still set.

## Admin API

People use the consoles. The JSON API is for scripts and automation. It's
authenticated by `CERTS_ADMIN_TOKEN`, which acts as a platform administrator,
and its actions are audited as `admin-token`. Repeated wrong tokens from one
address are rate-limited. Everything about one client's data lives under
`/admin/api/clients/{client}/`.

```sh
export T=your-admin-token H="Authorization: Bearer $T" API=localhost:8080/admin/api
curl -H "$H" -X POST $API/clients -d '{"slug":"twa","name":"TwinArrows","id_prefix":"TWA","site_url":"https://example.org"}'
curl -H "$H" -X PATCH $API/clients/twa -d '{"linkedin_org_id":"1234567"}'
curl -H "$H" -X POST $API/clients/twa/courses -d '{"slug":"data","title":"Data Engineering","skills":["Python","SQL"]}'
curl -H "$H" -X POST "$API/clients/twa/import?notify=true" -H 'Content-Type: text/csv' --data-binary @cohort.csv
curl -H "$H" $API/clients/twa/certificates?course=data
curl -H "$H" -X POST $API/clients/twa/certificates/TWA-XXXXXXXXXX/revoke -d '{"reason":"issued in error"}'
curl -H "$H" -X POST "$API/clients/twa/certificates/TWA-XXXXXXXXXX/claim-link?notify=true"
curl -H "$H" $API/clients/twa/stats
```

## Client API

Each client can create its own tokens on its API page, so an LMS or script
can issue certificates for that client and nothing else. The token decides
the client, so there's no client in the path, and actions are audited as
`api-token:<name>`.

```sh
H="Authorization: Bearer cfk_…"
curl -H "$H" -H 'Content-Type: text/csv' --data-binary @cohort.csv "https://certs.example.org/api/v1/import?notify=true"
curl -H "$H" "https://certs.example.org/api/v1/certificates?cohort=Spring%202026&claimed=no"
curl -H "$H" -X POST https://certs.example.org/api/v1/certificates/ZCW-XXXXXXXXXX/revoke -d '{"reason":"issued in error"}'
```

Also available: `GET /api/v1/certificates/{id}`, `POST …/{id}/claim-link`,
`GET`/`POST /api/v1/courses`, and `GET /api/v1/stats`.

## Layout

```
cmd/certsforever/        CLI + server entry point
internal/config/         env config, validation, *_FILE secrets
internal/buildinfo/      version stamped at build time
internal/store/          SQLite: writer/reader pools, checksummed migrations, backup/restore
internal/web/            handlers, templates, static assets (embedded)
internal/ogimage/        1200x627 share image (pure Go, bundled Go fonts), in each certificate's design
internal/imgnorm/        logo/signature uploads: PNG/JPEG only, size limits, re-encoded to PNG
internal/linkedin/       Add-to-profile and share URL builders
internal/certid/         certificate ID generation/normalization
internal/importer/       cohort CSV parsing
internal/totp/           RFC 6238 one-time codes (stdlib only)
internal/secretbox/      AES-256-GCM for secrets at rest (master key)
internal/ratelimit/      in-memory token buckets
internal/mail/           SMTP delivery (STARTTLS required off loopback), multipart, rejection classification
internal/emails/         email templates (text + HTML), branded per client
internal/outbox/         encrypted queue + delivery worker: retries, suppression, reminders
scripts/docker-smoke.sh  end-to-end check of a built image (used by CI)
deploy/Caddyfile         TLS reverse proxy for `docker compose --profile tls`
.github/                 CI, release-to-GHCR, Dependabot
```

Everything (templates, CSS, fonts, migrations) is embedded, so deployment is
one binary plus a SQLite file. Back up the SQLite file; it is the registry of
record.

## Development checks

These are the same checks CI runs:

```sh
gofmt -l . && go vet ./... && go test -race ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
docker build -t certsforever:local . && scripts/docker-smoke.sh certsforever:local
```
