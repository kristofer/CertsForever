-- 005_console.sql — what the client console needs: certificate designs,
-- uploaded images, design snapshots on certificates, name corrections and
-- per-client API tokens.

-- Images uploaded by a client (logos, signatures). Immutable: an edit
-- uploads a new asset, so certificates that reference an old one keep it.
CREATE TABLE assets (
    id           TEXT PRIMARY KEY,                  -- random, used in /assets/{id}
    client_id    INTEGER NOT NULL REFERENCES clients(id),
    kind         TEXT NOT NULL CHECK (kind IN ('logo', 'signature')),
    content_type TEXT NOT NULL CHECK (content_type = 'image/png'),
    bytes        BLOB NOT NULL,
    sha256       TEXT NOT NULL,
    width        INTEGER NOT NULL,
    height       INTEGER NOT NULL,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (client_id, id)
);

-- How a course's certificates look and read.
CREATE TABLE certificate_designs (
    id            INTEGER PRIMARY KEY,
    client_id     INTEGER NOT NULL REFERENCES clients(id),
    name          TEXT NOT NULL CHECK (length(trim(name)) > 0),
    heading       TEXT NOT NULL DEFAULT 'Certificate of Completion',
    body_text     TEXT NOT NULL DEFAULT 'has successfully completed',
    accent_color  TEXT NOT NULL DEFAULT '#1f8f81'
                  CHECK (length(accent_color) = 7 AND accent_color GLOB '#[0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F]'),
    logo_asset_id TEXT,
    signatories   TEXT NOT NULL DEFAULT '[]',       -- JSON [{name, title, signature_asset_id}]
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (client_id, name),
    UNIQUE (client_id, id),
    FOREIGN KEY (client_id, logo_asset_id) REFERENCES assets (client_id, id)
);

-- Courses pick a design. ALTER TABLE can only add a single-column foreign
-- key, so a trigger keeps the design within the course's client.
ALTER TABLE courses ADD COLUMN design_id INTEGER REFERENCES certificate_designs(id);

CREATE TRIGGER courses_design_same_client_insert
BEFORE INSERT ON courses
WHEN NEW.design_id IS NOT NULL
 AND NOT EXISTS (SELECT 1 FROM certificate_designs d WHERE d.id = NEW.design_id AND d.client_id = NEW.client_id)
BEGIN
    SELECT RAISE(ABORT, 'design belongs to another client');
END;

CREATE TRIGGER courses_design_same_client_update
BEFORE UPDATE OF design_id, client_id ON courses
WHEN NEW.design_id IS NOT NULL
 AND NOT EXISTS (SELECT 1 FROM certificate_designs d WHERE d.id = NEW.design_id AND d.client_id = NEW.client_id)
BEGIN
    SELECT RAISE(ABORT, 'design belongs to another client');
END;

-- The design as it was when the certificate was issued (JSON). Editing a
-- design never changes certificates already on someone's LinkedIn.
-- NULL (certificates from before this migration) renders the default look.
ALTER TABLE certificates ADD COLUMN design_snapshot TEXT;

-- Recipient-name corrections are made in place (the URL never changes)
-- and recorded here and in the audit log.
ALTER TABLE certificates ADD COLUMN name_corrected_at TEXT;

-- Per-client API tokens for automation (e.g. issuing from an LMS).
CREATE TABLE api_tokens (
    id             INTEGER PRIMARY KEY,
    client_id      INTEGER NOT NULL REFERENCES clients(id),
    name           TEXT NOT NULL CHECK (length(trim(name)) > 0),
    token_hash     BLOB NOT NULL UNIQUE,
    display_prefix TEXT NOT NULL,                   -- first characters, to recognize it
    created_by     INTEGER REFERENCES users(id),
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    last_used_at   TEXT,
    revoked_at     TEXT
);
CREATE INDEX api_tokens_client ON api_tokens(client_id);
