package store

import (
	"path/filepath"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "store_test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedSession creates a campaign with one account and binds it to a call,
// leaving the session in the active state an outcome is recorded from.
func seedSession(t *testing.T, db *DB) *RecoverySession {
	t.Helper()

	if err := db.InsertCampaign("camp_1", "Test", 1); err != nil {
		t.Fatalf("insert campaign: %v", err)
	}
	acc := Account{
		CustomerName:      "Rahul Sharma",
		OutstandingPaise:  420000,
		ProductName:       "Bajaj Finance Personal Loan",
		DueDate:           "2026-07-15",
		RazorpayPaymentID: "pay_1",
	}
	if err := db.InsertRecoverySession("rec_1", "camp_1", "prompt", acc); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	sess, err := db.AssignNextPending("call_1")
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if sess == nil {
		t.Fatal("assign returned no session")
	}
	return sess
}

func TestRecordOutcomeAppliesStatusAndAuditTogether(t *testing.T) {
	db := newTestDB(t)
	sess := seedSession(t, db)

	applied, err := db.RecordOutcome(OutcomeWrite{
		SessionID:      sess.ID,
		Status:         StatusLinkSent,
		RazorpayLinkID: "plink_abc",
		Events: []AuditEvent{
			{Type: "call_ended", Data: map[string]any{"outcome": "AGREED"}},
			{Type: "payment_link_sent", Data: map[string]any{"razorpay_link_id": "plink_abc"}},
		},
	}, []string{StatusActive})
	if err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	if !applied {
		t.Fatal("outcome was not applied from an allowed status")
	}

	updated, err := db.GetSessionByID(sess.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if updated.Status != StatusLinkSent {
		t.Errorf("status = %q, want %q", updated.Status, StatusLinkSent)
	}
	if updated.RazorpayLinkID.String != "plink_abc" {
		t.Errorf("razorpay_link_id = %q, want plink_abc", updated.RazorpayLinkID.String)
	}
	if !updated.CallEndedAt.Valid {
		t.Error("call_ended_at was not set")
	}

	events, err := db.AuditEventsForSession(sess.ID)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	// session_created is not written by the store, so the trail here is
	// exactly what RecordOutcome contributed.
	if len(events) != 2 {
		t.Fatalf("audit events = %v, want exactly the two supplied", events)
	}
}

// The point of the guard living inside the transaction: when it rejects,
// nothing at all is written. A partial apply would leave the audit trail
// disagreeing with the status it is supposed to explain.
func TestRecordOutcomeRejectionWritesNothing(t *testing.T) {
	db := newTestDB(t)
	sess := seedSession(t, db)

	before, err := db.AuditEventsForSession(sess.ID)
	if err != nil {
		t.Fatalf("audit before: %v", err)
	}

	applied, err := db.RecordOutcome(OutcomeWrite{
		SessionID:      sess.ID,
		Status:         StatusLinkSent,
		RazorpayLinkID: "plink_should_not_land",
		Events: []AuditEvent{
			{Type: "call_ended", Data: map[string]any{"outcome": "AGREED"}},
			{Type: "payment_link_sent", Data: nil},
		},
	}, []string{StatusRefused}) // the session is active, not refused
	if err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	if applied {
		t.Fatal("outcome was applied from a status that is not allowed")
	}

	updated, err := db.GetSessionByID(sess.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if updated.Status != StatusActive {
		t.Errorf("status = %q, want it unchanged at %q", updated.Status, StatusActive)
	}
	if updated.RazorpayLinkID.Valid {
		t.Errorf("razorpay_link_id = %q, want it unwritten", updated.RazorpayLinkID.String)
	}
	if updated.CallEndedAt.Valid {
		t.Error("call_ended_at was written despite the outcome being rejected")
	}

	after, err := db.AuditEventsForSession(sess.ID)
	if err != nil {
		t.Fatalf("audit after: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("audit events went from %d to %d; a rejected outcome wrote rows", len(before), len(after))
	}
}

// Only the first of two competing writes may land. This is what stops a
// second, later request from overwriting a settled outcome.
func TestRecordOutcomeSecondWriteIsRejected(t *testing.T) {
	db := newTestDB(t)
	sess := seedSession(t, db)

	allowed := []string{StatusActive, StatusUnclear}

	first, err := db.RecordOutcome(OutcomeWrite{
		SessionID: sess.ID,
		Status:    StatusRefused,
		Events:    []AuditEvent{{Type: "stopped_refused"}},
	}, allowed)
	if err != nil || !first {
		t.Fatalf("first write: applied=%v err=%v", first, err)
	}

	second, err := db.RecordOutcome(OutcomeWrite{
		SessionID:      sess.ID,
		Status:         StatusLinkSent,
		RazorpayLinkID: "plink_override",
		Events:         []AuditEvent{{Type: "payment_link_sent"}},
	}, allowed)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if second {
		t.Fatal("a settled refusal was overwritten")
	}

	updated, _ := db.GetSessionByID(sess.ID)
	if updated.Status != StatusRefused {
		t.Errorf("status = %q, want it to remain %q", updated.Status, StatusRefused)
	}
}

// promise_date and razorpay_link_id are only overwritten when the write
// carries a value, so recording an outcome cannot blank a field that
// another path already set.
func TestRecordOutcomeLeavesUnsetFieldsAlone(t *testing.T) {
	db := newTestDB(t)
	sess := seedSession(t, db)

	if _, err := db.RecordOutcome(OutcomeWrite{
		SessionID:   sess.ID,
		Status:      StatusPromised,
		PromiseDate: "2026-09-05",
		Events:      []AuditEvent{{Type: "promise_logged"}},
	}, []string{StatusActive}); err != nil {
		t.Fatalf("promise: %v", err)
	}

	// A later recovery carries no promise date; the original must survive.
	changed, err := db.MarkRecovered(sess.ID)
	if err != nil {
		t.Fatalf("mark recovered: %v", err)
	}
	if !changed {
		t.Fatal("MarkRecovered reported no change on a session that was not yet recovered")
	}

	updated, _ := db.GetSessionByID(sess.ID)
	if updated.PromiseDate.String != "2026-09-05" {
		t.Errorf("promise_date = %q, want it preserved", updated.PromiseDate.String)
	}
}

func TestRecordOutcomeUnknownSession(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.RecordOutcome(OutcomeWrite{
		SessionID: "rec_missing", Status: StatusRefused,
	}, []string{StatusActive}); err == nil {
		t.Error("expected an error for a session that does not exist")
	}
}

// AssignNextPending must hand a given session to exactly one call.
func TestAssignNextPendingDoesNotDoubleAssign(t *testing.T) {
	db := newTestDB(t)
	seedSession(t, db)

	second, err := db.AssignNextPending("call_2")
	if err != nil {
		t.Fatalf("second assign: %v", err)
	}
	if second != nil {
		t.Errorf("a second call was assigned session %s from an empty queue", second.ID)
	}
}

func TestAssignNextPendingSkipsInactiveCampaigns(t *testing.T) {
	db := newTestDB(t)
	if err := db.InsertCampaign("camp_paused", "Paused", 1); err != nil {
		t.Fatalf("insert campaign: %v", err)
	}
	if err := db.InsertRecoverySession("rec_p", "camp_paused", "prompt", Account{
		CustomerName: "A", OutstandingPaise: 1000, ProductName: "P", DueDate: "2026-07-01",
	}); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.conn.Exec(`UPDATE campaigns SET status = 'paused' WHERE id = ?`, "camp_paused"); err != nil {
		t.Fatalf("pause campaign: %v", err)
	}

	sess, err := db.AssignNextPending("call_1")
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if sess != nil {
		t.Error("a session from a paused campaign was assigned")
	}
}
