package razorpay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestLiveClient points a LiveClient at a local stand-in for Razorpay,
// so the request this code actually puts on the wire can be asserted
// without real credentials or network access.
func newTestLiveClient(t *testing.T, handler http.HandlerFunc) (*LiveClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client := NewLiveClient("rzp_test_abc123", "secret_xyz")
	client.apiBase = srv.URL
	return client, srv
}

func TestLiveClientRequestShape(t *testing.T) {
	var (
		gotPath   string
		gotAuth   string
		gotType   string
		gotBody   map[string]any
		beforeReq = time.Now().Unix()
	)

	client, _ := newTestLiveClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")

		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"plink_Test123","short_url":"https://rzp.io/i/Test123","status":"created"}`))
	})

	resp, err := client.CreatePaymentLink(context.Background(), PaymentLinkRequest{
		AmountPaise:  420000,
		Description:  "Payment recovery - Bajaj Finance Personal Loan",
		CustomerName: "Rahul Sharma",
	})
	if err != nil {
		t.Fatalf("CreatePaymentLink: %v", err)
	}

	if resp.ID != "plink_Test123" {
		t.Errorf("ID = %q, want plink_Test123", resp.ID)
	}
	if resp.ShortURL != "https://rzp.io/i/Test123" {
		t.Errorf("ShortURL = %q, want https://rzp.io/i/Test123", resp.ShortURL)
	}
	if gotPath != "/v1/payment_links" {
		t.Errorf("path = %q, want /v1/payment_links", gotPath)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("rzp_test_abc123:secret_xyz"))
	if gotAuth != wantAuth {
		t.Errorf("Authorization = %q, want %q", gotAuth, wantAuth)
	}

	if got := gotBody["amount"]; got != float64(420000) {
		t.Errorf("amount = %v, want 420000", got)
	}
	if got := gotBody["currency"]; got != "INR" {
		t.Errorf("currency = %v, want INR", got)
	}

	customer, ok := gotBody["customer"].(map[string]any)
	if !ok || customer["name"] != "Rahul Sharma" {
		t.Errorf("customer = %v, want name Rahul Sharma", gotBody["customer"])
	}

	// Notification must stay off: the demo's customer records are synthetic,
	// and enabling delivery would attempt to contact people who do not exist.
	notify, ok := gotBody["notify"].(map[string]any)
	if !ok || notify["sms"] != false || notify["email"] != false {
		t.Errorf("notify = %v, want sms and email both false", gotBody["notify"])
	}

	// Razorpay rejects expire_by less than 15 minutes out.
	expireBy, ok := gotBody["expire_by"].(float64)
	if !ok {
		t.Fatalf("expire_by missing or not a number: %v", gotBody["expire_by"])
	}
	if int64(expireBy) < beforeReq+15*60 {
		t.Errorf("expire_by = %d is inside Razorpay's 15 minute minimum (now %d)", int64(expireBy), beforeReq)
	}
}

// TestLiveClientSurfacesAPIError asserts that a rejection carries Razorpay's
// own description through, so a bad key or malformed amount is diagnosable
// from the audit log rather than requiring a dashboard visit.
func TestLiveClientSurfacesAPIError(t *testing.T) {
	client, _ := newTestLiveClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":"BAD_REQUEST_ERROR","description":"The amount must be atleast INR 1.00"}}`))
	})

	_, err := client.CreatePaymentLink(context.Background(), PaymentLinkRequest{AmountPaise: 1})
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if !strings.Contains(err.Error(), "The amount must be atleast INR 1.00") {
		t.Errorf("error = %v, want it to carry Razorpay's description", err)
	}
	if !strings.Contains(err.Error(), "BAD_REQUEST_ERROR") {
		t.Errorf("error = %v, want it to carry Razorpay's error code", err)
	}
}

func TestLiveClientRejectsEmptyID(t *testing.T) {
	client, _ := newTestLiveClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"short_url":"https://rzp.io/i/x"}`))
	})

	if _, err := client.CreatePaymentLink(context.Background(), PaymentLinkRequest{AmountPaise: 420000}); err == nil {
		t.Fatal("expected an error when the response carries no payment link id")
	}
}

func TestIsTestMode(t *testing.T) {
	tests := map[string]bool{
		"rzp_test_abc123": true,
		"rzp_live_abc123": false,
		"":                false,
		"test_abc":        false,
	}

	for keyID, want := range tests {
		if got := NewLiveClient(keyID, "s").IsTestMode(); got != want {
			t.Errorf("IsTestMode(%q) = %v, want %v", keyID, got, want)
		}
	}
}
