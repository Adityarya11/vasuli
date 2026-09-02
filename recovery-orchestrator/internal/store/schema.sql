-- A recovery campaign is a batch of accounts to contact.
CREATE TABLE IF NOT EXISTS campaigns (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    total       INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'completed', 'paused')),
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- One row per customer per recovery attempt.
--
-- Status lifecycle:
--   pending  -- queued, not yet contacted
--   active   -- call in progress (bound to a call_session_id)
--   link_sent -- outcome AGREED, payment link created, awaiting payment.captured
--   promised -- outcome PROMISED, customer committed to a future date
--   refused  -- outcome REFUSED, permanently excluded from further contact
--   unclear  -- outcome UNCLEAR, requires manual review (see docs/demo.md)
--   recovered -- payment.captured webhook confirmed
--   failed   -- stopping rule (max_contact_attempts) exhausted
CREATE TABLE IF NOT EXISTS recovery_sessions (
    id                      TEXT PRIMARY KEY,
    campaign_id             TEXT NOT NULL REFERENCES campaigns(id),
    call_session_id         TEXT,
    customer_name           TEXT NOT NULL,
    outstanding_paise       INTEGER NOT NULL,
    product_name            TEXT NOT NULL,
    due_date                TEXT,
    razorpay_payment_id     TEXT,
    system_prompt           TEXT NOT NULL,
    status                  TEXT NOT NULL DEFAULT 'pending'
                             CHECK (status IN (
                                'pending', 'active', 'link_sent', 'promised',
                                'refused', 'unclear', 'recovered', 'failed'
                             )),
    promise_date            TEXT,
    razorpay_link_id        TEXT,
    contact_attempts        INTEGER NOT NULL DEFAULT 0,
    max_contact_attempts    INTEGER NOT NULL DEFAULT 3,

    -- When this customer may next be contacted. NULL means never again:
    -- recovered, escalated to a human, or out of attempts.
    --
    -- NULL is the fail-safe direction on purpose. A code path that forgets
    -- to set this leaves the customer uncontacted, which is recoverable.
    -- The opposite default would leave them contacted forever.
    next_eligible_at        DATETIME,
    created_at              DATETIME DEFAULT CURRENT_TIMESTAMP,
    call_started_at         DATETIME,
    call_ended_at           DATETIME,
    recovered_at            DATETIME
);

-- Append-only event log. Every significant state change writes here.
-- This is the primary audit trail.
CREATE TABLE IF NOT EXISTS audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT NOT NULL,
    event_type  TEXT NOT NULL,
    event_data  TEXT,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_recovery_sessions_status ON recovery_sessions(status);
CREATE INDEX IF NOT EXISTS idx_recovery_sessions_campaign ON recovery_sessions(campaign_id);
CREATE INDEX IF NOT EXISTS idx_recovery_sessions_call_session ON recovery_sessions(call_session_id);
CREATE INDEX IF NOT EXISTS idx_audit_log_session ON audit_log(session_id);
