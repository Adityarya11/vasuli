package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"vasuli/recovery-orchestrator/internal/campaign"
	"vasuli/recovery-orchestrator/internal/razorpay"
)

// maxWebhookBody caps how much of an inbound request is read. The signature
// is computed over the whole body, so an unbounded read would let anyone
// force this process to buffer arbitrary data before the request is even
// authenticated.
const maxWebhookBody = 1 << 20 // 1 MiB

// RazorpayWebhook handles POST /webhooks/razorpay.
//
// Response codes here are chosen for how Razorpay reacts to them, not just
// for correctness: any non-2xx makes Razorpay retry with backoff. A genuine
// signature failure is worth rejecting (400 — retrying will not help), but
// an event about a session this system does not own must be acknowledged
// with 200, or Razorpay will redeliver it indefinitely.
func (h *Handlers) RazorpayWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}

	signature := r.Header.Get("X-Razorpay-Signature")
	if !razorpay.VerifyWebhookSignature(body, signature, h.webhookSecret) {
		// The body is deliberately not logged. A request that fails
		// verification is unauthenticated by definition, and may be an
		// attacker's payload rather than Razorpay's.
		log.Printf("[Recovery] webhook rejected: signature verification failed")
		writeError(w, http.StatusBadRequest, "signature verification failed")
		return
	}

	var event razorpay.WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		writeError(w, http.StatusBadRequest, "malformed webhook payload")
		return
	}

	// A nil error means the event reached a terminal decision, including
	// benign ones like an unknown session. A non-nil error means processing
	// failed for a reason a retry could fix — a database write that did not
	// land, say. Those two must not share a status code: acknowledging an
	// infrastructure failure with 200 tells Razorpay never to resend it, and
	// the recovery is lost with no way to recover it.
	var handlerErr error
	switch event.Event {
	case razorpay.EventPaymentFailed:
		handlerErr = h.handlePaymentFailed(event)
	case razorpay.EventPaymentCaptured:
		handlerErr = h.handlePaymentCaptured(event)
	case razorpay.EventPaymentLinkPaid:
		handlerErr = h.handlePaymentLinkPaid(event)
	default:
		log.Printf("[Recovery] webhook event '%s' not actionable, acknowledged.", event.Event)
	}

	if handlerErr != nil {
		log.Printf("[Recovery] webhook '%s' processing failed, asking Razorpay to retry: %v", event.Event, handlerErr)
		writeError(w, http.StatusInternalServerError, "webhook processing failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handlers) handlePaymentFailed(event razorpay.WebhookEvent) error {
	payment := event.Payload.Payment.Entity

	sess, created, err := h.manager.HandlePaymentFailed(payment)
	if errors.Is(err, campaign.ErrNoActiveCampaign) {
		// Terminal, not a failure: retrying delivers the same result until
		// an operator loads a campaign, which is not something Razorpay can
		// wait for.
		log.Printf("[Recovery] payment.failed received (%s) but no active campaign found - ignoring.", payment.ID)
		return nil
	}
	if err != nil {
		return err
	}
	if !created {
		log.Printf("[Recovery] payment.failed for %s already queued as %s - duplicate delivery, ignoring.",
			payment.ID, sess.ID)
		return nil
	}

	log.Printf("[Recovery] payment.failed queued for recovery: %s (%s, %d paise) on campaign %s",
		sess.ID, sess.CustomerName, sess.OutstandingPaise, sess.CampaignID)
	return nil
}

func (h *Handlers) handlePaymentCaptured(event razorpay.WebhookEvent) error {
	paymentID := event.Payload.Payment.Entity.ID

	recovered, err := h.manager.HandlePaymentCaptured(paymentID)
	if errors.Is(err, campaign.ErrSessionNotFound) {
		// Webhooks are not scoped to this integration; events for payments
		// Vasuli never handled are routine and retrying cannot change that.
		log.Printf("[Recovery] payment.captured for unknown payment %s - acknowledged, no action.", paymentID)
		return nil
	}
	if err != nil {
		return err
	}
	if !recovered {
		log.Printf("[Recovery] payment.captured for %s was already recovered - duplicate delivery, ignoring.", paymentID)
		return nil
	}

	log.Printf("[Recovery] payment.captured confirmed recovery for payment %s", paymentID)
	return nil
}

func (h *Handlers) handlePaymentLinkPaid(event razorpay.WebhookEvent) error {
	linkID := event.Payload.PaymentLink.Entity.ID
	paymentID := event.Payload.Payment.Entity.ID

	recovered, err := h.manager.HandlePaymentLinkPaid(linkID, paymentID)
	if errors.Is(err, campaign.ErrSessionNotFound) {
		log.Printf("[Recovery] payment_link.paid for unknown link %s - acknowledged, no action.", linkID)
		return nil
	}
	if err != nil {
		return err
	}
	if !recovered {
		log.Printf("[Recovery] payment_link.paid for %s was already recovered - duplicate delivery, ignoring.", linkID)
		return nil
	}

	log.Printf("[Recovery] payment_link.paid confirmed recovery for link %s (payment %s)", linkID, paymentID)
	return nil
}
