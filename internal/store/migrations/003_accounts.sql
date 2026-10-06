-- 003_accounts.sql — people who administer CertsForever, and what they did.
--
-- Two roles:
--   * super admin  (users.is_super_admin = 1): runs the platform, every client
--   * client admin (a row in memberships):    runs one client
--
-- Sign-in is by single-use emailed link (login_tokens); there are no
-- passwords. Every secret (login token, session id) is stored only as a
-- SHA-256 hash. TOTP secrets are stored encrypted (AES-GCM, master key).

CREATE TABLE users (
    id                INTEGER PRIMARY KEY,
    email             TEXT NOT NULL UNIQUE COLLATE NOCASE CHECK (email LIKE '%_@_%'),
    name              TEXT NOT NULL DEFAULT '',
    is_super_admin    INTEGER NOT NULL DEFAULT 0 CHECK (is_super_admin IN (0, 1)),
    totp_secret_enc   BLOB,
    totp_enabled_at   TEXT,
    totp_last_counter INTEGER NOT NULL DEFAULT 0,   -- replay protection
    disabled_at       TEXT,
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    last_login_at     TEXT
);

CREATE TABLE memberships (
    user_id    INTEGER NOT NULL REFERENCES users(id),
    client_id  INTEGER NOT NULL REFERENCES clients(id),
    role       TEXT NOT NULL DEFAULT 'admin' CHECK (role IN ('admin')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    created_by INTEGER REFERENCES users(id),
    PRIMARY KEY (user_id, client_id)
);
CREATE INDEX memberships_client ON memberships(client_id);

-- Single-use sign-in links. "invite" links last longer than "login" links.
CREATE TABLE login_tokens (
    token_hash BLOB PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id),
    purpose    TEXT NOT NULL CHECK (purpose IN ('login', 'invite')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    expires_at TEXT NOT NULL,
    used_at    TEXT
);
CREATE INDEX login_tokens_user ON login_tokens(user_id);

CREATE TABLE sessions (
    id_hash       BLOB PRIMARY KEY,
    user_id       INTEGER NOT NULL REFERENCES users(id),
    csrf_token    TEXT NOT NULL,
    totp_verified INTEGER NOT NULL DEFAULT 0 CHECK (totp_verified IN (0, 1)),
    created_at    TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    expires_at    TEXT NOT NULL,                     -- absolute limit
    ip            TEXT NOT NULL DEFAULT '',
    user_agent    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions(user_id);

-- Who did what. Append-only: the triggers below refuse UPDATE and DELETE.
CREATE TABLE audit_log (
    id            INTEGER PRIMARY KEY,
    at            TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    actor_user_id INTEGER REFERENCES users(id),
    actor_label   TEXT NOT NULL,                     -- email, "cli", "admin-token"
    client_id     INTEGER REFERENCES clients(id),
    impersonated  INTEGER NOT NULL DEFAULT 0 CHECK (impersonated IN (0, 1)),
    action        TEXT NOT NULL,                     -- e.g. certificate.revoke
    target_type   TEXT NOT NULL DEFAULT '',
    target_id     TEXT NOT NULL DEFAULT '',
    details       TEXT NOT NULL DEFAULT '{}',        -- JSON
    ip            TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_client_at ON audit_log(client_id, at);
CREATE INDEX audit_at ON audit_log(at);

CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;

CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;
