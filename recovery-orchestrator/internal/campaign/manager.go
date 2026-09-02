// Package campaign holds the business rules recovery-orchestrator exists
// for: rendering per-customer system prompts, applying stopping rules when
// assigning the next call, and recording what happened when a call ends.
// It knows nothing about HTTP or SQL directly, both are injected.
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
	"time"

	"vasuli/recovery-orchestrator/internal/razorpay"
	"vasuli/recovery-orchestrator/internal/store"
)

// ErrNoActiveCampaign reports an inbound payment.failed with no campaign to
// attach it to. Acknowledged and dropped rather than rejected, see
// HandlePaymentFailed.
var ErrNoActiveCampaign = errors.New("campaign: no active campaign to attach this payment to")

// systemPromptTemplate is rendered per customer at campaign-load time and
// reaches var-thon as a finished string.
//
// The constraints here are shaped by how the output is consumed. Every word
// is synthesized and played down a phone line, and the caller's microphone
// is gated at the edge for the whole of that playback, so a long reply does
// not merely ramble, it locks the customer out of the conversation for as
// many seconds as it takes to speak. Hence the per-turn word limit and the
// one-question rule, which replaced a numbered objective list that invited
// the model to complete every step in a single breath.
const systemPromptTemplate = `You are Priya. You work for a Razorpay merchant and you are on a live
phone call right now with that merchant's customer.

You are calling {{.CustomerName}}. They owe {{.AmountFormatted}} rupees for
{{.ProductName}}, which was due on {{.DueDate}} and is still unpaid.

How to speak:
- Everything you write is spoken aloud. Write only words meant to be heard.
- Keep each reply under 40 words, and ask at most one question in it.
- Say one thing, then stop and let {{.CustomerName}} answer.
- Never write parentheses, asides, or notes about your own reasoning.
- Speak natural, professional Indian English.

How the call goes, one step per turn:
1. Greet {{.CustomerName}} and check you are speaking to them.
2. Once confirmed, tell them the amount, what it is for, and that it is overdue.
3. Ask whether they can pay now.
4. If they cannot, ask for a date they can pay by.
5. Thank them and close.

Rules:
- Never be aggressive, threatening, or pressuring.
- Do not raise the payment again more than three times if they decline.
- If they agree to pay, say exactly "I will arrange a payment link to be
  sent to your registered number shortly."
- If they give a date, repeat that date back to confirm it.
- If they refuse, say "I understand, thank you for your time." and stop asking.
- Say amounts as "4,200 rupees", never with a currency symbol or "Rs."
- State the amount once. Do not repeat it in every reply.
- Do not thank them and close until the call is actually ending.
- {{.DueDate}} is already past. Never offer it as a date they could pay on.
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
		DueDate:         formatDueDate(acc.DueDate),
	}
	if err := m.promptTemplate.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("campaign: render system prompt: %w", err)
	}
	return buf.String(), nil
}

// AssignSession pops the next eligible pending session and binds it to
// callSessionID. Returns nil, nil when the queue is empty, this is the
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

// endSessionAllowedFrom is the set of statuses an outcome may be recorded
// from. Everything absent from it is either already settled or terminal.
//
// This exists because EndSession has side effects that must not repeat:
// recording AGREED twice would create a second payment link and duplicate
// the audit trail. More importantly, without it a session already marked
// refused could be dragged back into link_sent by a stray or replayed
// call. Re-engaging a customer who declined, which is exactly what the
// stopping rules exist to prevent.
//
// unclear is included deliberately: it means the classifier reached no
// verdict, and correcting that by hand is a documented operator action
// (see docs/demo.md), not a duplicate.
// followUpCooldown is how long Vasuli waits before calling a customer back
// when the last call did not settle anything.
//
// It is 24 hours because that is exactly how long a generated payment link
// lives (see razorpay.linkValidity). By the time the cooldown expires the
// old link is dead at Razorpay's end, so the follow-up call is not chasing
// someone who already has a working link, it exists to issue a new one.
// Changing one of these two numbers without the other breaks that property.
const followUpCooldown = 24 * time.Hour

var endSessionAllowedStatuses = []string{store.StatusActive, store.StatusUnclear}

var endSessionAllowedFrom = map[string]bool{
	store.StatusActive:  true,
	store.StatusUnclear: true,
}

// ErrOutcomeAlreadyRecorded reports an EndSession against a session whose
// outcome is already settled or terminal. Benign rather than exceptional:
// the caller acknowledges it and makes no change.
var ErrOutcomeAlreadyRecorded = errors.New("campaign: session outcome is already recorded")

// EndSession records the outcome of a completed call and applies the
// consequence for that outcome, payment link creation for AGREED, a
// promise date for PROMISED, a permanent stop for REFUSED, and either a
// retry-eligible or exhausted state for UNCLEAR.
//
// Idempotent: a repeat call for a session that already has an outcome
// makes no change and creates no second payment link.
func (m *Manager) EndSession(ctx context.Context, callSessionID, outcome, promiseDate string) error {
	sess, err := m.db.GetSessionByCallSessionID(callSessionID)
	if err != nil {
		return err
	}
	if sess == nil {
		return fmt.Errorf("campaign: no session bound to call_session_id %q", callSessionID)
	}

	if !endSessionAllowedFrom[sess.Status] {
		return ErrOutcomeAlreadyRecorded
	}

	// The payment link is created before anything is written, so a provider
	// failure cannot leave a session claiming a link that does not exist.
	// The reverse ordering is the known gap: a link created here whose
	// response never arrives is orphaned, since nothing records it. See
	// docs/build-log.md.
	var link *razorpay.PaymentLinkResponse
	if outcome == OutcomeAgreed {
		link, err = m.razorpay.CreatePaymentLink(ctx, razorpay.PaymentLinkRequest{
			AmountPaise:  sess.OutstandingPaise,
			Description:  "Payment recovery, " + sess.ProductName,
			CustomerName: sess.CustomerName,
		})
		if err != nil {
			_ = m.db.InsertAuditLog(sess.ID, "payment_link_failed", map[string]any{"error": err.Error()})
			return fmt.Errorf("campaign: create payment link: %w", err)
		}
	}

	write := m.buildOutcomeWrite(sess, outcome, promiseDate, link)

	applied, err := m.db.RecordOutcome(write, endSessionAllowedStatuses)
	if err != nil {
		return err
	}
	if !applied {
		// The status changed between the read above and the transaction,
		// meaning another request recorded an outcome first.
		return ErrOutcomeAlreadyRecorded
	}
	return nil
}

// buildOutcomeWrite maps an outcome to the status and audit events it
// produces. Assembling the whole change first is what lets it be applied
// atomically. The status and the audit rows explaining it commit together
// or not at all.
func (m *Manager) buildOutcomeWrite(
	sess *store.RecoverySession,
	outcome, promiseDate string,
	link *razorpay.PaymentLinkResponse,
) store.OutcomeWrite {
	write := store.OutcomeWrite{
		SessionID: sess.ID,
		Events: []store.AuditEvent{
			{Type: "call_ended", Data: map[string]any{"outcome": outcome}},
			{Type: "outcome_classified", Data: map[string]any{"outcome": outcome}},
		},
	}

	followUp := time.Now().UTC().Add(followUpCooldown)

	switch outcome {
	case OutcomeAgreed:
		// A link was sent but nothing is paid yet. The follow-up lands once
		// the link has expired, so calling back is not nagging, the
		// customer genuinely needs a fresh one. Payment confirmation clears
		// this timer; see store.MarkRecovered.
		write.Status = store.StatusLinkSent
		write.RazorpayLinkID = link.ID
		write.NextEligibleAt = &followUp
		write.Events = append(write.Events, store.AuditEvent{
			Type: "payment_link_sent",
			Data: map[string]any{
				"razorpay_link_id": link.ID,
				"short_url":        link.ShortURL,
				"amount_paise":     sess.OutstandingPaise,
			},
		})

	case OutcomePromised:
		write.Status = store.StatusPromised
		write.PromiseDate = promiseDate
		write.NextEligibleAt = promisedFollowUp(promiseDate, followUp)
		write.Events = append(write.Events, store.AuditEvent{
			Type: "promise_logged",
			Data: map[string]any{
				"promise_date":     promiseDate,
				"next_eligible_at": write.NextEligibleAt,
			},
		})

	case OutcomeRefused:
		// Not a dead end: a customer who disputes the debt has moved past
		// what an automated agent should handle, and is handed to a human.
		// Vasuli stops contacting them permanently, hence a nil follow-up.
		write.Status = store.StatusRefused
		write.NextEligibleAt = nil
		write.Events = append(write.Events, store.AuditEvent{
			Type: "stopped_refused",
			Data: map[string]any{"escalated_to": "human agent"},
		})

	default:
		// UNCLEAR, and anything unrecognised. A session that has used up its
		// contact budget is stopped permanently rather than left eligible,
		// per the stopping rules in docs/architecture.md.
		if sess.ContactAttempts >= sess.MaxContactAttempts {
			write.Status = store.StatusFailed
			write.NextEligibleAt = nil
			write.Events = append(write.Events, store.AuditEvent{Type: "stopped_max_attempts"})
		} else {
			write.Status = store.StatusUnclear
			write.NextEligibleAt = &followUp
		}
	}

	return write
}

// promisedFollowUp honours a date the customer named, falling back to the
// standard cooldown.
//
// The fallback is the common case today, not the exception: outcome
// classification returns a single word and never extracts a date, so
// promiseDate is only populated when an operator supplies one by hand.
// A promise whose date we could not capture still deserves a follow-up
// rather than silence.
func promisedFollowUp(promiseDate string, fallback time.Time) *time.Time {
	parsed, err := time.Parse("2006-01-02", promiseDate)
	if err != nil {
		return &fallback
	}

	// A date already in the past means the promise is due now.
	if parsed.Before(fallback) {
		now := time.Now().UTC()
		return &now
	}
	return &parsed
}

// ErrSessionNotFound reports a webhook that refers to a session this system
// has no record of. Webhook delivery is not scoped to one merchant
// integration, so unrelated events are an ordinary occurrence, not a
// failure. Callers acknowledge them rather than returning an error status
// that would make Razorpay retry forever.
var ErrSessionNotFound = errors.New("campaign: no recovery session matches this event")

// HandlePaymentCaptured confirms recovery from a payment.captured event,
// matching on the original failed payment's id.
//
// Note this only resolves payments against that original id, the demo's
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
	if sess.Status == store.StatusRecovered {
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

// formatDueDate turns a stored ISO date into something a speech synthesizer
// reads correctly. Piper pronounces "2026-08-15" as digits and dashes;
// "15 August 2026" is pronounced as a date. Input that is not an ISO date
// passes through untouched, the field is caller-supplied, and a malformed
// date should not fail campaign creation.
func formatDueDate(due string) string {
	parsed, err := time.Parse("2006-01-02", due)
	if err != nil {
		return due
	}
	return parsed.Format("2 January 2006")
}

// formatRupees converts paise to a comma-grouped rupee string (e.g.
// 420000 -> "4,200"). Grouping is plain 3-digit (not Indian lakh/crore
// style). The demo's amount range (₹2,000-₹25,000) never crosses the
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
