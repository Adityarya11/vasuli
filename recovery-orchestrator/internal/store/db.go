// Package store is the only place in recovery-orchestrator that talks SQL.
// Everything here is data access — business rules (stopping rules, outcome
// handling) live in the campaign package and call these methods.
package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
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

	return &DB{conn: conn}, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

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
	_, err := db.conn.Exec(
		`INSERT INTO recovery_sessions
			(id, campaign_id, customer_name, outstanding_paise, product_name,
			 due_date, razorpay_payment_id, system_prompt)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
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

// AssignNextPending atomically pops the oldest pending session belonging
// to an active campaign and binds it to callSessionID. Returns nil, nil
// if no eligible session exists (empty queue) — callers must distinguish
// this from an error and fall back accordingly.
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
		  WHERE rs.status = 'pending' AND c.status = 'active'
		  ORDER BY rs.created_at ASC
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

const sessionSelectCols = `
	SELECT id, campaign_id, call_session_id, customer_name, outstanding_paise,
	       product_name, due_date, razorpay_payment_id, system_prompt, status,
	       promise_date, razorpay_link_id, contact_attempts, max_contact_attempts,
	       created_at, call_started_at, call_ended_at, recovered_at
	  FROM recovery_sessions`

func (db *DB) scanSession(row *sql.Row) (*RecoverySession, error) {
	var s RecoverySession
	err := row.Scan(
		&s.ID, &s.CampaignID, &s.CallSessionID, &s.CustomerName, &s.OutstandingPaise,
		&s.ProductName, &s.DueDate, &s.RazorpayPaymentID, &s.SystemPrompt, &s.Status,
		&s.PromiseDate, &s.RazorpayLinkID, &s.ContactAttempts, &s.MaxContactAttempts,
		&s.CreatedAt, &s.CallStartedAt, &s.CallEndedAt, &s.RecoveredAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan recovery session: %w", err)
	}
	return &s, nil
}

func (db *DB) SetCallEnded(sessionID string) error {
	_, err := db.conn.Exec(
		`UPDATE recovery_sessions SET call_ended_at = CURRENT_TIMESTAMP WHERE id = ?`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("store: set call ended: %w", err)
	}
	return nil
}

func (db *DB) MarkLinkSent(sessionID, razorpayLinkID string) error {
	_, err := db.conn.Exec(
		`UPDATE recovery_sessions SET status = 'link_sent', razorpay_link_id = ? WHERE id = ?`,
		razorpayLinkID, sessionID,
	)
	if err != nil {
		return fmt.Errorf("store: mark link sent: %w", err)
	}
	return nil
}

func (db *DB) MarkPromised(sessionID, promiseDate string) error {
	_, err := db.conn.Exec(
		`UPDATE recovery_sessions SET status = 'promised', promise_date = ? WHERE id = ?`,
		promiseDate, sessionID,
	)
	if err != nil {
		return fmt.Errorf("store: mark promised: %w", err)
	}
	return nil
}

func (db *DB) MarkRefused(sessionID string) error {
	_, err := db.conn.Exec(`UPDATE recovery_sessions SET status = 'refused' WHERE id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("store: mark refused: %w", err)
	}
	return nil
}

func (db *DB) MarkUnclear(sessionID string) error {
	_, err := db.conn.Exec(`UPDATE recovery_sessions SET status = 'unclear' WHERE id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("store: mark unclear: %w", err)
	}
	return nil
}

func (db *DB) MarkFailedMaxAttempts(sessionID string) error {
	_, err := db.conn.Exec(`UPDATE recovery_sessions SET status = 'failed' WHERE id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("store: mark failed: %w", err)
	}
	return nil
}

func (db *DB) MarkRecovered(sessionID string) error {
	_, err := db.conn.Exec(
		`UPDATE recovery_sessions SET status = 'recovered', recovered_at = CURRENT_TIMESTAMP WHERE id = ?`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("store: mark recovered: %w", err)
	}
	return nil
}

// InsertAuditLog writes one immutable event row. eventData is marshaled to
// JSON as-is — pass a map[string]any or a small struct.
func (db *DB) InsertAuditLog(sessionID, eventType string, eventData any) error {
	var payload []byte
	if eventData != nil {
		var err error
		payload, err = json.Marshal(eventData)
		if err != nil {
			return fmt.Errorf("store: marshal audit payload: %w", err)
		}
	}

	_, err := db.conn.Exec(
		`INSERT INTO audit_log (session_id, event_type, event_data) VALUES (?, ?, ?)`,
		sessionID, eventType, string(payload),
	)
	if err != nil {
		return fmt.Errorf("store: insert audit log: %w", err)
	}
	return nil
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
