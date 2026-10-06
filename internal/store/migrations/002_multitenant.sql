-- 002_multitenant.sql — clients (tenants) and tenant-safe foreign keys.
--
-- Every client-owned table gets client_id, and children reference parents
-- through composite keys (client_id, id). The database itself then rejects
-- any row that points across clients, e.g. a certificate for client A that
-- names client B's course, even if application code has a bug.
--
-- Existing single-organization data (from 001) is moved into a client with
-- slug 'zcw'. On a fresh database no client is created.
--
-- SQLite can't add NOT NULL foreign-key columns in place, so each table is
-- rebuilt: create *_new, copy, drop the old table, rename. Renames update the
-- foreign-key references in the other *_new tables automatically.

CREATE TABLE clients (
    id              INTEGER PRIMARY KEY,
    slug            TEXT NOT NULL UNIQUE
                    CHECK (length(slug) BETWEEN 2 AND 40
                           AND slug GLOB '[a-z0-9]*'
                           AND slug NOT GLOB '*[^a-z0-9-]*'),
    name            TEXT NOT NULL CHECK (length(trim(name)) > 0),
    id_prefix       TEXT NOT NULL UNIQUE
                    CHECK (length(id_prefix) BETWEEN 2 AND 4 AND id_prefix NOT GLOB '*[^A-Z]*'),
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    site_url        TEXT NOT NULL DEFAULT '',
    blurb           TEXT NOT NULL DEFAULT '',
    linkedin_org_id TEXT NOT NULL DEFAULT '' CHECK (linkedin_org_id NOT GLOB '*[^0-9]*'),
    brand           TEXT NOT NULL DEFAULT '{}',           -- JSON: colors, fonts (Milestone 6)
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    suspended_at    TEXT
);

INSERT INTO clients (slug, name, id_prefix, site_url, blurb)
SELECT 'zcw', 'Zip Code Wilmington', 'ZCW', 'https://zipcodewilmington.com',
       'Zip Code Wilmington is a nonprofit, intensive coding bootcamp in Wilmington, Delaware, '
       || 'preparing people from all backgrounds for careers in software.'
WHERE EXISTS (SELECT 1 FROM courses) OR EXISTS (SELECT 1 FROM students);

-- ---- courses --------------------------------------------------------------
CREATE TABLE courses_new (
    id          INTEGER PRIMARY KEY,
    client_id   INTEGER NOT NULL REFERENCES clients(id),
    slug        TEXT NOT NULL CHECK (length(slug) > 0),
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    skills      TEXT NOT NULL DEFAULT '[]',
    hours       INTEGER,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (client_id, slug),
    UNIQUE (client_id, id)
);
INSERT INTO courses_new (id, client_id, slug, title, description, skills, hours, created_at)
SELECT id, (SELECT id FROM clients WHERE slug = 'zcw'), slug, title, description, skills, hours, created_at
FROM courses;

-- ---- cohorts --------------------------------------------------------------
CREATE TABLE cohorts_new (
    id         INTEGER PRIMARY KEY,
    client_id  INTEGER NOT NULL,
    course_id  INTEGER NOT NULL,
    name       TEXT NOT NULL,
    starts_on  TEXT,
    ends_on    TEXT,
    UNIQUE (course_id, name),
    UNIQUE (client_id, id),
    UNIQUE (id, course_id),
    FOREIGN KEY (client_id, course_id) REFERENCES courses_new (client_id, id)
);
INSERT INTO cohorts_new (id, client_id, course_id, name, starts_on, ends_on)
SELECT id, (SELECT id FROM clients WHERE slug = 'zcw'), course_id, name, starts_on, ends_on
FROM cohorts;

-- ---- students -------------------------------------------------------------
CREATE TABLE students_new (
    id         INTEGER PRIMARY KEY,
    client_id  INTEGER NOT NULL REFERENCES clients(id),
    email      TEXT NOT NULL COLLATE NOCASE,
    full_name  TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (client_id, email),
    UNIQUE (client_id, id)
);
INSERT INTO students_new (id, client_id, email, full_name, created_at)
SELECT id, (SELECT id FROM clients WHERE slug = 'zcw'), email, full_name, created_at
FROM students;

-- ---- certificates ---------------------------------------------------------
CREATE TABLE certificates_new (
    id               TEXT PRIMARY KEY,
    client_id        INTEGER NOT NULL,
    student_id       INTEGER NOT NULL,
    course_id        INTEGER NOT NULL,
    cohort_id        INTEGER NOT NULL,

    recipient_name   TEXT NOT NULL,
    course_title     TEXT NOT NULL,
    skills           TEXT NOT NULL DEFAULT '[]',
    issued_on        TEXT NOT NULL,

    status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    revoked_at       TEXT,
    revoke_reason    TEXT,

    visibility       TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'public')),
    claim_token_hash BLOB UNIQUE,
    claimed_at       TEXT,

    signature        TEXT,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),

    UNIQUE (student_id, cohort_id),
    UNIQUE (client_id, id),
    FOREIGN KEY (client_id, student_id) REFERENCES students_new (client_id, id),
    FOREIGN KEY (client_id, course_id)  REFERENCES courses_new (client_id, id),
    FOREIGN KEY (client_id, cohort_id)  REFERENCES cohorts_new (client_id, id),
    FOREIGN KEY (cohort_id, course_id)  REFERENCES cohorts_new (id, course_id)
);
INSERT INTO certificates_new
    (id, client_id, student_id, course_id, cohort_id, recipient_name, course_title, skills, issued_on,
     status, revoked_at, revoke_reason, visibility, claim_token_hash, claimed_at, signature, created_at)
SELECT c.id, (SELECT id FROM clients WHERE slug = 'zcw'), c.student_id, h.course_id, c.cohort_id,
       c.recipient_name, c.course_title, c.skills, c.issued_on, c.status, c.revoked_at, c.revoke_reason,
       c.visibility, c.claim_token_hash, c.claimed_at, c.signature, c.created_at
FROM certificates c JOIN cohorts h ON h.id = c.cohort_id;

-- ---- events ---------------------------------------------------------------
CREATE TABLE events_new (
    id             INTEGER PRIMARY KEY,
    client_id      INTEGER NOT NULL,
    certificate_id TEXT NOT NULL,
    kind           TEXT NOT NULL CHECK (kind IN
                   ('view', 'og_image', 'linkedin_add', 'linkedin_share', 'learn_more')),
    referrer_host  TEXT,
    at             TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    FOREIGN KEY (client_id, certificate_id) REFERENCES certificates_new (client_id, id)
);
INSERT INTO events_new (id, client_id, certificate_id, kind, referrer_host, at)
SELECT id, (SELECT id FROM clients WHERE slug = 'zcw'), certificate_id, kind, referrer_host, at
FROM events;

-- ---- swap: drop old (children first), rename new --------------------------
DROP TABLE events;
DROP TABLE certificates;
DROP TABLE students;
DROP TABLE cohorts;
DROP TABLE courses;

ALTER TABLE courses_new      RENAME TO courses;
ALTER TABLE cohorts_new      RENAME TO cohorts;
ALTER TABLE students_new     RENAME TO students;
ALTER TABLE certificates_new RENAME TO certificates;
ALTER TABLE events_new       RENAME TO events;

CREATE INDEX certificates_client_issued ON certificates(client_id, issued_on);
CREATE INDEX certificates_cohort        ON certificates(cohort_id);
CREATE INDEX events_cert_kind           ON events(certificate_id, kind);
CREATE INDEX events_client_at           ON events(client_id, at);

-- ---- invariants the schema can't express as constraints ------------------

-- A certificate ID must carry its client's prefix (ZCW-…, TWA-…).
CREATE TRIGGER certificates_id_prefix
BEFORE INSERT ON certificates
WHEN NEW.id NOT LIKE (SELECT id_prefix FROM clients WHERE id = NEW.client_id) || '-%'
BEGIN
    SELECT RAISE(ABORT, 'certificate id must start with its client''s id_prefix');
END;

CREATE TRIGGER certificates_identity_immutable
BEFORE UPDATE OF id, client_id, student_id, course_id, cohort_id ON certificates
BEGIN
    SELECT RAISE(ABORT, 'certificate identity columns are immutable');
END;

-- Prefixes are part of public URLs: frozen once a certificate uses them.
CREATE TRIGGER clients_prefix_frozen
BEFORE UPDATE OF id_prefix ON clients
WHEN OLD.id_prefix <> NEW.id_prefix
     AND EXISTS (SELECT 1 FROM certificates WHERE client_id = OLD.id)
BEGIN
    SELECT RAISE(ABORT, 'id_prefix cannot change after certificates are issued');
END;
