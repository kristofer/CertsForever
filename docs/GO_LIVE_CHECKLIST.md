# Go-live checklist

Tick items as they're done (`[x]`) and commit. Details for each area are in
[PRODUCTION_PLAN.md](PRODUCTION_PLAN.md) (§ numbers below) and
[RUNBOOK.md](RUNBOOK.md).

CertsForever runs as **one server with SQLite** by design. The goal is fast,
rehearsed recovery rather than zero downtime, which is why the restore drill
is the most important item here.

## Decisions (before building further) — §10

- [ ] Permanent platform domain (it goes into every certificate URL, forever)
- [ ] Operating entity (e.g. CodeHavn LLC or Zip Code): owns the terms, privacy policy, and the TwinArrows data agreement
- [ ] Hosting provider
- [ ] Email provider
- [ ] Object-storage bucket for backups
- [ ] FERPA question answered by Zip Code's compliance contact (are completion records education records?)

## Product — Milestones 1–6 (§9)

- [x] **M1** Multi-client schema; every client-data store call takes a client scope; per-client ID prefixes; tenant-isolation tests pass
- [x] **M2** Login (magic link), sessions, CSRF, both admin roles, TOTP for super admins, audit log, rate limits
- [x] **M3** System-sent email (outbox, retries, bounce handling)
- [x] **M4** Client console: courses, certificate designs, CSV issue with dry run, roster, revoke / name correction / resend, team, settings, stats, export
- [x] **M5** Super console: clients, suspend, domains, first-admin invite, act-as with audit
- [x] **M6** Per-client branding on certificate pages and share images (name, blurb, site and LinkedIn ID already come from the client record since M1; colors and logos from certificate designs in M4); custom domains with canonical redirects and Caddy on-demand TLS

## Code quality — Milestone 0 (§9)

- [x] Lint, tests with the race detector, and a vulnerability scan on every push (CI)
- [x] Docker image: non-root, read-only filesystem, health check, smoke-tested
- [x] Startup config validation; checksummed migrations with a downgrade guard and pre-migration snapshots
- [x] `backup` and `restore` commands; restore drilled in Docker
- [ ] First green CI run on GitHub (including govulncheck against the real vulnerability database)
- [ ] First release image published to GHCR from a version tag

## Infrastructure — Milestone 7 (§8)

- [ ] Server set up: firewall allows only 22/80/443, SSH by key only, automatic security updates
- [ ] DNS for the platform domain; HTTPS working through Caddy
- [ ] If a client wants its own domain at launch: CNAME in place, verified on its page, *Test it* shows its verify page over HTTPS, then *Use for links* (runbook §7a)
- [ ] HSTS turned on (only after the domain is final)
- [ ] Email sending domain authenticated: SPF, DKIM, DMARC
- [ ] Secrets only in `.env` or secret files (never in git); admin token freshly generated
- [ ] `CERTS_MASTER_KEY` generated and stored with the backups (separately from the server)
- [ ] First platform administrator created (`superadmin add`) and two-step verification set up
- [ ] SMTP configured; `certsforever email test` succeeds; a sign-in email received end to end
- [ ] Provider's bounce/complaint webhook pointed at `/hooks/email/bounce` with `CERTS_BOUNCE_WEBHOOK_TOKEN`; a test bounce shows on `/super/system`
- [ ] Each client's Reply-To set (`client update -reply-to`), so student replies reach the school
- [ ] Continuous backups with Litestream to object storage
- [ ] Nightly off-server backup copy, kept 30 days
- [ ] **Restore drill on a fresh server, in under 30 minutes, following the runbook**

## Monitoring (§8)

- [ ] External uptime check on `/readyz`
- [ ] External uptime check on one real public certificate URL
- [ ] Alerts: 5xx rate, email failures, backup lag, low disk
- [ ] Logs retained long enough to investigate an incident

## Security review (§7)

- [ ] Security checklist walked through: cookies, CSRF, super-admin TOTP, upload limits, rate limits, headers
- [ ] Tried to reach client B's data while logged in as client A, by hand (as well as in the tests). Include the M4 routes: certificates, designs (and previews), cohorts, tokens, and `/api/v1` with client A's token
- [ ] Privacy page published
- [ ] Student removal-request path working

## Launch (§9, Milestone 8)

- [ ] Zip Code client set up: 3 courses, 3 certificate designs
- [ ] Zip Code's LinkedIn company ID configured (logo shows on "Add to profile")
- [ ] Share previews checked in LinkedIn's Post Inspector
- [ ] Pilot cohort issued; some students publish and add certificates to their profiles
- [ ] Rollback rehearsed: previous version deployed and the pre-migration snapshot restored
- [ ] TwinArrows onboarded
