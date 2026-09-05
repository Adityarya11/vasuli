package campaign

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vasuli/recovery-orchestrator/internal/razorpay"
	"vasuli/recovery-orchestrator/internal/store"
)

// countingRazorpay records how many payment links were requested. Several
// tests assert on that count rather than on stored state, because the
// damage from a non-idempotent EndSession is the second API call itself,
// a real payment demand, not the database row it leaves behind.
type countingRazorpay struct {
	calls int
	err   error
}

func (c *countingRazorpay) CreatePaymentLink(ctx context.Context, req razorpay.PaymentLinkRequest) (*razorpay.PaymentLinkResponse, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return &razorpay.PaymentLinkResponse{
		ID:       "plink_test_" + string(rune('a'+c.calls)),
		ShortURL: "https://rzp.io/i/test",
	}, nil
}

func newTestManager(t *testing.T) (*Manager, *store.DB, *countingRazorpay) {
	t.Helper()

	// A file in t.TempDir rather than :memory:, so the database behaves
	// exactly as it does in production, including on-disk constraints.
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rzp := &countingRazorpay{}
	manager, err := NewManager(db, rzp)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return manager, db, rzp
}

func sampleAccounts(n int) []store.Account {
	accounts := make([]store.Account, n)
	for i := range accounts {
		accounts[i] = store.Account{
			CustomerName:      "Customer " + string(rune('A'+i)),
			OutstandingPaise:  int64(100000 * (i + 1)),
			ProductName:       "Test Product",
			DueDate:           "2026-07-15",
			RazorpayPaymentID: "pay_test_" + string(rune('a'+i)),
		}
	}
	return accounts
}

func assignAndGet(t *testing.T, m *Manager, db *store.DB, callID string) *store.RecoverySession {
	t.Helper()
	sess, err := m.AssignSession(callID)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if sess == nil {
		t.Fatal("assign returned no session; queue unexpectedly empty")
	}
	return sess
}

func statusOf(t *testing.T, db *store.DB, callID string) string {
	t.Helper()
	sess, err := db.GetSessionByCallSessionID(callID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if sess == nil {
		t.Fatalf("no session bound to %q", callID)
	}
	return sess.Status
}

func countAudit(t *testing.T, db *store.DB, sessionID, eventType string) int {
	t.Helper()
	events, err := db.AuditEventsForSession(sessionID)
	if err != nil {
		t.Fatalf("audit lookup: %v", err)
	}
	n := 0
	for _, e := range events {
		if e == eventType {
			n++
		}
	}
	return n
}

func TestCreateCampaignRendersPromptPerCustomer(t *testing.T) {
	m, db, _ := newTestManager(t)

	c, err := m.CreateCampaign("Test Batch", sampleAccounts(2))
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	if c.Total != 2 {
		t.Errorf("Total = %d, want 2", c.Total)
	}

	sess := assignAndGet(t, m, db, "call_1")
	if sess.SystemPrompt == "" {
		t.Fatal("system prompt is empty")
	}
	// The prompt must carry this customer's details, not a template.
	if !strings.Contains(sess.SystemPrompt, sess.CustomerName) {
		t.Errorf("system prompt does not name the customer %q", sess.CustomerName)
	}
	if strings.Contains(sess.SystemPrompt, "{{") {
		t.Error("system prompt still contains unrendered template syntax")
	}
}

func TestCreateCampaignRejectsEmptyAccounts(t *testing.T) {
	m, _, _ := newTestManager(t)
	if _, err := m.CreateCampaign("Empty", nil); err == nil {
		t.Error("expected an error creating a campaign with no accounts")
	}
}

func TestAssignSessionIsFIFOAndBindsCall(t *testing.T) {
	m, db, _ := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(2)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	first := assignAndGet(t, m, db, "call_1")
	second := assignAndGet(t, m, db, "call_2")

	if first.ID == second.ID {
		t.Fatal("the same session was assigned to two calls")
	}
	if first.Status != store.StatusActive {
		t.Errorf("status = %q, want %q", first.Status, store.StatusActive)
	}
	if first.ContactAttempts != 1 {
		t.Errorf("ContactAttempts = %d, want 1", first.ContactAttempts)
	}
	if !first.CallSessionID.Valid || first.CallSessionID.String != "call_1" {
		t.Errorf("call_session_id = %v, want call_1", first.CallSessionID)
	}
}

// An empty queue is an expected operating condition, not an error:
// var-thon distinguishes the two to decide whether to fall back to its
// static profile.
func TestAssignSessionEmptyQueueReturnsNilNil(t *testing.T) {
	m, _, _ := newTestManager(t)

	sess, err := m.AssignSession("call_1")
	if err != nil {
		t.Fatalf("expected no error on empty queue, got %v", err)
	}
	if sess != nil {
		t.Errorf("expected nil session on empty queue, got %+v", sess)
	}
}

func TestEndSessionAgreedCreatesOneLink(t *testing.T) {
	m, db, rzp := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	sess := assignAndGet(t, m, db, "call_1")

	if err := m.EndSession(context.Background(), "call_1", OutcomeAgreed, ""); err != nil {
		t.Fatalf("end session: %v", err)
	}

	if rzp.calls != 1 {
		t.Errorf("payment link calls = %d, want 1", rzp.calls)
	}
	if got := statusOf(t, db, "call_1"); got != store.StatusLinkSent {
		t.Errorf("status = %q, want %q", got, store.StatusLinkSent)
	}
	if n := countAudit(t, db, sess.ID, "payment_link_sent"); n != 1 {
		t.Errorf("payment_link_sent audit rows = %d, want 1", n)
	}
}

// The regression this guards: recording AGREED twice used to create a
// second real payment link and duplicate the audit trail.
func TestEndSessionIsIdempotent(t *testing.T) {
	m, db, rzp := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	sess := assignAndGet(t, m, db, "call_1")

	if err := m.EndSession(context.Background(), "call_1", OutcomeAgreed, ""); err != nil {
		t.Fatalf("first end: %v", err)
	}

	err := m.EndSession(context.Background(), "call_1", OutcomeAgreed, "")
	if !errors.Is(err, ErrOutcomeAlreadyRecorded) {
		t.Fatalf("second end: got %v, want ErrOutcomeAlreadyRecorded", err)
	}

	if rzp.calls != 1 {
		t.Errorf("payment link calls = %d, want 1, a duplicate end created a second link", rzp.calls)
	}
	if n := countAudit(t, db, sess.ID, "call_ended"); n != 1 {
		t.Errorf("call_ended audit rows = %d, want 1", n)
	}
}

// A refused customer must stay refused. This is the stopping rule, and
// losing it means re-engaging someone who declined.
func TestEndSessionCannotOverrideRefusal(t *testing.T) {
	m, db, rzp := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	assignAndGet(t, m, db, "call_1")

	if err := m.EndSession(context.Background(), "call_1", OutcomeRefused, ""); err != nil {
		t.Fatalf("refuse: %v", err)
	}

	err := m.EndSession(context.Background(), "call_1", OutcomeAgreed, "")
	if !errors.Is(err, ErrOutcomeAlreadyRecorded) {
		t.Fatalf("override attempt: got %v, want ErrOutcomeAlreadyRecorded", err)
	}
	if got := statusOf(t, db, "call_1"); got != store.StatusRefused {
		t.Errorf("status = %q, want it to remain %q", got, store.StatusRefused)
	}
	if rzp.calls != 0 {
		t.Errorf("payment link calls = %d, want 0 for a refused customer", rzp.calls)
	}
}

// unclear means the classifier reached no verdict, so an operator
// correcting it by hand is a documented action rather than a duplicate.
func TestEndSessionAllowsCorrectingUnclear(t *testing.T) {
	m, db, rzp := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	assignAndGet(t, m, db, "call_1")

	if err := m.EndSession(context.Background(), "call_1", OutcomeUnclear, ""); err != nil {
		t.Fatalf("unclear: %v", err)
	}
	if got := statusOf(t, db, "call_1"); got != store.StatusUnclear {
		t.Fatalf("status = %q, want %q", got, store.StatusUnclear)
	}

	if err := m.EndSession(context.Background(), "call_1", OutcomeAgreed, ""); err != nil {
		t.Fatalf("manual correction: %v", err)
	}
	if got := statusOf(t, db, "call_1"); got != store.StatusLinkSent {
		t.Errorf("status = %q, want %q after correction", got, store.StatusLinkSent)
	}
	if rzp.calls != 1 {
		t.Errorf("payment link calls = %d, want 1", rzp.calls)
	}
}

func TestEndSessionPromisedStoresDate(t *testing.T) {
	m, db, rzp := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	assignAndGet(t, m, db, "call_1")

	if err := m.EndSession(context.Background(), "call_1", OutcomePromised, "2026-09-05"); err != nil {
		t.Fatalf("promise: %v", err)
	}

	sess, _ := db.GetSessionByCallSessionID("call_1")
	if sess.Status != store.StatusPromised {
		t.Errorf("status = %q, want %q", sess.Status, store.StatusPromised)
	}
	if !sess.PromiseDate.Valid || sess.PromiseDate.String != "2026-09-05" {
		t.Errorf("promise_date = %v, want 2026-09-05", sess.PromiseDate)
	}
	if rzp.calls != 0 {
		t.Errorf("payment link calls = %d, want 0 for a promise", rzp.calls)
	}
}

func TestEndSessionUnknownCallIsAnError(t *testing.T) {
	m, _, _ := newTestManager(t)
	if err := m.EndSession(context.Background(), "never_assigned", OutcomeAgreed, ""); err == nil {
		t.Error("expected an error ending a call that was never assigned")
	}
}

// A failing payment provider must not leave the session claiming a link
// was sent.
func TestEndSessionSurfacesPaymentLinkFailure(t *testing.T) {
	m, db, rzp := newTestManager(t)
	rzp.err = errors.New("razorpay unavailable")
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	sess := assignAndGet(t, m, db, "call_1")

	if err := m.EndSession(context.Background(), "call_1", OutcomeAgreed, ""); err == nil {
		t.Fatal("expected an error when payment link creation fails")
	}
	if got := statusOf(t, db, "call_1"); got == store.StatusLinkSent {
		t.Error("status is link_sent even though no link was created")
	}
	if n := countAudit(t, db, sess.ID, "payment_link_failed"); n != 1 {
		t.Errorf("payment_link_failed audit rows = %d, want 1", n)
	}
}

func TestHandlePaymentLinkPaidRecoversByLinkID(t *testing.T) {
	m, db, _ := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	assignAndGet(t, m, db, "call_1")
	if err := m.EndSession(context.Background(), "call_1", OutcomeAgreed, ""); err != nil {
		t.Fatalf("end session: %v", err)
	}

	sess, _ := db.GetSessionByCallSessionID("call_1")
	linkID := sess.RazorpayLinkID.String

	// The payment id here is deliberately unrelated to the failed payment
	// that started recovery: that is what Razorpay actually sends, and
	// matching on it instead of the link id resolves nothing.
	recovered, err := m.HandlePaymentLinkPaid(linkID, "pay_completely_unrelated")
	if err != nil {
		t.Fatalf("link paid: %v", err)
	}
	if !recovered {
		t.Error("expected the session to be newly recovered")
	}
	if got := statusOf(t, db, "call_1"); got != store.StatusRecovered {
		t.Errorf("status = %q, want %q", got, store.StatusRecovered)
	}

	again, err := m.HandlePaymentLinkPaid(linkID, "pay_completely_unrelated")
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if again {
		t.Error("redelivery reported a second recovery")
	}
	if n := countAudit(t, db, sess.ID, "payment_captured"); n != 1 {
		t.Errorf("payment_captured audit rows = %d, want 1", n)
	}
}

func TestHandlePaymentLinkPaidUnknownLink(t *testing.T) {
	m, _, _ := newTestManager(t)
	_, err := m.HandlePaymentLinkPaid("plink_does_not_exist", "pay_x")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("got %v, want ErrSessionNotFound", err)
	}
}

func TestHandlePaymentFailedRequiresActiveCampaign(t *testing.T) {
	m, _, _ := newTestManager(t)

	_, _, err := m.HandlePaymentFailed(razorpay.PaymentEntity{
		ID: "pay_orphan", Amount: 50000, Description: "Orphan",
	})
	if !errors.Is(err, ErrNoActiveCampaign) {
		t.Errorf("got %v, want ErrNoActiveCampaign", err)
	}
}

func TestHandlePaymentFailedDedupesRedelivery(t *testing.T) {
	m, db, _ := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	payment := razorpay.PaymentEntity{
		ID: "pay_inbound", Amount: 189000, Description: "Airtel Postpaid Bill",
		Notes: map[string]string{"customer_name": "Preethi Nair"},
	}

	first, created, err := m.HandlePaymentFailed(payment)
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if !created {
		t.Error("first delivery did not report creating a session")
	}
	if first.CustomerName != "Preethi Nair" {
		t.Errorf("CustomerName = %q, want Preethi Nair", first.CustomerName)
	}

	second, created, err := m.HandlePaymentFailed(payment)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if created {
		t.Error("redelivery created a duplicate session")
	}
	if second.ID != first.ID {
		t.Errorf("redelivery returned a different session: %s vs %s", second.ID, first.ID)
	}

	metrics, err := db.CampaignMetrics(first.CampaignID)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if metrics.TotalAccounts != 2 {
		t.Errorf("TotalAccounts = %d, want 2 (one seeded, one inbound)", metrics.TotalAccounts)
	}
}

func TestMetricsReflectOutcomes(t *testing.T) {
	m, db, _ := newTestManager(t)
	c, err := m.CreateCampaign("Batch", sampleAccounts(3))
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	assignAndGet(t, m, db, "call_1")
	if err := m.EndSession(context.Background(), "call_1", OutcomeAgreed, ""); err != nil {
		t.Fatalf("agreed: %v", err)
	}
	assignAndGet(t, m, db, "call_2")
	if err := m.EndSession(context.Background(), "call_2", OutcomeRefused, ""); err != nil {
		t.Fatalf("refused: %v", err)
	}

	metrics, err := m.Metrics(c.ID)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}

	if metrics.TotalAccounts != 3 {
		t.Errorf("TotalAccounts = %d, want 3", metrics.TotalAccounts)
	}
	if metrics.Contacted != 2 {
		t.Errorf("Contacted = %d, want 2", metrics.Contacted)
	}
	if metrics.Pending != 1 {
		t.Errorf("Pending = %d, want 1", metrics.Pending)
	}
	if metrics.Refused != 1 {
		t.Errorf("Refused = %d, want 1", metrics.Refused)
	}
	if metrics.PaymentLinksSent != 1 {
		t.Errorf("PaymentLinksSent = %d, want 1", metrics.PaymentLinksSent)
	}
	// Not yet recovered: a link was sent but no payment is confirmed.
	if metrics.Recovered != 0 {
		t.Errorf("Recovered = %d, want 0 before payment confirmation", metrics.Recovered)
	}
}

// Razorpay delivers webhooks at least once and gives no guarantee that two
// deliveries of the same event arrive one after the other. This fires them
// concurrently, which is the case a sequential redelivery test cannot
// reach.
//
// The guard used to be a status check on a session read a moment earlier,
// so both goroutines could observe "not yet recovered" before either wrote,
// and both would append a payment_captured row. That row is what the
// confirmed-recovery metric counts, so the duplicate showed up as a
// merchant being told two payments landed when one did. The status is now
// part of the UPDATE's WHERE clause, which makes the database the arbiter.
func TestHandlePaymentLinkPaidConcurrentDeliveries(t *testing.T) {
	m, db, _ := newTestManager(t)
	if _, err := m.CreateCampaign("Batch", sampleAccounts(1)); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	assignAndGet(t, m, db, "call_1")
	if err := m.EndSession(context.Background(), "call_1", OutcomeAgreed, ""); err != nil {
		t.Fatalf("end session: %v", err)
	}

	sess, _ := db.GetSessionByCallSessionID("call_1")
	linkID := sess.RazorpayLinkID.String

	const deliveries = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		recovered int
	)
	for i := 0; i < deliveries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := m.HandlePaymentLinkPaid(linkID, "pay_unrelated")
			if err != nil {
				t.Errorf("delivery: %v", err)
				return
			}
			if ok {
				mu.Lock()
				recovered++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if recovered != 1 {
		t.Errorf("%d of %d concurrent deliveries reported a recovery, want exactly 1", recovered, deliveries)
	}
	if n := countAudit(t, db, sess.ID, "payment_captured"); n != 1 {
		t.Errorf("payment_captured audit rows = %d, want 1; the confirmed-recovery metric counts these", n)
	}
}
