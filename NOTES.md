I'll look at what's in the CertsForever folder first, then write the schema and routes and scaffold a Go project that builds and runs.

My `git status` left an empty `.git/index.lock` behind because this shell can't delete files, and that lock would block your git commands. I'll try moving it out of the way.

The project is going into one Go binary with a SQLite store, server-rendered pages, an Open Graph image renderer, a token-protected admin API, and a CLI for importing cohorts.

Now the web layer.

Now the server tests and the CLI entry point.

The only failure is in the page itself: the meta description still says "Verified" on a revoked certificate. Fixing the template.

Tests pass. Now I'll run the real binary with the demo data and look at the page and the share image.

The method checks out: my computed hash for `mattn/go-sqlite3` matches the one Go recorded itself. Now I'll write the final `go.mod` and `go.sum` without the replace directives.

The build passes in strict read-only module mode, so `go.mod` and `go.sum` are correct as shipped. Next: the design doc, README, Makefile, Dockerfile, and a sample CSV.

The CLI import works, and re-running it is idempotent. Copying the 33 files into your CertsForever folder now.

Your computer blocks remote tools from writing a file named `Makefile`, so I'll leave it out and change the README to plain `go run` commands rather than work around the block.

I've written the schema and routes sketch as `DESIGN.md` and scaffolded the Go project into your CertsForever folder. The tests pass, and I ran the server on demo data and checked the certificate page (light and dark, desktop and phone width), the student's claim page, and the share image. The Dockerfile is written but I haven't built it.

**Schema** (SQLite)
- **Tables:** ` ` ` `students`, plus an `events` table.
- **Certificates don't change once issued.** The student's name, course title and skills are copied in at issue time; only revoked status and public/private visibility can change later.
- **IDs:** each certificate gets an unguessable ID like `ZCW-7K3M9QF2XA`. Typed-in IDs are cleaned up, so lowercase or O-for-0 mistakes still resolve.
- **Analytics:** the `events` table counts views, share-image fetches, LinkedIn adds, shares and "learn more" clicks. It stores no IPs and sets no cookies.

**Routes**
- **Public:**
  - `/c/{id}` is the certificate page, with the social preview tags.
  - `/c/{id}/og.png` is the 1200Certificates627 share image.
  - `/c/{id}/credential.json` is a machine-readable record.
  - `/c/{id}/share/linkedin` and `/c/{id}/learn` count the click, then redirect. The second goes to your site with UTM tags.
  - `/verify` lets anyone look up a certificate by ID.
- **Student:** each student gets a private claim link, `/claim/{token}`. Certificates start private, and the student publishes from that page. "Add to LinkedIn profile" only appears there, not on the public page employers see.
- **Admin:** a JSON API under `/admin/api/*`, protected by a token. It covers creating courses, importing a cohort CSV, listing, revoking, replacing a lost claim link, and stats.
- **CLI:** `course`, `import`, `revoke`, `claim-link` and `demo`. Importing writes a CSV of claim links you can mail-merge, and re-importing the same file doesn't create duplicates.

To try it: `go run ./cmd/certsforever demo`, then `go run ./cmd/certsforever serve`, and open the URLs `demo` printed.

Things you should know:
- **SQLite driver:** it uses `mattn/go-sqlite3`, which needs a C compiler (the Xcode command-line tools are enough on a Mac). The pure-Go `modernc.org/sqlite` would be nicer, but I couldn't download it here to test. Switching is one import and the connection string.
- **No Makefile:** your computer blocks remote tools from writing a file with that name, so the README uses `go run` commands instead.
- **Stale git lock file:** an early `git status` of mine left a lock file in `.git` that I couldn't delete, and it would have blocked your git commands. I renamed it to `.git/index.lock.stale-from-claude`; you can delete it.
- **Not committed:** nothing is committed to git.

Before launch, choose the permanent domain (it becomes `CERTS_BASE_URL`), and set `CERTS_LINKEDIN_ORG_ID` to Zip Code's LinkedIn company ID so your logo shows on students' profiles. The colors and fonts are placeholders until you swap in Zip Code's branding.` cohorts` courses` 


**** Andother Note

I'll check what tooling is available, then do the work: Docker packaging, the database-layer hardening, and the CI workflows.

You've already run it locally: there's a `certs.db` and a new `NOTES.md`. I'll read your notes and check whether you changed any of my files, so I build on your current versions.

Docker runs in my workspace. Checking whether it can pull base images.

I'll write the code first, then build the real Dockerfile against local stand-in images and run the same smoke test CI will run.

The store builds. Now the store tests.

All pass. Checking that every test actually ran, then testing the upgrade against a copy of your real `certs.db`:

Your existing database opens cleanly: the checksum was backfilled and your demo certificate is intact. Next: config validation, build info, `/readyz`, and the CLI commands for operations.

Now the CLI: `serve` validates config and logs the migration report, plus new `migrate`, `backup`, `restore`, `healthcheck` and `version` commands.

All green with the race detector on. I'm adding a `/readyz` assertion to the web tests, then writing the Docker files.

Caddy fails to start on an empty `email` directive, so I'll make `ACME_EMAIL` required in the TLS profile. Then I'll write the smoke-test script that CI will also run.

Both stand-ins are ready. I'll serve the module cache over local HTTP so the build's `go mod download` works, then build the real Dockerfile unchanged except for the `GOPROXY` build argument.

The image builds and is 20 MB. Now the smoke-test script, which CI runs against every image build:

All smoke checks pass. Next, testing `compose.yaml` with an `.env` made from the example: the default profile and the TLS profile's config.

Default profile works now. Next, the GitHub Actions workflows: CI on every push and PR, and a release workflow that publishes images on version tags.

The GitHub API is blocked, but `git` access to GitHub works. I'll build `staticcheck` and `actionlint` from source through GitHub mirrors, so the lint steps CI will run get checked here first.

`staticcheck` is clean. Now `actionlint` for the workflow files:

`actionlint` passes on both workflows. Rebuilding the image with the final code, then rerunning the smoke test and doing a restore drill inside Docker:

The restore drill works end to end: back up, revoke, stop, restore, and the certificate is valid again, with the replaced database kept. Now the runbook and README updates.

I'll check that the CI's "`go mod tidy` is clean" step will pass, then run all checks one last time.

The executable bit on the smoke script may not survive the copy to your Mac, so I'll have the workflows call it with `bash` explicitly.

All 24 files match byte for byte. Sending you the three GitHub files:

CertsForever now runs in Docker, and Milestone 0 is done and in your folder. The only exception is the three GitHub Actions files: your computer blocks remote tools from writing into `.github/`, so I sent them as attachments. Put `ci.yml` and `release.yml` in `.github/workflows/` and `dependabot.yml` in `.github/`.

**Docker**
- **Running it:** copy `.env.example` to `.env` and edit it, then run `docker compose up -d --build`. Adding `--profile tls` puts Caddy in front for automatic HTTPS once DNS points at the server.
- **The image:** 20 MB. It runs as a non-root user with a read-only filesystem and keeps the database on a volume. Docker's health check uses a new `/readyz` endpoint.
- **What I tested:** Docker Hub and gcr.io are blocked here, so I built your actual Dockerfile against local stand-ins for the Go and distroless base images. The image passes the smoke-test script CI will run: it refuses an invalid production config, starts healthy as non-root, issues and serves a certificate and share image, enforces the admin token, takes a backup, and keeps its data across a restart.
- **Restore drill:** back up, revoke a certificate, stop, restore. The certificate came back valid.
- **Not tested:** the build against the real base images, and Caddy itself. The first CI run will cover the base images.

**Database**
- **Two connection pools:** one writer whose transactions queue instead of failing with "database is locked", plus a read-only pool. A test with 16 concurrent writers and 16 readers has no lock errors.
- **Safer migrations:** each migration's checksum is recorded, so an edited migration stops startup. An older build refuses to start against a newer database. Before migrating a database that has data, it saves a snapshot automatically.
- **Backup and restore:** `backup` makes a consistent copy while the server is running. `restore -yes` checks the backup's integrity, keeps the current database, and swaps the backup in.
- **Your local `certs.db`:** I tested the upgrade on a copy. It opens fine and your demo certificate is intact.

**Configuration** is checked at startup, and every problem is reported at once. In production the base URL must be `https` on a real domain, and the admin token must be at least 32 characters. The token can also be read from a file, for Docker secrets.

**Automated tests and builds**
- Every push and pull request runs formatting and lint checks, a vulnerability scan, the tests with Go's race detector, and a Docker build plus the smoke test.
- Pushing a version tag such as `v0.1.0` publishes an image to GitHub's container registry for both Intel/AMD and ARM servers.
- Dependabot opens weekly update pull requests.

`staticcheck` and `actionlint` pass here. The vulnerability scan (`govulncheck`) couldn't run because its database is blocked, so its first real run will be in CI.

`docs/RUNBOOK.md` covers first deploy, upgrades, backups, restores, rollbacks and rotating the admin token. Until automatic backups arrive in Milestone 7, set up the nightly backup cron it describes.

Nothing is committed to git. Once the `.github` files are in place, committing and pushing to GitHub will trigger the first CI run.
