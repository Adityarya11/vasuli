package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"vasuli/recovery-orchestrator/internal/campaign"
	"vasuli/recovery-orchestrator/internal/razorpay"
	"vasuli/recovery-orchestrator/internal/store"
)

const testSecret = "whsec_api_test"

type stubRazorpay struct{ calls int }

func (s *stubRazorpay) CreatePaymentLink(ctx context.Context, req razorpay.PaymentLinkRequest) (*razorpay.PaymentLinkResponse, error) {
	s.calls++
	return &razorpay.PaymentLinkResponse{ID: "plink_api_test", ShortURL: "https://rzp.io/i/x"}, nil
}

func newTestServer(t *testing.T) (http.Handler, *store.DB, *campaign.Manager) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "api_test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	manager, err := campaign.NewManager(db, &stubRazorpay{})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	return NewRouter(NewHandlers(manager, db, testSecret)), db, manager
}

func postWebhook(t *testing.T, handler http.Handler, body string, secret string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")

	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		req.Header.Set("X-Razorpay-Signature", hex.EncodeToString(mac.Sum(nil)))
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// Status codes here are chosen for how Razorpay reacts to them, so they are
// worth asserting explicitly: 400 stops redelivery of something a retry
// cannot fix, 200 stops redelivery of something already handled, and 500
// is reserved for failures a retry genuinely could resolve.
func TestWebhookSignatureVerification(t *testing.T) {
	handler, _, _ := newTestServer(t)
	body := `{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_unknown"}}}}`

	tests := []struct {
		name   string
		secret string
		want   int
	}{
		{"valid signature accepted", testSecret, http.StatusOK},
		{"wrong secret rejected", "wrong_secret", http.StatusBadRequest},
		{"missing signature rejected", "", http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := postWebhook(t, handler, body, tc.secret).Code; got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWebhookRejectsTamperedBody(t *testing.T) {
	handler, _, _ := newTestServer(t)
	body := `{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_1"}}}}`

	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", bytes.NewReader([]byte(body+" ")))
	req.Header.Set("X-Razorpay-Signature", hex.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d for a body modified after signing", rec.Code, http.StatusBadRequest)
	}
}

// Events this system does not own are routine, not failures. Answering
// anything but 200 makes Razorpay redeliver them indefinitely.
func TestWebhookAcknowledgesUnownedEvents(t *testing.T) {
	handler, _, _ := newTestServer(t)

	tests := map[string]string{
		"unactionable event type": `{"event":"refund.created","payload":{}}`,
		"unknown payment id":      `{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_nobody"}}}}`,
		"unknown link id":         `{"event":"payment_link.paid","payload":{"payment_link":{"entity":{"id":"plink_nobody"}},"payment":{"entity":{"id":"pay_x"}}}}`,
		"no active campaign":      `{"event":"payment.failed","payload":{"payment":{"entity":{"id":"pay_orphan","amount":5000}}}}`,
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if got := postWebhook(t, handler, body, testSecret).Code; got != http.StatusOK {
				t.Errorf("status = %d, want 200", got)
			}
		})
	}
}

func TestWebhookRejectsMalformedJSON(t *testing.T) {
	handler, _, _ := newTestServer(t)
	if got := postWebhook(t, handler, `{"event":`, testSecret).Code; got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
}

// The full inbound path: a failed payment seeds a session, and a
// redelivery of the same event does not seed a second one.
func TestWebhookPaymentFailedSeedsSessionOnce(t *testing.T) {
	handler, db, manager := newTestServer(t)
	if _, err := manager.CreateCampaign("Batch", []store.Account{{
		CustomerName: "Seed", OutstandingPaise: 100000, ProductName: "Seed Product",
		DueDate: "2026-07-01", RazorpayPaymentID: "pay_seed",
	}}); err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	body := `{"event":"payment.failed","payload":{"payment":{"entity":{"id":"pay_inbound","amount":189000,"description":"Airtel Postpaid Bill","notes":{"customer_name":"Preethi Nair"}}}}}`

	for i := 0; i < 2; i++ {
		if got := postWebhook(t, handler, body, testSecret).Code; got != http.StatusOK {
			t.Fatalf("delivery %d: status = %d, want 200", i+1, got)
		}
	}

	sess, err := db.GetSessionByRazorpayPaymentID("pay_inbound")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if sess == nil {
		t.Fatal("no session created from payment.failed")
	}
	if sess.CustomerName != "Preethi Nair" {
		t.Errorf("CustomerName = %q, want Preethi Nair", sess.CustomerName)
	}

	events, err := db.AuditEventsForSession(sess.ID)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	created := 0
	for _, e := range events {
		if e == "session_created" {
			created++
		}
	}
	if created != 1 {
		t.Errorf("session_created audit rows = %d, want 1 after a redelivery", created)
	}
}

// A repeated end must be acknowledged, not reported as a server error:
// var-thon logs non-2xx responses, and a benign duplicate should not read
// as a failure.
func TestEndCallDuplicateIsAcknowledged(t *testing.T) {
	handler, _, manager := newTestServer(t)
	if _, err := manager.CreateCampaign("Batch", []store.Account{{
		CustomerName: "Rahul Sharma", OutstandingPaise: 420000, ProductName: "Loan EMI",
		DueDate: "2026-07-15", RazorpayPaymentID: "pay_1",
	}}); err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	if _, err := manager.AssignSession("call_1"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	end := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/calls/call_1/end",
			bytes.NewReader([]byte(`{"outcome":"AGREED"}`)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if got := end().Code; got != http.StatusOK {
		t.Fatalf("first end: status = %d, want 200", got)
	}

	second := end()
	if second.Code != http.StatusOK {
		t.Errorf("duplicate end: status = %d, want 200", second.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(second.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["note"] == "" {
		t.Error("duplicate end response gives no indication that nothing changed")
	}
}

func TestAssignReturns404OnEmptyQueue(t *testing.T) {
	handler, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/calls/assign",
		bytes.NewReader([]byte(`{"call_session_id":"call_1"}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// var-thon treats 404 as "queue empty, use the static profile" rather
	// than as a transport failure.
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 on an empty queue", rec.Code)
	}
}

func TestCreateCampaignValidatesInput(t *testing.T) {
	handler, _, _ := newTestServer(t)

	tests := map[string]string{
		"missing name":   `{"accounts":[{"customer_name":"A","outstanding_paise":1000}]}`,
		"no accounts":    `{"name":"Batch","accounts":[]}`,
		"malformed json": `{"name":`,
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/campaigns", bytes.NewReader([]byte(body)))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestHealth(t *testing.T) {
	handler, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
