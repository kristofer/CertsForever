# CertsForever — design sketch

> This describes the single-organization scaffold. The multi-client production
> build (super admins, client admins, per-course designs) is planned in
> [docs/PRODUCTION_PLAN.md](docs/PRODUCTION_PLAN.md), which supersedes this
> document where they differ.

Self-hosted completion certificates for Zip Code Wilmington. Students get a
permanent public page they can add to LinkedIn; employers can verify it; and
every shared link is a small, measurable referral back to the program.

## Principles

1. **The URL is the certificate.** `https://<certs-domain>/c/ZCW-7K3M9QF2XA`
   is what goes on LinkedIn, résumés and email signatures. It must never
   change, so the domain and `/c/{id}` path are permanent decisions.
2. **Issued certificates are immutable.** Name, course title and skills are
   snapshotted at issue time. Only status (revoked) and visibility change.
3. **Students consent before anything is public.** Certificates start private;
   the student publishes from a private claim link.
4. **Revocation is visible.** A revoked public certificate still resolves and
   says "Revoked". A verifier must never mistake a revoked certificate for a
   missing one, or a missing one for a valid one.
5. **Analytics without tracking people.** Count views, shares and click-throughs
   per certificate. No cookies, IPs or user agents are stored.

## Data model (SQLite)

```
courses ─┐
         └─< cohorts ─┐
students ─────────────┴─< certificates ─< events
```

| table | key columns | notes |
|---|---|---|
| `courses` | `slug` (unique), `title`, `description`, `skills` (JSON), `hours` | A program. Editing a course does not touch issued certificates. |
| `cohorts` | `course_id`, `name` — unique together | "Java 13", "Data 4.2". Created on import. |
| `students` | `email` (unique, case-insensitive), `full_name` | Created on import. Matched by email. |
| `certificates` | `id` (public, `ZCW-` + 10 Crockford base32 chars) | One per (student, cohort). See below. |
| `events` | `certificate_id`, `kind`, `referrer_host`, `at` | `view`, `og_image`, `linkedin_add`, `linkedin_share`, `learn_more`. |

`certificates` columns:

- **Snapshot:** `recipient_name`, `course_title`, `skills`, `issued_on`
- **Lifecycle:** `status` (`active` | `revoked`), `revoked_at`, `revoke_reason`
- **Consent:** `visibility` (`private` | `public`), `claim_token_hash`, `claimed_at`
- **Phase 2:** `signature` (Open Badges 3.0 proof)

The certificate ID has 50 random bits. It's unguessable, but short enough to
read over the phone. Input is normalized: case, dashes and spaces are ignored,
O becomes 0, and I and L become 1.

The claim token is 192 random bits, emailed to the student. Only its SHA-256
hash is stored, so a database leak doesn't expose management links.

Full DDL: [`internal/store/migrations/001_init.sql`](internal/store/migrations/001_init.sql).

## Routes

### Public (employers, LinkedIn, anyone)

| method | path | does |
|---|---|---|
| GET | `/` , `/verify?id=` | Verify form. A valid ID redirects to the certificate. |
| GET | `/c/{id}` | **The certificate page.** Open Graph tags, verified/revoked banner, share/copy/print, "About Zip Code" call to action. Private or unknown → 404 (same response). Non-canonical ID → 301. |
| GET | `/c/{id}/og.png` | 1200×627 share image rendered server-side. |
| GET | `/c/{id}/credential.json` | Machine-readable record (CORS open). Becomes a signed OB3 credential in phase 2. |
| GET | `/c/{id}/share/linkedin` | Counts the share, then redirects to `linkedin.com/sharing/share-offsite`. |
| GET | `/c/{id}/learn` | Counts the click, then redirects to the program site with `utm_source=certificate&utm_medium=referral&utm_campaign=<course>`. |
| GET | `/healthz` | Liveness. |

### Student (private claim link from email)

| method | path | does |
|---|---|---|
| GET | `/claim/{token}` | The student's page: publish/unpublish, copy link, LinkedIn buttons. `noindex`, `no-store`. |
| POST | `/claim/{token}` | `visibility=public|private`. |
| GET | `/claim/{token}/linkedin/add` | Counts the add, then redirects to LinkedIn's prefilled "Add license or certification" form. Only offered once the certificate is public. |

"Add to profile" is deliberately only on the student's page. The public page is
for verifiers, so it shows "Share" but not "Add to *your* profile".

### Admin (JSON, `Authorization: Bearer $CERTS_ADMIN_TOKEN`)

> Since Milestone 1, client data routes are nested under
> `/admin/api/clients/{client}/` (e.g. `/admin/api/clients/zcw/import`), and
> `/admin/api/clients` creates and lists clients. See the README.

| method | path | body / query | returns |
|---|---|---|---|
| POST | `/admin/api/courses` | `{slug,title,description,skills[],hours}` | course (upsert by slug) |
| POST | `/admin/api/import` | cohort CSV | `[{email, certificate_id, certificate_url, claim_url, existing}]` |
| GET | `/admin/api/certificates` | `?course=&cohort=` | certificates |
| POST | `/admin/api/certificates/{id}/revoke` | `{reason}` | — |
| POST | `/admin/api/certificates/{id}/claim-link` | — | new `claim_url` (old one stops working) |
| GET | `/admin/api/stats` | — | per-course issued/public/views/previews/adds/shares/learn-more |

The CLI (`certsforever course|import|revoke|claim-link`) does the same things
against the database directly, which is enough for issuing to a cohort.

## Flows

**Issue a cohort**

1. `certsforever course -slug java -title "Java Full-Stack Developer" -skills "Java,Spring Boot,SQL"` (once per course)
2. Export completions as `email,full_name,course,cohort,completed_on`.
3. `certsforever import -out links.csv java13.csv` issues the certificates (all private) and writes each student's claim link.
4. Mail-merge `links.csv`. Re-running the import is safe: existing certificates are reported, not duplicated.

**Student**

1. The student opens the claim link and clicks **Publish**.
2. **Add to LinkedIn profile** opens LinkedIn's form with the name, organization, issue month and year, credential ID and URL filled in.
3. **Share a post** opens LinkedIn's composer. LinkedIn fetches the Open Graph tags and `og.png` to build the preview card.

**Verifier**

1. A verifier clicks the link on LinkedIn, or enters the ID at `/verify`.
2. The page shows a green "Verified" banner or a red "Revoked" one.

## LinkedIn notes

- Set `CERTS_LINKEDIN_ORG_ID` to Zip Code's numeric LinkedIn company ID, so the
  issuer appears as the company (with logo) rather than as plain text.
- LinkedIn caches link previews. After changing the image or OG tags, refresh
  with LinkedIn's Post Inspector.
- No LinkedIn app, API key or OAuth is needed. Both buttons are plain URLs.

## Phase 2

- **Signed credentials:** Open Badges 3.0, which is a W3C Verifiable Credential. Sign with an
  Ed25519 key (`crypto/ed25519`), publish the issuer profile and public key
  at `/issuer`, store the proof in `certificates.signature`, and serve it from
  `/c/{id}/credential.json`. Certificates then stay verifiable even if this
  server goes away.
- **Email:** send claim links directly (SMTP or Postmark) instead of mail-merge.
- **Horizon LMS hook:** issue automatically on a course-completion event.
- **Static fallback:** `certsforever export-static` writes every public
  certificate as static HTML and PNG, so links survive a server outage or
  migration.
- **Admin UI:** a small HTML front end over the admin API.
- **PDF:** the print stylesheet covers this for now. A rendered PDF can come later.
