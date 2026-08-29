package razorpay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Webhook event names Vasuli acts on.
const (
	EventPaymentFailed   = "payment.failed"
	EventPaymentCaptured = "payment.captured"
	EventPaymentLinkPaid = "payment_link.paid"
)

// VerifyWebhookSignature reports whether signature is a valid HMAC-SHA256
// of body under secret.
//
// body must be the exact bytes received on the wire. Decoding the JSON and
// re-encoding it changes key order and whitespace, which changes the hash —
// signature verification has to happen before parsing, not after.
//
// hmac.Equal is used rather than == so comparison time does not depend on
// how many leading bytes match, which would otherwise leak the expected
// signature to an attacker one byte at a time.
func VerifyWebhookSignature(body []byte, signature, secret string) bool {
	if signature == "" || secret == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

// WebhookEvent is the subset of Razorpay's webhook envelope Vasuli reads.
// Razorpay sends a payload keyed by entity type, and which entities are
// present varies by event: payment.* carries `payment`, payment_link.paid
// carries `payment_link`, `order`, and `payment` together.
type WebhookEvent struct {
	Event   string `json:"event"`
	Payload struct {
		Payment struct {
			Entity PaymentEntity `json:"entity"`
		} `json:"payment"`
		PaymentLink struct {
			Entity PaymentLinkEntity `json:"entity"`
		} `json:"payment_link"`
	} `json:"payload"`
}

type PaymentEntity struct {
	ID          string            `json:"id"`
	Amount      int64             `json:"amount"`
	Currency    string            `json:"currency"`
	Description string            `json:"description"`
	Email       string            `json:"email"`
	Contact     string            `json:"contact"`
	Notes       map[string]string `json:"notes"`

	// CreatedAt is when Razorpay recorded the payment attempt, as a Unix
	// timestamp. Used as the due date for webhook-created sessions: the
	// server's own clock would be wrong for a webhook redelivered hours or
	// days after the payment actually failed.
	CreatedAt int64 `json:"created_at"`
}

type PaymentLinkEntity struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Amount     int64  `json:"amount"`
	AmountPaid int64  `json:"amount_paid"`
}

// CustomerName pulls the customer's name out of the payment's notes, which
// is where Razorpay surfaces merchant-supplied metadata.
//
// The fallback is deliberately a phrase that can be spoken: this value is
// substituted into the agent's system prompt, so a placeholder like
// "Unknown Customer" would be read aloud to a real caller as their name.
// "the customer" degrades into a natural sentence instead.
func (p PaymentEntity) CustomerName() string {
	for _, key := range []string{"customer_name", "name"} {
		if v, ok := p.Notes[key]; ok && v != "" {
			return v
		}
	}
	return "the customer"
}

// DueDate reports the date this payment was attempted, formatted for the
// system prompt. payment.failed carries no due date of its own, and the
// attempt date is the closest honest proxy — the amount was payable then
// and was not paid. Falls back to today only when the event omits a
// timestamp entirely.
func (p PaymentEntity) DueDate() string {
	if p.CreatedAt > 0 {
		return time.Unix(p.CreatedAt, 0).UTC().Format("2006-01-02")
	}
	return time.Now().UTC().Format("2006-01-02")
}
