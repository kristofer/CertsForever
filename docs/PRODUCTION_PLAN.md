# CertsForever — production plan

This plan takes the scaffold to a hosted, multi-client service. It keeps
SQLite and adds two admin levels:

- **Super admin** runs the platform. They create and manage *clients*
  (Zip Code Wilmington, TwinArrows, …) and invite each client's first admin.
- **Client admin** runs one client. For example, a Zip Code admin registers
  three courses, gives each its own certificate design, issues to cohorts, and
  manages their certificates, team and branding.

It builds on [DESIGN.md](../DESIGN.md). Where the two disagree, this plan
supersedes it.

---

## 1. What changes from the scaffold

| Area | Scaffold today | Production |
|---|---|---|
| Tenancy | single org, set by env vars | `clients` table; every row is scoped to one client |
| Certificate look | one hard-coded design | per-course **certificate designs** owned by the client |
| Admin auth | one shared bearer token | user accounts, email magic-link login, sessions, two roles |
| Admin UI | JSON API + CLI | server-rendered consoles: `/admin` (client) and `/super` (platform) |
| Issuing | CSV → mail-merge | CSV upload with a dry-run preview, then emails sent by the system |
| IDs | fixed `ZCW-` prefix | per-client prefix (`ZCW-`, `TWA-`), globally unique |
| Domains | one base URL | shared platform domain, plus optional custom domain per client |
| Ops | none | Litestream backups, Caddy TLS, monitoring, runbook |
| Accountability | none | audit log of every admin action |

The scaffold has no production data yet, so **rewrite `001_init.sql` as the
multi-tenant schema** instead of writing an `ALTER TABLE` migration. SQLite
can't add `NOT NULL` foreign-key columns without rebuilding the table, and
nothing needs preserving yet. Forward-only migrations start from there.

---

## 2. Roles and permissions

There are two levels, as requested. Internally they're represented so that a
third role (for example, an "issuer" who can issue but not change settings)
could be added later without a schema change.

| Capability | Super admin | Client admin |
|---|---|---|
| Create, rename or suspend clients | ✅ | — |
| Set a client's ID prefix and domains | ✅ (prefix locked after the first certificate) | view only |
| Invite or remove a client's admins | ✅ any client | ✅ own client |
| Manage super admins | ✅ | — |
| Branding: logo, colors, blurb, site URL, LinkedIn org ID, email sender name | ✅ | ✅ own client |
| Courses and certificate designs | ✅ (while acting as client) | ✅ own client |
| Issue, revoke, reissue, resend claim links | ✅ (while acting as client) | ✅ own client |
| Stats and export | all clients | own client |
| Audit log | all | own client |
| API tokens (for automation like Horizon) | — | ✅ own client |
| System health (backups, email queue, version) | ✅ | — |

**Acting as a client.** A super admin can enter any client's console. The
session records which client they are acting as, every page shows a banner,
and every action is written to the audit log with `impersonated = 1`. This is
how support happens without sharing passwords.

**Users in more than one client.** A user can belong to several clients. After
login, they pick a client; one membership skips straight to that client's
console.

**Enforcement.** Authorization lives in one place, not scattered through handlers:

- Middleware resolves `session → Principal{user, is_super, memberships}`.
- Client routes are `/admin/{client}/…`. Middleware resolves `{client}`, checks
  `authz.Can(principal, action, client)`, and puts a `Scope{ClientID}` in the
  request context.
- **Every store method for client-owned data takes a `Scope` argument**. There
  is no unscoped `GetCourse(id)`, so a missed check fails to compile instead of
  leaking data.
- The schema backs this up with composite foreign keys (§3), so a certificate
  can't reference another client's course even if the code is wrong.
- A test suite logs in as client A and tries every client-B URL and API call.
  It must get 404 every time.

---

## 3. Data model (SQLite)

```
clients ─┬─< client_domains
         ├─< memberships >── users ──< sessions, login_tokens
         ├─< invitations
         ├─< api_tokens
         ├─< assets (logos, signatures)
         ├─< certificate_designs ─┐
         ├─< courses ─────────────┘ (course → design)
         │     └─< cohorts
         ├─< students
         ├─< certificates (→ course, cohort, student; design snapshot)
         ├─< events  → event_daily (rollup)
         ├─< email_outbox
         └─< audit_log
```

### New tables

| Table | Purpose / key columns |
|---|---|
| `clients` | `slug` (unique, URL-safe), `name`, `id_prefix` (unique, 2–4 letters, immutable once used), `status` (`active`/`suspended`), `site_url`, `blurb`, `linkedin_org_id`, `logo_asset_id`, `brand` (JSON: colors, fonts), `email_from_name`, `reply_to` |
| `client_domains` | `host` (unique), `client_id`, `is_canonical`, `verified_at`. Hosts are never deleted, only marked non-canonical, so old links keep working. |
| `users` | `email` (unique, case-insensitive), `name`, `is_super_admin`, `totp_secret_enc`, `disabled_at`, `last_login_at` |
| `memberships` | `(user_id, client_id)` primary key, `role` (`admin` for now) |
| `invitations` | `client_id` (NULL means a super-admin invite), `email`, `role`, `token_hash`, `invited_by`, `expires_at`, `accepted_at` |
| `login_tokens` | magic-link tokens: `token_hash`, `user_id`, `expires_at` (15 min), `used_at` (single use) |
| `sessions` | `id_hash`, `user_id`, `acting_client_id`, `impersonating`, `created_at`, `last_seen_at`, `expires_at`, `ip`, `user_agent` |
| `api_tokens` | `client_id`, `name`, `token_hash`, `display_prefix`, `created_by`, `last_used_at`, `revoked_at` |
| `assets` | `client_id`, `kind` (`logo`/`signature`/`background`), `content_type`, `bytes` (BLOB), `sha256`, `width`, `height`. Uploads are decoded and re-encoded as PNG, and capped at about 512 KB. Keeping them in the database means one file holds everything to back up. |
| `certificate_designs` | `client_id`, `name`, `heading` ("Certificate of Completion"), `body_text` ("has successfully completed"), `layout`, `colors` (JSON), `logo_asset_id`, `signatories` (JSON: name, title, signature asset), `version` |
| `email_outbox` | `client_id`, `to_addr`, `template`, `payload` (JSON), `status` (`queued`/`sending`/`sent`/`failed`), `attempts`, `next_attempt_at`, `last_error`, `provider_message_id` |
| `audit_log` | `at`, `actor_user_id`, `actor_api_token_id`, `client_id`, `impersonated`, `action` (`course.create`, `cert.revoke`, …), `target_type`, `target_id`, `details` (JSON), `ip`. Append-only. |
| `event_daily` | rollup of `(client_id, course_id, day, kind) → count`. Raw `events` older than 90 days are pruned after rolling up. |
| `issuer_keys` | *(Milestone 9)* `client_id`, `kid`, `public_key`, `private_key_enc`, `created_at`, `retired_at` |

### Changed tables

- **Every client-owned table** (`courses`, `cohorts`, `students`, `certificates`,
  `events`, …) gets `client_id NOT NULL` and `UNIQUE(client_id, id)`. Child rows
  reference parents through composite keys, for example
  `FOREIGN KEY (client_id, course_id) REFERENCES courses(client_id, id)`.
  This is the database-level guarantee against tenant mix-ups.
- **`courses`**: `UNIQUE(client_id, slug)`, plus `design_id`. Each of Zip Code's
  three courses points at its own design. Designs are separate rows so two
  courses *can* share one, but by default each course gets its own.
- **`students`**: `UNIQUE(client_id, email)`. The same person at two clients is
  two unrelated rows.
- **`certificates`** additions:
  - `client_id` and `course_id`
  - `design_snapshot` (JSON): the design fields frozen at issue time, so editing
    a design never changes a certificate already on someone's LinkedIn
  - `replaces_id` and `superseded_by`: name corrections reissue rather than edit
  - `expires_on` (optional, NULL means it never expires)
  - `status` gains `superseded`

### Lifecycle rules

- **Name correction or typo** → reissue. The old certificate becomes
  `superseded`, and its URL 301-redirects to the new one. LinkedIn links never
  break.
- **Revoke** → the page stays up with a "Revoked" banner (unchanged from now).
- **Student asks to be removed** → set the certificate to private and anonymize
  the student row. The certificate ID then resolves to 404, like any private
  certificate.
- **Client suspended** → they can't log in or issue, but **public certificate
  pages keep resolving**. That's the "Forever" promise. A client is only
  deleted by a super admin, and only after an export.

### SQLite in production

- **Connections:** two `*sql.DB` handles. A write pool with `MaxOpenConns=1`
  and `_txlock=immediate` (no `SQLITE_BUSY` races), and a read pool of about 8.
- **Pragmas:** `journal_mode=WAL`, `synchronous=NORMAL`, `foreign_keys=ON`,
  `busy_timeout=5000`. Run `PRAGMA optimize` on shutdown and daily.
- **Backups:** **Litestream** replicates continuously to S3-compatible storage
  (Backblaze B2, Cloudflare R2 or S3). That gives a recovery point of about one
  second. Add a nightly `VACUUM INTO` snapshot kept for 30 days, and run a
  monthly restore drill.
- **Migrations:** embedded and forward-only. They run at startup, after an
  automatic `VACUUM INTO` pre-migration snapshot.
- **Scale check:** a few thousand certificates a year plus events is
  megabytes. One SQLite file on one VM has headroom of several orders of
  magnitude.
- **Driver:** Milestone 0 kept `mattn/go-sqlite3` (cgo; the Docker build
  handles the C toolchain). Switching to `modernc.org/sqlite` (pure Go) is
  still an option. It allows static builds and simpler cross-compiling, and you can
  fetch it on your machine. `mattn/go-sqlite3` is also fine. Either way the
  choice stays inside `internal/store`.

---

## 4. URLs and routes

### Domains and IDs

- **Platform domain:** for example `certs.<platform>.org`. Certificates live at
  `/c/{PREFIX}-{10 chars}`, and the ID is globally unique, so lookup never
  needs the host.
- **Optional custom domain per client:** for example
  `certs.zipcodewilmington.org`. The host selects branding and the canonical
  URL. Any certificate URL on any host the platform has ever served for that
  client must keep working, via a 301 to the canonical host. A request for
  client B's certificate on client A's domain returns a 301 to the right host.
- **TLS for custom domains:** Caddy's on-demand TLS, using an `ask` endpoint
  (`/internal/tls-ask?domain=`) so certificates are only obtained for hosts in
  `client_domains`.
- **ID normalization:** generalize `certid.Normalize` to accept any registered
  prefix.

### Public (unchanged paths, now branded per client)

`/`, `/verify`, `/c/{id}`, `/c/{id}/og.png`, `/c/{id}/credential.json`,
`/c/{id}/share/linkedin`, `/c/{id}/learn`, `/claim/{token}`,
`/claim/{token}/linkedin/add`, `/healthz`

### Auth

| Route | Purpose |
|---|---|
| `GET/POST /login` | Enter an email address. The response is always "check your email", so it never reveals whether an account exists. |
| `GET /login/{token}` | Consume the magic link and create a session. Super admins then go to TOTP. |
| `GET/POST /login/totp` | Second factor (required for super admins, optional for client admins). |
| `POST /logout` | End the session. |
| `GET/POST /invite/{token}` | Accept an invitation (sets the user's name). |

Magic links mean no passwords to store, reset or leak. Each link is
single-use, expires in 15 minutes, and is rate-limited per email address and
per IP.

### Client console — `/admin/{client}/…`

| Page | What it does |
|---|---|
| `/admin` | Pick a client, or redirect if the user has only one |
| `…/` | Dashboard: issued, published %, views, LinkedIn adds, learn-more clicks (30/90 days) |
| `…/courses`, `…/courses/new`, `…/courses/{slug}` | Course CRUD: title, description, skills, hours, design |
| `…/designs`, `…/designs/{id}` | Design editor with a **live preview** of both the certificate page and the 1200×627 share image; upload logo and signatures |
| `…/issue` | Upload CSV → **dry-run preview** (new / existing / errors per row) → confirm → emails queued |
| `…/cohorts/{id}` | Roster with published/claimed status; "remind unclaimed" button |
| `…/certificates`, `…/certificates/{id}` | Search and filter; detail page with revoke, reissue (name fix), resend claim email, events timeline |
| `…/team` | Invite or remove client admins |
| `…/api-tokens` | Create or revoke tokens for automation |
| `…/settings` | Branding, site URL, blurb, LinkedIn org ID, email sender name, reply-to |
| `…/audit` | This client's audit log |
| `…/export` | Download all certificates as CSV or JSON; later a static-site bundle |

### Super console — `/super/…`

| Page | What it does |
|---|---|
| `/super` | Platform overview: clients, issuance per client, email queue health, last backup |
| `/super/clients`, `/super/clients/new` | Create a client: slug, name, ID prefix, first admin's email (sends an invitation) |
| `/super/clients/{slug}` | Edit, manage domains, suspend or reactivate, list members, **Act as client** |
| `/super/users` | All users; disable; grant or revoke super admin |
| `/super/audit` | Global audit log, filterable |
| `/super/system` | Version, migrations, Litestream status, outbox (retry failed), disk usage |

### Machine API — `/api/v1/…` (client API token; the token implies the client)

`POST /api/v1/certificates` (single or batch issue) ·
`GET /api/v1/certificates?course=&cohort=` ·
`POST /api/v1/certificates/{id}/revoke` ·
`POST /api/v1/certificates/{id}/resend` ·
`GET /api/v1/courses`

This is the integration point for Horizon LMS to issue automatically on course
completion. It replaces the scaffold's `/admin/api/*`.

### Internal

`/readyz` (database reachable, migrations current) ·
`/metrics` (Prometheus, served on localhost only) · `/internal/tls-ask`

---

## 5. Frontend approach

- **Rendering:** server-rendered `html/template`, the same as the public
  pages. One binary, no Node build.
- **Interactivity:** vendor **htmx** (about 14 KB, served from `/static`, so
  the CSP stays `'self'`) for the live design preview, the CSV dry run, and
  inline revoke and resend.
- **Forms:** every form gets a CSRF token (double-submit, tied to the session),
  and every POST redirects to a GET.
- **Accessibility and layout:** keyboard-usable, labeled inputs, and works at
  phone width. Client admins will check rosters from their phones.

---

## 6. Email

- **Queue and worker:** `email_outbox` with a background worker goroutine.
  Retries use exponential backoff (1 min to 6 h, up to 8 attempts), then the
  message is marked `failed` and shown on `/super/system`.
- **Provider:** a small interface with an SMTP implementation first, so it
  works with Postmark, SES, Resend or Mailgun. Add an HTTP API implementation
  if needed.
- **Sending domain and identity:** send from the platform domain with SPF,
  DKIM and DMARC set up. The display name is per client ("Zip Code Wilmington
  Certificates"), and reply-to is the client's address.
- **Templates:** login link, invitation, "your certificate is ready" (claim
  link), and a reminder to publish (one nudge after 7 days if unpublished; the
  client can turn it off). Each has text and HTML parts.
- **Bounces:** a provider webhook marks the address. The cohort roster then
  shows "email bounced" so the admin can fix it.

---

## 7. Security checklist

- **Cookies:** `__Host-` prefix, `Secure`, `HttpOnly` and `SameSite=Lax`.
- **Sessions:** idle timeout of 12 h, absolute limit of 30 days, rotated on
  login. Only session IDs are stored server-side, hashed.
- **Stored secrets:** tokens of every kind (login, claim, invitation, API,
  session) are stored as SHA-256 hashes only.
- **Super admins:** TOTP is required. Super-admin status can only be granted by
  another super admin, or by the bootstrap CLI
  (`certsforever superadmin add you@example.org`).
- **Rate limits:** in-memory token buckets (fine on a single instance) on
  login, verify, claim and the API.
- **Uploads:** size caps; images are decoded and re-encoded (which strips
  metadata and payloads); CSVs are capped at 5 MB.
- **Headers:** CSP as now, plus HSTS (after the domain is final),
  `X-Frame-Options` and `Permissions-Policy`.
- **Secrets:** read from environment or files (systemd credentials or Docker
  secrets). A master key encrypts TOTP secrets and, later, signing keys.
- **Tooling:** `govulncheck` and `staticcheck` run in CI. Do a dependency
  review before launch.
- **Privacy:** publishing a student's name stays opt-in (it already is).
  Before launch, ask Zip Code's compliance contact whether completion records
  fall under FERPA. Opt-in publishing is the conservative default either way.
  Publish a privacy page and a student data-removal path.
- **Audit log:** append-only from the application; no update or delete paths.

---

## 8. Deployment and operations

| Area | Plan |
|---|---|
| Hosting | One small VM (Hetzner, DigitalOcean or Lightsail; 1–2 vCPU, 2 GB) or a single Fly.io machine with a volume. SQLite means **one instance**, which is fine at this scale. |
| Process | `certsforever serve` under systemd (or Docker), with Caddy in front for TLS, HTTP/2 and on-demand certificates for custom domains |
| Backups | Litestream as a systemd sidecar, plus nightly `VACUUM INTO` to object storage. A documented restore that you've actually run. |
| Deploys | GitHub Actions builds and tests; deploys copy the binary and restart. The restart is about a second; Caddy retries upstream errors, so it's effectively zero-downtime at this traffic. |
| Monitoring | An external uptime check on `/readyz` and one real certificate URL. Structured JSON logs. Alerts on email failures, Litestream lag and 5xx rate. |
| Error reporting | Optional Sentry (or log-based alerts to start) |
| Runbook | `docs/RUNBOOK.md`: deploy, roll back, restore from Litestream, rotate the master key, add a client domain, handle a removal request |

---

## 9. Milestones

Effort estimates are for focused build time and are rough.

| # | Milestone | Done when | Est. |
|---|---|---|---|
| **0** ✅ | **Foundations** (done 2026-10-06; Docker packaging added) | Driver decision; read/write pools; pragmas; migration runner with pre-migration snapshot; config validation; CI (vet, staticcheck, govulncheck, tests); `docs/RUNBOOK.md` started | 2–3 d |
| **1** | **Multi-tenant schema** | New `001_init.sql` with `clients` and composite FKs; `Scope`-required store API; per-client ID prefixes; tenant-isolation test suite passes | 3–4 d |
| **2** | **Accounts and roles** | Users, magic-link login, sessions, CSRF, TOTP for super admins, invitations, `authz.Can`, audit log, rate limits, `superadmin add` CLI | 4–5 d |
| **3** | **Email** | Outbox, worker, SMTP provider, four templates, bounce webhook, dev mode that logs emails instead of sending | 2 d |
| **4** | **Client console** | Courses, designs with live preview, CSV dry-run issue, cohort roster, certificate detail (revoke, reissue, resend), team, settings, stats, export, API tokens | 7–10 d |
| **5** | **Super console** | Client CRUD, suspend, domains, first-admin invite, act-as with banner and audit, users, global audit, system page | 3–4 d |
| **6** | **Per-client public experience** | Branded certificate page and share image driven by `design_snapshot`; host-based routing; canonical redirects; Caddy `ask` endpoint | 3–4 d |
| **7** | **Production hardening** | Deployed on the chosen host; Litestream and nightly snapshots; restore drill done; monitoring and alerts; security checklist walked through; small load test | 3–4 d |
| **8** | **Launch** | Zip Code onboarded (3 courses, 3 designs), LinkedIn org ID set, previews checked in LinkedIn Post Inspector, pilot cohort issued and claimed; then TwinArrows onboarded | 2–3 d |
| **9** | **After launch** | Open Badges 3.0 signing (Ed25519 per client, `/issuers/{slug}`); Horizon integration through `/api/v1`; static-export fallback; student portal ("all my certificates" by email login); optional PDF | ongoing |

**Total to launch:** about 30–40 days of focused work. Milestones 1–2 must
come first; 3, 5 and 6 can overlap with 4.

### Definition of done for "production"

- A super admin can create TwinArrows, invite its admin, and act as it, all
  from the browser with no CLI.
- A Zip Code admin can log in, set up three courses each with its own design,
  issue to a cohort from a CSV, and see claims and LinkedIn adds come in. They
  never touch a terminal.
- Client A can't see any client B data. This is proven by the test suite and by
  composite foreign keys.
- The database can be restored to a fresh VM from object storage in under 30
  minutes, by following the runbook.
- Every certificate URL ever issued still resolves, including after a
  suspension, a name correction or a domain change.

---

## 10. Decisions needed before Milestone 1

1. **Platform domain.** It becomes part of every certificate URL forever.
2. **Operator.** Which entity runs the platform (for example CodeHavn LLC, or
   Zip Code)? That determines who owns the terms, the privacy policy and any
   data agreement with TwinArrows.
3. **Custom domains at launch** or later? They aren't needed for Zip Code to
   start.
4. **Hosting provider** and **email provider**.
5. **Login method.** Magic link plus TOTP is recommended. Passkeys could come
   later.
6. **Billing.** Will clients pay? The schema tracks per-client issuance either
   way; billing itself is out of scope here.
7. **Design editing after issue.** Is freezing via `design_snapshot`
   (recommended) right, or should design changes restyle old certificates? The
   wording would stay frozen either way.
