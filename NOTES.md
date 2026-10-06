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

