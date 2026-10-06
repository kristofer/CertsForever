-- 006_platform.sql — what the super console needs: custom domains per
-- client, the client a super admin is acting as, and why a client was
-- suspended.

-- Custom domains (certs.zipcodewilmington.com). Milestone 6 routes and
-- serves them; here they're recorded and verified. A domain that has ever
-- been verified may have certificate links pointing at it, so it's never
-- deleted, only retired (is_canonical = 0); old links keep working.
CREATE TABLE client_domains (
    id           INTEGER PRIMARY KEY,
    client_id    INTEGER NOT NULL REFERENCES clients(id),
    host         TEXT NOT NULL UNIQUE COLLATE NOCASE
                 CHECK (length(host) BETWEEN 4 AND 253 AND host COLLATE BINARY = lower(host) AND host NOT GLOB '*[^a-z0-9.-]*'),
    is_canonical INTEGER NOT NULL DEFAULT 0 CHECK (is_canonical IN (0, 1)),
    verified_at  TEXT,
    verified_by  TEXT NOT NULL DEFAULT '',          -- "dns" or "manual"
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE INDEX client_domains_client ON client_domains(client_id);
-- At most one canonical domain per client, and only a verified one.
CREATE UNIQUE INDEX client_domains_one_canonical ON client_domains(client_id) WHERE is_canonical = 1;

CREATE TRIGGER client_domains_canonical_verified_insert
BEFORE INSERT ON client_domains
WHEN NEW.is_canonical = 1 AND NEW.verified_at IS NULL
BEGIN
    SELECT RAISE(ABORT, 'a canonical domain must be verified');
END;

CREATE TRIGGER client_domains_canonical_verified_update
BEFORE UPDATE OF is_canonical, verified_at ON client_domains
WHEN NEW.is_canonical = 1 AND NEW.verified_at IS NULL
BEGIN
    SELECT RAISE(ABORT, 'a canonical domain must be verified');
END;

-- A domain never moves to another client.
CREATE TRIGGER client_domains_client_immutable
BEFORE UPDATE OF client_id, host ON client_domains
BEGIN
    SELECT RAISE(ABORT, 'a domain cannot move to another client or be renamed');
END;

-- A verified domain is never deleted (links may point at it).
CREATE TRIGGER client_domains_keep_verified
BEFORE DELETE ON client_domains
WHEN OLD.verified_at IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'verified domains are retired, not deleted');
END;

-- The client a super admin's session is acting as (NULL: not acting).
-- Entering a client's console as a non-member requires choosing to act as
-- it, which is audited; the banner shows while it's set.
ALTER TABLE sessions ADD COLUMN acting_client_id INTEGER REFERENCES clients(id);

-- Why a client was suspended (shown to platform admins).
ALTER TABLE clients ADD COLUMN suspend_reason TEXT NOT NULL DEFAULT '';
