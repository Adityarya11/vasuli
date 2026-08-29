// Package campaign holds the business rules recovery-orchestrator exists
// for: rendering per-customer system prompts, applying stopping rules when
// assigning the next call, and recording what happened when a call ends.
// It knows nothing about HTTP or SQL directly — both are injected.
package campaign

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"vasuli/recovery-orchestrator/internal/razorpay"
	"vasuli/recovery-orchestrator/internal/store"
)

// ErrNoActiveCampaign reports an inbound payment.failed with no campaign to
// attach it to. Acknowledged and dropped rather than rejected — see
// HandlePaymentFailed.
var ErrNoActiveCampaign = errors.New("campaign: no active campaign to attach this payment to")

const systemPromptTemplate = `You are Priya, a professional payment recovery specialist calling on behalf
of a Razorpay merchant.

You are calling {{.CustomerName}} regarding an outstanding payment of
₹{{.AmountFormatted}} for {{.ProductName}} that was due on {{.DueDate}}.

Your objectives in order:
1. Greet {{.CustomerName}} and confirm you are speaking with them.
2. State the outstanding amount clearly: ₹{{.AmountFormatted}} for {{.ProductName}}.
3. Offer to help them pay now — you will arrange a payment link.
4. If they cannot pay now, get a specific date and confirm it back to them.
5. If they decline, acknowledge respectfully and close.

Rules:
- Never be aggressive, threatening, or pressuring.
- Do not mention the payment more than three times if refused.
- If they agree: say exactly "I will arrange a payment link to be sent
  to your registered number shortly."
- If they give a date: repeat it back — "So you will pay by [date],
  I have noted that down."
- If they refuse: say "I understand, thank you for your time."
- Keep the call under 3 minutes.
- Speak professional Indian English.
`

type promptData struct {
	CustomerName    string
	AmountFormatted string
	ProductName     string
	DueDate         string
}

type Manager struct {
	db             *store.DB
	razorpay       razorpay.Client
	promptTemplate *template.Template
}

func NewManager(db *store.DB, rzp razorpay.Client) (*Manager, error) {
	tmpl, err := template.New("system_prompt").Parse(systemPromptTemplate)
	if err != nil {
		return nil, fmt.Errorf("campaign: parse system prompt template: %w", err)
	}
	return &Manager{db: db, razorpay: rzp, promptTemplate: tmpl}, nil
}

func (m *Manager) CreateCampaign(name string, accounts []store.Account) (*store.Campaign, error) {
	if len(accounts) == 0 {
		return nil, fmt.Errorf("campaign: cannot create campaign with zero accounts")
	}

	campaignID, err := randomID("camp_")
	if err != nil {
		return nil, err
	}

	if err := m.db.InsertCampaign(campaignID, name, len(accounts)); err != nil {
		return nil, err
	}

	for _, acc := range accounts {
		sessionID, err := randomID("rec_")
		if err != nil {
			return nil, err
		}

		prompt, err := m.renderSystemPrompt(acc)
		if err != nil {
			return nil, err
		}

		if err := m.db.InsertRecoverySession(sessionID, campaignID, prompt, acc); err != nil {
			return nil, err
		}

		if err := m.db.InsertAuditLog(sessionID, "session_created", map[string]any{
			"customer_name": acc.CustomerName,
		}); err != nil {
			return nil, err
		}
	}

	return m.db.GetCampaign(campaignID)
}

func (m *Manager) renderSystemPrompt(acc store.Account) (string, error) {
	var buf strings.Builder
	data := promptData{
		CustomerName:    acc.CustomerName,
		AmountFormatted: formatRupees(acc.OutstandingPaise),
		ProductName:     acc.ProductName,
		DueDate:         acc.DueDate,
	}
	if err := m.promptTemplate.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("campaign: render system prompt: %w", err)
	}
	return buf.String(), nil
}

// AssignSession pops the next eligible pending session and binds it to
// callSessionID. Returns nil, nil when the queue is empty — this is the
// expected "no work available" case, not an error; var-thon falls back to
// its static profile when it sees this.
func (m *Manager) AssignSession(callSessionID string) (*store.RecoverySession, error) {
	sess, err := m.db.AssignNextPending(callSessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}

	if err := m.db.InsertAuditLog(sess.ID, "session_assigned", map[string]any{
		"call_session_id": callSessionID,
	}); err != nil {
		return nil, err
	}

	return sess, nil
}

const (
	OutcomeAgreed  = "AGREED"
	OutcomePromised = "PROMISED"
	OutcomeRefused = "REFUSED"
	OutcomeUnclear = "UNCLEAR"
)

// EndSession records the outcome of a completed call and applies the
// consequence for that outcome — payment link creation for AGREED, a
// promise date for PROMISED, a permanent stop for REFUSED, and either a
// retry-eligible or exhausted state for UNCLEAR.
func (m *Manager) EndSession(ctx context.Context, callSessionID, outcome, promiseDate string) error {
	sess, err := m.db.GetSessionByCallSessionID(callSessionID)
	if err != nil {
		return err
	}
	if sess == nil {
		return fmt.Errorf("campaign: no session bound to call_session_id %q", callSessionID)
	}

	if err := m.db.SetCallEnded(sess.ID); err != nil {
		return err
	}
	if err := m.db.InsertAuditLog(sess.ID, "call_ended", map[string]any{"outcome": outcome}); err != nil {
		return err
	}
	if err := m.db.InsertAuditLog(sess.ID, "outcome_classified", map[string]any{"outcome": outcome}); err != nil {
		return err
	}

	switch outcome {
	case OutcomeAgreed:
		return m.handleAgreed(ctx, sess)
	case OutcomePromised:
		return m.handlePromised(sess, promiseDate)
	case OutcomeRefused:
		if err := m.db.MarkRefused(sess.ID); err != nil {
			return err
		}
		return m.db.InsertAuditLog(sess.ID, "stopped_refused", nil)
	default:
		return m.handleUnclear(sess)
	}
}

func (m *Manager) handleAgreed(ctx context.Context, sess *store.RecoverySession) error {
	resp, err := m.razorpay.CreatePaymentLink(ctx, razorpay.PaymentLinkRequest{
		AmountPaise:  sess.OutstandingPaise,
		Description:  "Payment recovery — " + sess.ProductName,
		CustomerName: sess.CustomerName,
	})
	if err != nil {
		_ = m.db.InsertAuditLog(sess.ID, "payment_link_failed", map[string]any{"error": err.Error()})
		return fmt.Errorf("campaign: create payment link: %w", err)
	}

	if err := m.db.MarkLinkSent(sess.ID, resp.ID); err != nil {
		return err
	}

	return m.db.InsertAuditLog(sess.ID, "payment_link_sent", map[string]any{
		"razorpay_link_id": resp.ID,
		"short_url":        resp.ShortURL,
		"amount_paise":     sess.OutstandingPaise,
	})
}

func (m *Manager) handlePromised(sess *store.RecoverySession, promiseDate string) error {
	if err := m.db.MarkPromised(sess.ID, promiseDate); err != nil {
		return err
	}
	return m.db.InsertAuditLog(sess.ID, "promise_logged", map[string]any{"promise_date": promiseDate})
}

// handleUnclear applies the max-attempts stopping rule: a session that has
// exhausted its retry budget is permanently failed rather than left
// eligible for re-assignment, matching the stopping rules documented in
// docs/architecture.md.
func (m *Manager) handleUnclear(sess *store.RecoverySession) error {
	if sess.ContactAttempts >= sess.MaxContactAttempts {
		if err := m.db.MarkFailedMaxAttempts(sess.ID); err != nil {
			return err
		}
		return m.db.InsertAuditLog(sess.ID, "stopped_max_attempts", nil)
	}
	return m.db.MarkUnclear(sess.ID)
}

// ErrSessionNotFound reports a webhook that refers to a session this system
// has no record of. Webhook delivery is not scoped to one merchant
// integration, so unrelated events are an ordinary occurrence, not a
// failure — callers acknowledge them rather than returning an error status
// that would make Razorpay retry forever.
var ErrSessionNotFound = errors.New("campaign: no recovery session matches this event")

// HandlePaymentCaptured confirms recovery from a payment.captured event,
// matching on the original failed payment's id.
//
// Note this only resolves payments against that original id — the demo's
// simulated capture. A customer paying a link Vasuli generated produces a
// different payment id entirely; that path is HandlePaymentLinkPaid.
// Idempotent: a repeat capture for an already-recovered session is a no-op.
// The bool reports whether this call actually changed the session's state,
// so callers can distinguish a real recovery from a redelivered duplicate
// instead of logging both identically.
func (m *Manager) HandlePaymentCaptured(razorpayPaymentID string) (bool, error) {
	sess, err := m.db.GetSessionByRazorpayPaymentID(razorpayPaymentID)
	if err != nil {
		return false, err
	}
	if sess == nil {
		return false, ErrSessionNotFound
	}

	return m.markRecovered(sess, map[string]any{
		"razorpay_payment_id": razorpayPaymentID,
	})
}

// HandlePaymentLinkPaid confirms recovery from a payment_link.paid event,
// matching on the generated link's id. This is the path a real customer
// payment takes.
func (m *Manager) HandlePaymentLinkPaid(razorpayLinkID, razorpayPaymentID string) (bool, error) {
	sess, err := m.db.GetSessionByRazorpayLinkID(razorpayLinkID)
	if err != nil {
		return false, err
	}
	if sess == nil {
		return false, ErrSessionNotFound
	}

	return m.markRecovered(sess, map[string]any{
		"razorpay_link_id":    razorpayLinkID,
		"razorpay_payment_id": razorpayPaymentID,
	})
}

// markRecovered reports false when the session was already recovered, so a
// redelivered webhook neither writes a second audit row nor reads as a
// second recovery in the logs.
func (m *Manager) markRecovered(sess *store.RecoverySession, auditData map[string]any) (bool, error) {
	if sess.Status == "recovered" {
		return false, nil
	}

	if err := m.db.MarkRecovered(sess.ID); err != nil {
		return false, err
	}
	if err := m.db.InsertAuditLog(sess.ID, "payment_captured", auditData); err != nil {
		return false, err
	}
	return true, nil
}

// HandlePaymentFailed creates a recovery session from an inbound
// payment.failed event, attaching it to the most recent active campaign.
//
// Returns ErrNoActiveCampaign when nothing is active. Auto-creating a
// campaign to hold orphaned sessions was rejected deliberately: it makes
// campaign ownership ambiguous, and that ambiguity surfaces later as
// metrics that do not reconcile against any batch an operator loaded.
// The bool reports whether a new session was created, distinguishing a
// first delivery from a redelivery of the same event.
func (m *Manager) HandlePaymentFailed(payment razorpay.PaymentEntity) (*store.RecoverySession, bool, error) {
	campaignRow, err := m.db.GetMostRecentActiveCampaign()
	if err != nil {
		return nil, false, err
	}
	if campaignRow == nil {
		return nil, false, ErrNoActiveCampaign
	}

	// Webhook delivery is at-least-once. Without this check a redelivered
	// payment.failed would queue the same customer for a second call.
	if payment.ID != "" {
		existing, err := m.db.GetSessionByRazorpayPaymentID(payment.ID)
		if err != nil {
			return nil, false, err
		}
		if existing != nil {
			return existing, false, nil
		}
	}

	acc := store.Account{
		CustomerName:      payment.CustomerName(),
		OutstandingPaise:  payment.Amount,
		ProductName:       payment.Description,
		DueDate:           payment.DueDate(),
		RazorpayPaymentID: payment.ID,
	}
	if acc.ProductName == "" {
		acc.ProductName = "outstanding payment"
	}

	sessionID, err := randomID("rec_")
	if err != nil {
		return nil, false, err
	}

	prompt, err := m.renderSystemPrompt(acc)
	if err != nil {
		return nil, false, err
	}

	if err := m.db.InsertRecoverySession(sessionID, campaignRow.ID, prompt, acc); err != nil {
		return nil, false, err
	}
	if err := m.db.IncrementCampaignTotal(campaignRow.ID); err != nil {
		return nil, false, err
	}
	if err := m.db.InsertAuditLog(sessionID, "webhook_received", map[string]any{
		"razorpay_payment_id": payment.ID,
		"customer_name":       acc.CustomerName,
		"amount_paise":        acc.OutstandingPaise,
	}); err != nil {
		return nil, false, err
	}
	if err := m.db.InsertAuditLog(sessionID, "session_created", map[string]any{
		"customer_name": acc.CustomerName,
		"source":        "payment.failed webhook",
	}); err != nil {
		return nil, false, err
	}

	created, err := m.db.GetSessionByID(sessionID)
	return created, true, err
}

func (m *Manager) Metrics(campaignID string) (*store.Metrics, error) {
	return m.db.CampaignMetrics(campaignID)
}

func randomID(prefix string) (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("campaign: id generation: %w", err)
	}
	return prefix + hex.EncodeToString(buf), nil
}

// formatRupees converts paise to a comma-grouped rupee string (e.g.
// 420000 -> "4,200"). Grouping is plain 3-digit (not Indian lakh/crore
// style) — the demo's amount range (₹2,000-₹25,000) never crosses the
// point where the two styles diverge, so the simpler implementation is
// correct for every value this system actually produces.
func formatRupees(paise int64) string {
	rupees := paise / 100
	s := strconv.FormatInt(rupees, 10)

	n := len(s)
	if n <= 3 {
		return s
	}

	var b strings.Builder
	rem := n % 3
	if rem > 0 {
		b.WriteString(s[:rem])
		b.WriteByte(',')
	}
	for i := rem; i < n; i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < n {
			b.WriteByte(',')
		}
	}
	return b.String()
}
