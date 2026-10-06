-- 004_email.sql — durable outgoing email.
--
-- Every email goes through email_outbox and is delivered by a background
-- worker with retries. Bodies contain sign-in and claim links (as good as
-- passwords), so they're stored encrypted (body_enc, sealed with the master
-- key) and wiped once delivered, or a week after delivery finally fails.

CREATE TABLE email_outbox (
    id              INTEGER PRIMARY KEY,
    client_id       INTEGER REFERENCES clients(id),     -- NULL for platform mail (sign-in)
    to_addr         TEXT NOT NULL,
    template        TEXT NOT NULL,                      -- sign_in, invite, certificate_ready, reminder, test
    subject         TEXT NOT NULL,
    body_enc        BLOB,                               -- sealed message; NULL once wiped
    status          TEXT NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued', 'sending', 'sent', 'failed', 'suppressed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    locked_until    TEXT,                               -- lease while a worker is sending
    last_error      TEXT NOT NULL DEFAULT '',
    ref_type        TEXT NOT NULL DEFAULT '',           -- e.g. certificate
    ref_id          TEXT NOT NULL DEFAULT '',           -- e.g. ZCW-7K3M9QF2XA
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    sent_at         TEXT,
    finished_at     TEXT                                -- sent, failed or suppressed
);
CREATE INDEX email_outbox_due ON email_outbox(status, next_attempt_at);
CREATE INDEX email_outbox_ref ON email_outbox(ref_type, ref_id);

-- Addresses we must not send to: hard bounces (SMTP 5xx at RCPT, or the
-- provider's webhook), spam complaints, or manual. Platform-wide.
CREATE TABLE email_suppressions (
    email      TEXT PRIMARY KEY COLLATE NOCASE,
    reason     TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL CHECK (source IN ('smtp', 'webhook', 'manual')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Per-client email settings.
ALTER TABLE clients ADD COLUMN reply_to TEXT NOT NULL DEFAULT '';
ALTER TABLE clients ADD COLUMN send_reminders INTEGER NOT NULL DEFAULT 1;

-- One "you haven't published your certificate yet" nudge per certificate.
ALTER TABLE certificates ADD COLUMN reminder_sent_at TEXT;
