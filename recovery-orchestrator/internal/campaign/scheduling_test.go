package campaign

import (
	"context"
	"testing"
	"time"

	"vasuli/recovery-orchestrator/internal/store"
)

// backdate simulates the passage of time by moving a session's follow-up
// timer into the past. This is the same edit the demo performs on camera,
// and it is the only way to exercise a 24-hour cooldown in a test that
// finishes in milliseconds.
func backdate(t *testing.T, db *store.DB, sessionID string) {
	t.Helper()
	if err := db.SetNextEligibleAt(sessionID, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

func nextEligible(t *testing.T, db *store.DB, sessionID string) (time.Time, bool) {
	t.Helper()
	sess, err := db.GetSessionByID(sessionID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return sess.NextEligibleAt.Time, sess.NextEligibleAt.Valid
}

// A customer whose call reached no conclusion must be called back once the
// cooldown expires, not abandoned.
func TestUnclearReturnsToQueueAfterCooldown(t *testing.T) {
	m, db, _ := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	sess := assignAndGet(t, m, db, "call_1")

	if err := m.EndSession(context.Background(), "call_1", OutcomeUnclear, ""); err != nil {
		t.Fatalf("unclear: %v", err)
	}

	// Still inside the cooldown, so nothing is due.
	if again, err := m.AssignSession("call_2"); err != nil || again != nil {
		t.Fatalf("assigned during cooldown: session=%v err=%v", again, err)
	}

	backdate(t, db, sess.ID)

	returned, err := m.AssignSession("call_2")
	if err != nil {
		t.Fatalf("assign after cooldown: %v", err)
	}
	if returned == nil {
		t.Fatal("customer never returned to the queue after the cooldown expired")
	}
	if returned.ID != sess.ID {
		t.Errorf("returned session %s, want the original %s", returned.ID, sess.ID)
	}
	if returned.ContactAttempts != 2 {
		t.Errorf("ContactAttempts = %d, want 2 — a callback is another attempt", returned.ContactAttempts)
	}
}

// An unpaid payment link is the whole point of following up: the link has
// expired by the time the cooldown ends, so the customer needs a new one.
func TestUnpaidLinkReturnsToQueue(t *testing.T) {
	m, db, rzp := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	sess := assignAndGet(t, m, db, "call_1")

	if err := m.EndSession(context.Background(), "call_1", OutcomeAgreed, ""); err != nil {
		t.Fatalf("agreed: %v", err)
	}
	if got := statusOf(t, db, "call_1"); got != store.StatusLinkSent {
		t.Fatalf("status = %q, want %q", got, store.StatusLinkSent)
	}

	backdate(t, db, sess.ID)

	returned, err := m.AssignSession("call_2")
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if returned == nil {
		t.Fatal("a customer with an unpaid, expired link was never called back")
	}

	// The follow-up call agrees again and issues a fresh link.
	if err := m.EndSession(context.Background(), "call_2", OutcomeAgreed, ""); err != nil {
		t.Fatalf("second agreed: %v", err)
	}
	if rzp.calls != 2 {
		t.Errorf("payment link calls = %d, want 2 — the expired link should be replaced", rzp.calls)
	}
}

// Confirming payment must cancel the pending follow-up, or the system
// phones someone about a debt they have already settled.
func TestRecoveryCancelsTheFollowUp(t *testing.T) {
	m, db, _ := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	sess := assignAndGet(t, m, db, "call_1")
	if err := m.EndSession(context.Background(), "call_1", OutcomeAgreed, ""); err != nil {
		t.Fatalf("agreed: %v", err)
	}

	if _, valid := nextEligible(t, db, sess.ID); !valid {
		t.Fatal("no follow-up was scheduled after a link was sent")
	}

	updated, _ := db.GetSessionByID(sess.ID)
	if _, err := m.HandlePaymentLinkPaid(updated.RazorpayLinkID.String, "pay_new"); err != nil {
		t.Fatalf("link paid: %v", err)
	}

	if _, valid := nextEligible(t, db, sess.ID); valid {
		t.Error("a recovered customer still has a follow-up call scheduled")
	}

	backdate(t, db, sess.ID) // no-op on NULL, but proves it cannot be revived
	if again, err := m.AssignSession("call_2"); err != nil || again != nil {
		t.Errorf("a recovered customer was queued again: session=%v err=%v", again, err)
	}
}

// The stopping rule that matters most: a disputed debt goes to a human and
// the agent never dials it again, no matter how much time passes.
func TestEscalatedCustomerNeverReturns(t *testing.T) {
	m, db, _ := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	sess := assignAndGet(t, m, db, "call_1")

	if err := m.EndSession(context.Background(), "call_1", OutcomeRefused, ""); err != nil {
		t.Fatalf("refused: %v", err)
	}

	if _, valid := nextEligible(t, db, sess.ID); valid {
		t.Error("an escalated customer has a follow-up scheduled")
	}

	// Even a hand-edited timer must not bring them back — the status check
	// in the eligibility predicate is the second lock on this door.
	backdate(t, db, sess.ID)

	if again, err := m.AssignSession("call_2"); err != nil || again != nil {
		t.Errorf("an escalated customer was re-queued: session=%v err=%v", again, err)
	}
}

// A promise with a real date is honoured rather than collapsed into the
// standard cooldown.
func TestPromiseDateSchedulesTheFollowUp(t *testing.T) {
	m, db, _ := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	sess := assignAndGet(t, m, db, "call_1")

	future := time.Now().UTC().AddDate(0, 0, 10).Format("2006-01-02")
	if err := m.EndSession(context.Background(), "call_1", OutcomePromised, future); err != nil {
		t.Fatalf("promised: %v", err)
	}

	at, valid := nextEligible(t, db, sess.ID)
	if !valid {
		t.Fatal("a promise scheduled no follow-up at all")
	}
	if got := at.Format("2006-01-02"); got != future {
		t.Errorf("follow-up scheduled for %s, want the promised date %s", got, future)
	}

	// Ten days out is well past the 24-hour cooldown, so nothing is due.
	if again, err := m.AssignSession("call_2"); err != nil || again != nil {
		t.Errorf("called back before the promised date: session=%v err=%v", again, err)
	}
}

// Outcome classification returns one word and never a date, so most
// promises arrive without one. They still need a follow-up.
func TestPromiseWithoutDateFallsBackToCooldown(t *testing.T) {
	m, db, _ := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	sess := assignAndGet(t, m, db, "call_1")

	if err := m.EndSession(context.Background(), "call_1", OutcomePromised, ""); err != nil {
		t.Fatalf("promised: %v", err)
	}

	at, valid := nextEligible(t, db, sess.ID)
	if !valid {
		t.Fatal("a dateless promise scheduled no follow-up")
	}
	if delta := time.Until(at); delta < 23*time.Hour || delta > 25*time.Hour {
		t.Errorf("follow-up in %v, want roughly the 24h cooldown", delta)
	}
}

// The workflow is bounded: repeated inconclusive calls stop permanently
// once the attempt budget is spent.
func TestAttemptsExhaustAndStopPermanently(t *testing.T) {
	m, db, _ := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	var sessionID string
	for attempt := 1; attempt <= 3; attempt++ {
		call := "call_" + string(rune('0'+attempt))

		got, err := m.AssignSession(call)
		if err != nil {
			t.Fatalf("attempt %d assign: %v", attempt, err)
		}
		if got == nil {
			t.Fatalf("attempt %d: nothing was due, expected the same customer back", attempt)
		}
		sessionID = got.ID

		if err := m.EndSession(context.Background(), call, OutcomeUnclear, ""); err != nil {
			t.Fatalf("attempt %d end: %v", attempt, err)
		}

		// Only fast-forward between attempts. Doing it after the last one
		// would overwrite the NULL that stopping the workflow just wrote,
		// and the assertion below is precisely that it stays NULL.
		if attempt < 3 {
			backdate(t, db, sessionID)
		}
	}

	sess, _ := db.GetSessionByID(sessionID)
	if sess.Status != store.StatusFailed {
		t.Errorf("status = %q, want %q after exhausting attempts", sess.Status, store.StatusFailed)
	}
	if sess.NextEligibleAt.Valid {
		t.Error("an exhausted session still has a follow-up scheduled")
	}

	if again, err := m.AssignSession("call_4"); err != nil || again != nil {
		t.Errorf("a fourth call was scheduled past the attempt limit: session=%v err=%v", again, err)
	}
}

// The queue view must agree with the assigner. A view that says someone is
// due while the assigner skips them is worse than no view at all.
func TestQueueViewMatchesWhatIsActuallyAssigned(t *testing.T) {
	m, db, _ := newTestManager(t)
	c, err := m.CreateCampaign("Batch", sampleAccounts(4))
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	// One escalated, one on hold, one recovered, one untouched.
	escalated := assignAndGet(t, m, db, "call_esc")
	if err := m.EndSession(context.Background(), "call_esc", OutcomeRefused, ""); err != nil {
		t.Fatalf("refuse: %v", err)
	}

	held := assignAndGet(t, m, db, "call_hold")
	if err := m.EndSession(context.Background(), "call_hold", OutcomeUnclear, ""); err != nil {
		t.Fatalf("unclear: %v", err)
	}

	paid := assignAndGet(t, m, db, "call_paid")
	if err := m.EndSession(context.Background(), "call_paid", OutcomeAgreed, ""); err != nil {
		t.Fatalf("agreed: %v", err)
	}
	paidRow, _ := db.GetSessionByID(paid.ID)
	if _, err := m.HandlePaymentLinkPaid(paidRow.RazorpayLinkID.String, "pay_x"); err != nil {
		t.Fatalf("link paid: %v", err)
	}

	entries, err := db.CampaignQueue(c.ID)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}

	bucket := map[string]string{}
	for _, e := range entries {
		bucket[e.SessionID] = e.Bucket
	}

	if got := bucket[escalated.ID]; got != store.BucketClosed {
		t.Errorf("escalated session bucket = %q, want %q", got, store.BucketClosed)
	}
	if got := bucket[held.ID]; got != store.BucketOnHold {
		t.Errorf("cooling-down session bucket = %q, want %q", got, store.BucketOnHold)
	}
	if got := bucket[paid.ID]; got != store.BucketClosed {
		t.Errorf("recovered session bucket = %q, want %q", got, store.BucketClosed)
	}

	// Whoever the view calls due must be exactly who the assigner hands out.
	var due []string
	for _, e := range entries {
		if e.Bucket == store.BucketDueNow {
			due = append(due, e.SessionID)
		}
	}
	if len(due) != 1 {
		t.Fatalf("queue reports %d sessions due, want 1", len(due))
	}

	assigned, err := m.AssignSession("call_next")
	if err != nil || assigned == nil {
		t.Fatalf("assign: session=%v err=%v", assigned, err)
	}
	if assigned.ID != due[0] {
		t.Errorf("queue said %s was due, assigner handed out %s", due[0], assigned.ID)
	}
}
