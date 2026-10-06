# CertsForever

Self-hosted, verifiable course-completion certificates for Zip Code Wilmington
students: a permanent public page per certificate, one-click "Add to LinkedIn
profile", a generated share image, and referral tracking back to the program
site.

Design, schema and routes are in [DESIGN.md](DESIGN.md).

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

## Issuing certificates to a cohort

```sh
# once per course
go run ./cmd/certsforever course -slug java-fullstack \
  -title "Java Full-Stack Developer" -skills "Java,Spring Boot,SQL,REST APIs,Git"

# each cohort: CSV with email,full_name,course,cohort,completed_on
go run ./cmd/certsforever import -out links.csv examples/cohort.csv
```

`links.csv` contains each student's private `claim_url`. Email it to them;
certificates stay private until the student publishes from that page.
Re-importing the same file is safe.

Other commands:

```sh
certsforever revoke -reason "issued in error" ZCW-XXXXXXXXXX
certsforever claim-link ZCW-XXXXXXXXXX     # replace a lost claim link
```

## Configuration

| variable | default | |
|---|---|---|
| `CERTS_ADDR` | `:8080` | listen address |
| `CERTS_DB` | `certs.db` | SQLite file |
| `CERTS_BASE_URL` | `http://localhost:8080` | public origin. **Set this in production**; it goes into every link and share image |
| `CERTS_ADMIN_TOKEN` | *(empty: admin API off)* | bearer token for `/admin/api/*` |
| `CERTS_LINKEDIN_ORG_ID` | *(empty)* | Zip Code's numeric LinkedIn company ID, so the logo shows on profiles |
| `CERTS_SITE_URL` | `https://zipcodewilmington.com` | "Learn about the program" destination (UTM-tagged) |
| `CERTS_ORG_NAME` | `Zip Code Wilmington` | |
| `CERTS_ORG_BLURB` | *(one sentence)* | shown on every certificate page |

## Admin API

```sh
export T=your-admin-token
curl -H "Authorization: Bearer $T" -X POST localhost:8080/admin/api/courses \
  -d '{"slug":"data","title":"Data Engineering","skills":["Python","SQL","Spark"]}'
curl -H "Authorization: Bearer $T" -X POST localhost:8080/admin/api/import \
  -H 'Content-Type: text/csv' --data-binary @examples/cohort.csv
curl -H "Authorization: Bearer $T" localhost:8080/admin/api/stats
```

## Layout

```
cmd/certsforever/        CLI + server entry point
internal/store/          SQLite access, embedded migrations
internal/web/            handlers, templates, static assets (embedded)
internal/ogimage/        1200x627 share image (pure Go, bundled Go fonts)
internal/linkedin/       Add-to-profile and share URL builders
internal/certid/         certificate ID generation/normalization
internal/importer/       cohort CSV parsing
```

Everything (templates, CSS, fonts, migrations) is embedded, so deployment is
one binary plus a SQLite file. Back up the SQLite file; it is the registry of
record.

## Deploying

`docker build -t certsforever .` produces a small image that keeps its
database in `/data`. Put it behind TLS (Caddy, a load balancer, or Fly.io and
similar), set `CERTS_BASE_URL` to the permanent `https://` origin, and mount
`/data` on a persistent volume.
