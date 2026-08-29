package razorpay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// TestVerifyWebhookSignature covers the cases that decide whether an
// unauthenticated request can reach business logic. The rejection cases
// matter more than the acceptance one: a verifier that accepts everything
// still passes a happy-path test.
func TestVerifyWebhookSignature(t *testing.T) {
	const secret = "whsec_test_value"
	body := []byte(`{"event":"payment.captured","payload":{}}`)
	valid := sign(body, secret)

	tests := []struct {
		name      string
		body      []byte
		signature string
		secret    string
		want      bool
	}{
		{"valid signature", body, valid, secret, true},
		{"tampered body", append(body, ' '), valid, secret, false},
		{"wrong secret", body, sign(body, "other_secret"), secret, false},
		{"empty signature", body, "", secret, false},
		{"empty secret fails closed", body, valid, "", false},
		{"garbage signature", body, "not-hex", secret, false},
		{"empty body still verifiable", []byte{}, sign([]byte{}, secret), secret, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyWebhookSignature(tc.body, tc.signature, tc.secret); got != tc.want {
				t.Errorf("VerifyWebhookSignature() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVerifyRejectsReencodedBody pins the reason verification must run on
// raw bytes: semantically identical JSON produces a different signature, so
// parsing before verifying would break every legitimate webhook.
func TestVerifyRejectsReencodedBody(t *testing.T) {
	const secret = "whsec_test_value"

	// Indented as Razorpay may send it. Marshalling a decoded copy strips
	// this whitespace, which is exactly the byte difference that breaks the
	// signature. A compact, alphabetically-ordered payload would survive the
	// round trip unchanged and prove nothing.
	original := []byte("{\n  \"event\": \"payment.captured\",\n  \"payload\": {\n    \"payment\": {\"entity\": {\"id\": \"pay_1\"}}\n  }\n}")
	signature := sign(original, secret)

	var parsed map[string]any
	if err := json.Unmarshal(original, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reencoded, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if VerifyWebhookSignature(reencoded, signature, secret) {
		t.Error("re-encoded body verified against the original signature; " +
			"verification must operate on the bytes as received")
	}
}

// TestParsePaymentLinkPaid asserts the payload path the recovery lookup
// depends on. payment_link.paid carries both a link entity and a payment
// entity, and matching on the wrong one silently never resolves: the
// payment id here belongs to the new payment, not to the original failed
// payment that triggered recovery.
func TestParsePaymentLinkPaid(t *testing.T) {
	raw := []byte(`{
      "event": "payment_link.paid",
      "payload": {
        "payment_link": {"entity": {"id": "plink_QflcnnZqCekuvL", "status": "paid", "amount": 420000, "amount_paid": 420000}},
        "payment": {"entity": {"id": "pay_NEWpayment123", "amount": 420000, "status": "captured"}}
      }
    }`)

	var event WebhookEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if event.Event != EventPaymentLinkPaid {
		t.Errorf("Event = %q, want %q", event.Event, EventPaymentLinkPaid)
	}
	if got := event.Payload.PaymentLink.Entity.ID; got != "plink_QflcnnZqCekuvL" {
		t.Errorf("payment link id = %q, want plink_QflcnnZqCekuvL", got)
	}
	if got := event.Payload.Payment.Entity.ID; got != "pay_NEWpayment123" {
		t.Errorf("payment id = %q, want pay_NEWpayment123", got)
	}
}

func TestParsePaymentFailedNotes(t *testing.T) {
	raw := []byte(`{
      "event": "payment.failed",
      "payload": {"payment": {"entity": {
        "id": "pay_failed_1", "amount": 189000, "currency": "INR",
        "description": "Airtel Postpaid Bill",
        "notes": {"customer_name": "Preethi Nair"}
      }}}
    }`)

	var event WebhookEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	entity := event.Payload.Payment.Entity
	if entity.CustomerName() != "Preethi Nair" {
		t.Errorf("CustomerName() = %q, want Preethi Nair", entity.CustomerName())
	}
	if entity.Amount != 189000 {
		t.Errorf("Amount = %d, want 189000", entity.Amount)
	}
	if entity.Description != "Airtel Postpaid Bill" {
		t.Errorf("Description = %q, want Airtel Postpaid Bill", entity.Description)
	}
}

// TestCustomerNameFallback pins that the fallback is speakable. This value
// is substituted into the agent's system prompt, so a placeholder like
// "Unknown Customer" would be read aloud to a caller as their own name.
func TestCustomerNameFallback(t *testing.T) {
	var entity PaymentEntity
	if got := entity.CustomerName(); got != "the customer" {
		t.Errorf("CustomerName() with no notes = %q, want %q", got, "the customer")
	}

	entity.Notes = map[string]string{"name": "Vikram Rao"}
	if got := entity.CustomerName(); got != "Vikram Rao" {
		t.Errorf("CustomerName() from 'name' key = %q, want Vikram Rao", got)
	}

	entity.Notes = map[string]string{"customer_name": "Anita Desai", "name": "ignored"}
	if got := entity.CustomerName(); got != "Anita Desai" {
		t.Errorf("CustomerName() = %q, want customer_name to win over name", got)
	}
}

// TestDueDateUsesPaymentTimestamp asserts the due date comes from the event
// rather than the server clock. A webhook redelivered days after the
// payment failed must not claim the payment was due on the day it happened
// to be reprocessed.
func TestDueDateUsesPaymentTimestamp(t *testing.T) {
	entity := PaymentEntity{CreatedAt: 1786752000} // 2026-08-15 00:00:00 UTC
	if got := entity.DueDate(); got != "2026-08-15" {
		t.Errorf("DueDate() = %q, want 2026-08-15", got)
	}

	var noTimestamp PaymentEntity
	want := time.Now().UTC().Format("2006-01-02")
	if got := noTimestamp.DueDate(); got != want {
		t.Errorf("DueDate() with no timestamp = %q, want today (%s)", got, want)
	}
}
