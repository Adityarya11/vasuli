// Package store is the only place in recovery-orchestrator that talks SQL.
// Everything here is data access, business rules (stopping rules, outcome
// handling) live in the campaign package and call these methods.
package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type DB struct {
	conn *sql.DB
}

// Open initializes the SQLite database at path and applies the schema.
//
// MaxOpenConns is pinned to 1. SQLite allows only one writer at a time;
// database/sql's connection pool is designed for server databases where
// concurrent connections are cheap and expected. Left at its default, two
// goroutines can each open a connection, both read the same "pending" row
// before either writes it back as "active", and the same customer gets
// assigned to two concurrent calls. Capping the pool serializes all access
// through one connection, so database/sql's own queuing prevents the race
// instead of needing manual locking in application code.
func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: failed to open database: %w", err)
	}
	conn.SetMaxOpenConns(1)

	if _, err := conn.Exec(schemaSQL); err != nil {
		conn.Close()
		return nil, fmt.Errorf("store: failed to apply schema: %w", err)
	}

	if err := migrate(conn); err != nil {
		conn.Close()
		return nil, err
	}

	applyUniqueIndexes(conn)

	return &DB{conn: conn}, nil
}

// migrate brings a database created by an earlier schema up to date.
// CREATE TABLE IF NOT EXISTS silently does nothing when the table already
// exists, so new columns have to be added separately or an existing
// vasuli.db would fail every query that mentions them.
func migrate(conn *sql.DB) error {
	has, err := hasColumn(conn, "recovery_sessions", "next_eligible_at")
	if err != nil {
		return err
	}
	if has {
		return nil
	}

	if _, err := conn.Exec(`ALTER TABLE recovery_sessions ADD COLUMN next_eligible_at DATETIME`); err != nil {
		return fmt.Errorf("store: add next_eligible_at: %w", err)
	}

	// Backfill: anything still open becomes eligible immediately, anything
	// already concluded stays NULL and is never contacted again.
	if _, err := conn.Exec(
		`UPDATE recovery_sessions
		    SET next_eligible_at = COALESCE(call_ended_at, created_at, CURRENT_TIMESTAMP)
		  WHERE status IN ('pending', 'active', 'unclear', 'link_sent', 'promised')`,
	); err != nil {
		return fmt.Errorf("store: backfill next_eligible_at: %w", err)
	}

	return nil
}

// uniqueIndexes enforce that a call session, a failed payment, and a
// generated link each resolve to exactly one recovery session. They are
// partial because all three columns stay NULL until the relevant event
// happens, and SQLite treats NULLs as distinct, so a plain UNIQUE would
// block nothing useful while still permitting duplicate NULLs.
var uniqueIndexes = []string{
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_recovery_sessions_call_session_unique
	   ON recovery_sessions(call_session_id) WHERE call_session_id IS NOT NULL`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_recovery_sessions_payment_unique
	   ON recovery_sessions(razorpay_payment_id) WHERE razorpay_payment_id IS NOT NULL`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_recovery_sessions_link_unique
	   ON recovery_sessions(razorpay_link_id) WHERE razorpay_link_id IS NOT NULL`,
}

// applyUniqueIndexes adds the uniqueness guarantees, tolerating databases
// that already contain duplicates.
//
// Failing here is not fatal on purpose. A database written before these
// constraints existed may hold rows that violate them, and refusing to
// start is a worse outcome than running without the index: every one of
// these is a safety net over application logic that already checks the
// same thing. The warning says which one did not apply so it can be
// cleaned up deliberately.
func applyUniqueIndexes(conn *sql.DB) {
	for _, stmt := range uniqueIndexes {
		if _, err := conn.Exec(stmt); err != nil {
			log.Printf("[Recovery] uniqueness constraint not applied, existing rows may conflict: %v", err)
		}
	}
}

func hasColumn(conn *sql.DB, table, column string) (bool, error) {
	rows, err := conn.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("store: inspect %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, fmt.Errorf("store: scan column name: %w", err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (db *DB) Close() error {
	return db.conn.Close()
}

// Recovery session statuses. These mirror the CHECK constraint in
// schema.sql; changing one without the other will surface as a write
// rejected by the database.
const (
	StatusPending   = "pending"   // queued, not yet contacted
	StatusActive    = "active"    // a call is in progress
	StatusLinkSent  = "link_sent" // agreed, link created, payment unconfirmed
	StatusPromised  = "promised"  // committed to a future date
	StatusRefused   = "refused"   // declined; excluded from further contact
	StatusUnclear   = "unclear"   // no clear resolution, awaiting manual review
	StatusRecovered = "recovered" // payment confirmed
	StatusFailed    = "failed"    // contact attempts exhausted
)

type Campaign struct {
	ID        string
	Name      string
	Total     int
	Status    string
	CreatedAt time.Time
}

// Account is the caller-supplied input for one customer in a campaign.
// It becomes a RecoverySession once the system prompt is rendered.
type Account struct {
	CustomerName      string
	OutstandingPaise  int64
	ProductName       string
	DueDate           string
	RazorpayPaymentID string
}

type RecoverySession struct {
	ID                 string
	CampaignID         string
	CallSessionID      sql.NullString
	CustomerName       string
	OutstandingPaise   int64
	ProductName        string
	DueDate            string
	RazorpayPaymentID  sql.NullString
	SystemPrompt       string
	Status             string
	PromiseDate        sql.NullString
	RazorpayLinkID     sql.NullString
	ContactAttempts    int
	MaxContactAttempts int
	CreatedAt          time.Time
	CallStartedAt      sql.NullTime
	CallEndedAt        sql.NullTime
	RecoveredAt        sql.NullTime

	// NextEligibleAt is invalid (NULL) when this customer must never be
	// contacted again. Recovered, escalated to a human, or out of attempts.
	NextEligibleAt sql.NullTime
}

func (db *DB) InsertCampaign(id, name string, total int) error {
	_, err := db.conn.Exec(
		`INSERT INTO campaigns (id, name, total) VALUES (?, ?, ?)`,
		id, name, total,
	)
	if err != nil {
		return fmt.Errorf("store: insert campaign: %w", err)
	}
	return nil
}

func (db *DB) InsertRecoverySession(id, campaignID, systemPrompt string, acc Account) error {
	// next_eligible_at is set on insert rather than defaulted, so a freshly
	// loaded account is contactable immediately while the column keeps its
	// fail-safe NULL-means-never semantics everywhere else.
	_, err := db.conn.Exec(
		`INSERT INTO recovery_sessions
			(id, campaign_id, customer_name, outstanding_paise, product_name,
			 due_date, razorpay_payment_id, system_prompt, next_eligible_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		id, campaignID, acc.CustomerName, acc.OutstandingPaise, acc.ProductName,
		acc.DueDate, nullIfEmpty(acc.RazorpayPaymentID), systemPrompt,
	)
	if err != nil {
		return fmt.Errorf("store: insert recovery session: %w", err)
	}
	return nil
}

func (db *DB) GetCampaign(id string) (*Campaign, error) {
	var c Campaign
	err := db.conn.QueryRow(
		`SELECT id, name, total, status, created_at FROM campaigns WHERE id = ?`, id,
	).Scan(&c.ID, &c.Name, &c.Total, &c.Status, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get campaign: %w", err)
	}
	return &c, nil
}

// eligibleNow is the single definition of "may be contacted". Both the
// assignment query and the queue view read from it, so the scheduling rule
// cannot drift between what the system does and what it reports.
//
// The status list is redundant with next_eligible_at when every write path
// is correct, and is kept deliberately: this column gets edited by hand
// during demos, and a stray UPDATE that caught an escalated row would
// re-contact a customer the system promised never to call again.
const eligibleNow = `
	rs.next_eligible_at IS NOT NULL
	AND rs.next_eligible_at <= CURRENT_TIMESTAMP
	AND rs.contact_attempts < rs.max_contact_attempts
	AND rs.status IN ('pending', 'unclear', 'link_sent', 'promised')`

// AssignNextPending atomically claims the longest-waiting eligible session
// belonging to an active campaign and binds it to callSessionID. Returns
// nil, nil when nothing is due, callers must distinguish that from an
// error and fall back accordingly.
func (db *DB) AssignNextPending(callSessionID string) (*RecoverySession, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin assign tx: %w", err)
	}
	defer tx.Rollback()

	var id string
	err = tx.QueryRow(
		`SELECT rs.id
		   FROM recovery_sessions rs
		   JOIN campaigns c ON c.id = rs.campaign_id
		  WHERE c.status = 'active' AND (` + eligibleNow + `)
		  ORDER BY rs.next_eligible_at ASC, rs.created_at ASC
		  LIMIT 1`,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: select pending session: %w", err)
	}

	_, err = tx.Exec(
		`UPDATE recovery_sessions
		    SET status = 'active',
		        call_session_id = ?,
		        contact_attempts = contact_attempts + 1,
		        call_started_at = CURRENT_TIMESTAMP
		  WHERE id = ?`,
		callSessionID, id,
	)
	if err != nil {
		return nil, fmt.Errorf("store: bind session to call: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit assign tx: %w", err)
	}

	return db.GetSessionByID(id)
}

func (db *DB) GetSessionByID(id string) (*RecoverySession, error) {
	return db.scanSession(db.conn.QueryRow(sessionSelectCols+` WHERE id = ?`, id))
}

func (db *DB) GetSessionByCallSessionID(callSessionID string) (*RecoverySession, error) {
	return db.scanSession(db.conn.QueryRow(sessionSelectCols+` WHERE call_session_id = ?`, callSessionID))
}

func (db *DB) GetSessionByRazorpayPaymentID(paymentID string) (*RecoverySession, error) {
	return db.scanSession(db.conn.QueryRow(sessionSelectCols+` WHERE razorpay_payment_id = ?`, paymentID))
}

// GetSessionByRazorpayLinkID resolves the session that a payment_link.paid
// webhook refers to. This is a distinct lookup from the payment-id one on
// purpose: a payment made against a link we generated carries a brand new
// payment id, unrelated to the failed payment that originally triggered
// recovery, so razorpay_payment_id can never match it.
func (db *DB) GetSessionByRazorpayLinkID(linkID string) (*RecoverySession, error) {
	return db.scanSession(db.conn.QueryRow(sessionSelectCols+` WHERE razorpay_link_id = ?`, linkID))
}

// GetMostRecentActiveCampaign returns the newest campaign still accepting
// work, or nil when none is active. Inbound payment.failed webhooks attach
// to it; with no active campaign they are acknowledged and dropped rather
// than inventing a campaign to own them.
func (db *DB) GetMostRecentActiveCampaign() (*Campaign, error) {
	var c Campaign
	err := db.conn.QueryRow(
		`SELECT id, name, total, status, created_at
		   FROM campaigns
		  WHERE status = 'active'
		  ORDER BY created_at DESC
		  LIMIT 1`,
	).Scan(&c.ID, &c.Name, &c.Total, &c.Status, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get active campaign: %w", err)
	}
	return &c, nil
}

// IncrementCampaignTotal keeps campaigns.total consistent when a session is
// added after campaign creation, so metrics denominators stay correct.
func (db *DB) IncrementCampaignTotal(campaignID string) error {
	_, err := db.conn.Exec(
		`UPDATE campaigns SET total = total + 1 WHERE id = ?`, campaignID,
	)
	if err != nil {
		return fmt.Errorf("store: increment campaign total: %w", err)
	}
	return nil
}

const sessionSelectCols = `
	SELECT id, campaign_id, call_session_id, customer_name, outstanding_paise,
	       product_name, due_date, razorpay_payment_id, system_prompt, status,
	       promise_date, razorpay_link_id, contact_attempts, max_contact_attempts,
	       created_at, call_started_at, call_ended_at, recovered_at, next_eligible_at
	  FROM recovery_sessions`

func (db *DB) scanSession(row *sql.Row) (*RecoverySession, error) {
	var s RecoverySession
	err := row.Scan(
		&s.ID, &s.CampaignID, &s.CallSessionID, &s.CustomerName, &s.OutstandingPaise,
		&s.ProductName, &s.DueDate, &s.RazorpayPaymentID, &s.SystemPrompt, &s.Status,
		&s.PromiseDate, &s.RazorpayLinkID, &s.ContactAttempts, &s.MaxContactAttempts,
		&s.CreatedAt, &s.CallStartedAt, &s.CallEndedAt, &s.RecoveredAt, &s.NextEligibleAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan recovery session: %w", err)
	}
	return &s, nil
}

// AuditEvent is one row destined for the audit log.
type AuditEvent struct {
	Type string
	Data any
}

// OutcomeWrite is the complete set of changes a finished call produces.
// It is applied as a unit so the audit trail can never disagree with the
// state it audits.
type OutcomeWrite struct {
	SessionID      string
	Status         string
	PromiseDate    string
	RazorpayLinkID string
	Events         []AuditEvent

	// NextEligibleAt is nil when this outcome ends contact permanently.
	// It is always written, unlike the optional fields above, because
	// "when may we call again" has a definite answer for every outcome.
	NextEligibleAt *time.Time
}

// RecordOutcome applies an OutcomeWrite in a single transaction, but only
// if the session is still in one of allowedFrom. It reports false when the
// session has moved on, leaving the database untouched.
//
// The status is re-checked inside the transaction rather than trusted from
// an earlier read, so two requests racing to end the same call cannot both
// commit: the loser sees the winner's status and is rejected. Everything
// here is local database work; the payment-link API call happens before
// this is invoked, precisely so no network round trip is held inside a
// transaction that blocks every other writer.
func (db *DB) RecordOutcome(w OutcomeWrite, allowedFrom []string) (bool, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return false, fmt.Errorf("store: begin outcome tx: %w", err)
	}
	defer tx.Rollback()

	var current string
	err = tx.QueryRow(`SELECT status FROM recovery_sessions WHERE id = ?`, w.SessionID).Scan(&current)
	if err == sql.ErrNoRows {
		return false, fmt.Errorf("store: no session %q", w.SessionID)
	}
	if err != nil {
		return false, fmt.Errorf("store: read status for outcome: %w", err)
	}

	if !slices.Contains(allowedFrom, current) {
		return false, nil
	}

	_, err = tx.Exec(
		`UPDATE recovery_sessions
		    SET status = ?,
		        call_ended_at = CURRENT_TIMESTAMP,
		        promise_date = COALESCE(NULLIF(?, ''), promise_date),
		        razorpay_link_id = COALESCE(NULLIF(?, ''), razorpay_link_id),
		        next_eligible_at = ?
		  WHERE id = ?`,
		w.Status, w.PromiseDate, w.RazorpayLinkID, w.NextEligibleAt, w.SessionID,
	)
	if err != nil {
		return false, fmt.Errorf("store: apply outcome: %w", err)
	}

	for _, event := range w.Events {
		if err := insertAuditLogTx(tx, w.SessionID, event.Type, event.Data); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit outcome: %w", err)
	}
	return true, nil
}

// SetNextEligibleAt reschedules a follow-up. It writes the timestamp and
// nothing else. It does not check whether the session should ever be
// contacted again, because that judgement belongs to the eligibleNow
// predicate. A session escalated to a human stays out of the queue even
// with a timer set, which is what makes hand-editing this column during a
// demo safe.
func (db *DB) SetNextEligibleAt(sessionID string, at time.Time) error {
	_, err := db.conn.Exec(
		`UPDATE recovery_sessions SET next_eligible_at = ? WHERE id = ?`,
		at.UTC(), sessionID,
	)
	if err != nil {
		return fmt.Errorf("store: set next eligible at: %w", err)
	}
	return nil
}

// MarkRecovered is the one single-statement status write that remains.
// Confirming a payment changes exactly one thing and is driven by a webhook
// whose own idempotency check sits in the campaign layer, so it does not
// need the transactional treatment RecordOutcome gives a call's outcome.
func (db *DB) MarkRecovered(sessionID string) error {
	// Clearing next_eligible_at is what actually stops the follow-up call.
	// A recovered customer whose 24-hour nudge timer was still set would
	// otherwise be phoned about a debt they have already settled.
	_, err := db.conn.Exec(
		`UPDATE recovery_sessions
		    SET status = 'recovered',
		        recovered_at = CURRENT_TIMESTAMP,
		        next_eligible_at = NULL
		  WHERE id = ?`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("store: mark recovered: %w", err)
	}
	return nil
}

// InsertAuditLog writes one immutable event row. eventData is marshaled to
// JSON as-is, pass a map[string]any or a small struct.
func (db *DB) InsertAuditLog(sessionID, eventType string, eventData any) error {
	return insertAuditLogTx(db.conn, sessionID, eventType, eventData)
}

// execer is satisfied by both *sql.DB and *sql.Tx, so audit rows are
// written identically whether or not they are part of a transaction.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertAuditLogTx(e execer, sessionID, eventType string, eventData any) error {
	var payload []byte
	if eventData != nil {
		var err error
		payload, err = json.Marshal(eventData)
		if err != nil {
			return fmt.Errorf("store: marshal audit payload: %w", err)
		}
	}

	_, err := e.Exec(
		`INSERT INTO audit_log (session_id, event_type, event_data) VALUES (?, ?, ?)`,
		sessionID, eventType, string(payload),
	)
	if err != nil {
		return fmt.Errorf("store: insert audit log: %w", err)
	}
	return nil
}

// AuditEventsForSession returns the event types recorded against a session
// in the order they occurred. The audit trail is the primary evidence this
// system produces, so being able to assert on its shape matters as much as
// being able to read it.
func (db *DB) AuditEventsForSession(sessionID string) ([]string, error) {
	rows, err := db.conn.Query(
		`SELECT event_type FROM audit_log WHERE session_id = ? ORDER BY id`, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: audit events: %w", err)
	}
	defer rows.Close()

	var events []string
	for rows.Next() {
		var eventType string
		if err := rows.Scan(&eventType); err != nil {
			return nil, fmt.Errorf("store: scan audit event: %w", err)
		}
		events = append(events, eventType)
	}
	return events, rows.Err()
}

// Queue buckets. A session is due now, waiting for a timer, or finished
// with, there is no fourth state.
const (
	BucketDueNow = "due_now"
	BucketOnHold = "on_hold"
	BucketClosed = "closed"
)

type QueueEntry struct {
	SessionID          string
	CustomerName       string
	OutstandingPaise   int64
	ProductName        string
	Status             string
	Bucket             string
	ContactAttempts    int
	MaxContactAttempts int
	NextEligibleAt     sql.NullTime
}

// CampaignQueue reports every session in a campaign with the scheduling
// decision that applies to it. The bucket is computed from the same
// eligibleNow predicate the assignment query uses, so this view cannot
// claim someone is due while the assigner disagrees.
func (db *DB) CampaignQueue(campaignID string) ([]QueueEntry, error) {
	rows, err := db.conn.Query(
		`SELECT rs.id, rs.customer_name, rs.outstanding_paise, rs.product_name,
		        rs.status, rs.contact_attempts, rs.max_contact_attempts,
		        rs.next_eligible_at,
		        CASE
		          WHEN `+eligibleNow+` THEN '`+BucketDueNow+`'
		          WHEN rs.next_eligible_at IS NOT NULL
		               AND rs.contact_attempts < rs.max_contact_attempts
		               AND rs.status IN ('pending', 'unclear', 'link_sent', 'promised')
		            THEN '`+BucketOnHold+`'
		          ELSE '`+BucketClosed+`'
		        END AS bucket
		   FROM recovery_sessions rs
		  WHERE rs.campaign_id = ?
		  ORDER BY rs.next_eligible_at ASC, rs.created_at ASC`,
		campaignID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: campaign queue: %w", err)
	}
	defer rows.Close()

	var entries []QueueEntry
	for rows.Next() {
		var e QueueEntry
		if err := rows.Scan(
			&e.SessionID, &e.CustomerName, &e.OutstandingPaise, &e.ProductName,
			&e.Status, &e.ContactAttempts, &e.MaxContactAttempts,
			&e.NextEligibleAt, &e.Bucket,
		); err != nil {
			return nil, fmt.Errorf("store: scan queue entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

type Metrics struct {
	TotalAccounts        int
	Contacted            int
	Recovered            int
	RecoveredAmountPaise int64
	Promised             int
	PromisedAmountPaise  int64
	Refused              int
	Unclear              int
	Pending              int
	StoppedMaxAttempts   int
	PaymentLinksSent     int
	RazorpayCaptured     int
}

func (db *DB) CampaignMetrics(campaignID string) (*Metrics, error) {
	var m Metrics

	row := db.conn.QueryRow(
		`SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE status != 'pending'),
			COUNT(*) FILTER (WHERE status = 'recovered'),
			COALESCE(SUM(outstanding_paise) FILTER (WHERE status = 'recovered'), 0),
			COUNT(*) FILTER (WHERE status = 'promised'),
			COALESCE(SUM(outstanding_paise) FILTER (WHERE status = 'promised'), 0),
			COUNT(*) FILTER (WHERE status = 'refused'),
			COUNT(*) FILTER (WHERE status = 'unclear'),
			COUNT(*) FILTER (WHERE status = 'pending'),
			COUNT(*) FILTER (WHERE status = 'failed'),
			COUNT(*) FILTER (WHERE razorpay_link_id IS NOT NULL)
		   FROM recovery_sessions WHERE campaign_id = ?`,
		campaignID,
	)
	err := row.Scan(
		&m.TotalAccounts, &m.Contacted, &m.Recovered, &m.RecoveredAmountPaise,
		&m.Promised, &m.PromisedAmountPaise, &m.Refused, &m.Unclear, &m.Pending,
		&m.StoppedMaxAttempts, &m.PaymentLinksSent,
	)
	if err != nil {
		return nil, fmt.Errorf("store: campaign metrics: %w", err)
	}

	err = db.conn.QueryRow(
		`SELECT COUNT(*) FROM audit_log a
		   JOIN recovery_sessions rs ON rs.id = a.session_id
		  WHERE rs.campaign_id = ? AND a.event_type = 'payment_captured'`,
		campaignID,
	).Scan(&m.RazorpayCaptured)
	if err != nil {
		return nil, fmt.Errorf("store: campaign metrics captured count: %w", err)
	}

	return &m, nil
}

func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
