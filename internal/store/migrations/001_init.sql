-- 001_init.sql — initial CertsForever schema.
-- Timestamps are ISO-8601 UTC text; dates are YYYY-MM-DD text.

CREATE TABLE courses (
    id          INTEGER PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,           -- "java-fullstack", used in URLs/UTM
    title       TEXT NOT NULL,                  -- printed on the certificate
    description TEXT NOT NULL DEFAULT '',
    skills      TEXT NOT NULL DEFAULT '[]',     -- JSON array of strings
    hours       INTEGER,                        -- instructional hours, optional
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE cohorts (
    id         INTEGER PRIMARY KEY,
    course_id  INTEGER NOT NULL REFERENCES courses(id),
    name       TEXT NOT NULL,                   -- "Data 4.2", "Java 13"
    starts_on  TEXT,
    ends_on    TEXT,
    UNIQUE (course_id, name)
);

CREATE TABLE students (
    id         INTEGER PRIMARY KEY,
    email      TEXT NOT NULL UNIQUE COLLATE NOCASE,
    full_name  TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- A certificate is immutable once issued: recipient name, course title and
-- skills are snapshotted so later edits to courses/students never change
-- what a credential says. Only status/visibility/claim fields change.
CREATE TABLE certificates (
    id               TEXT PRIMARY KEY,          -- public ID, e.g. ZCW-7K3M9QF2XA
    student_id       INTEGER NOT NULL REFERENCES students(id),
    cohort_id        INTEGER NOT NULL REFERENCES cohorts(id),

    recipient_name   TEXT NOT NULL,
    course_title     TEXT NOT NULL,
    skills           TEXT NOT NULL DEFAULT '[]',
    issued_on        TEXT NOT NULL,

    status           TEXT NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active', 'revoked')),
    revoked_at       TEXT,
    revoke_reason    TEXT,

    -- Consent: certificates start private; the student makes them public
    -- from their claim link.
    visibility       TEXT NOT NULL DEFAULT 'private'
                     CHECK (visibility IN ('private', 'public')),
    claim_token_hash BLOB UNIQUE,               -- sha256 of the emailed claim token
    claimed_at       TEXT,

    signature        TEXT,                      -- phase 2: Open Badges 3.0 proof (JWS)
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),

    UNIQUE (student_id, cohort_id)
);

CREATE INDEX certificates_cohort ON certificates(cohort_id);

-- Lightweight, cookie-free analytics: what certificates bring in.
-- No IPs or user agents are stored.
CREATE TABLE events (
    id             INTEGER PRIMARY KEY,
    certificate_id TEXT NOT NULL REFERENCES certificates(id),
    kind           TEXT NOT NULL CHECK (kind IN
                   ('view', 'og_image', 'linkedin_add', 'linkedin_share', 'learn_more')),
    referrer_host  TEXT,
    at             TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX events_cert_kind ON events(certificate_id, kind);
CREATE INDEX events_at ON events(at);
