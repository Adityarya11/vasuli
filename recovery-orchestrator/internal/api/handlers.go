package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"vasuli/recovery-orchestrator/internal/campaign"
	"vasuli/recovery-orchestrator/internal/store"
)

type Handlers struct {
	manager *campaign.Manager
	db      *store.DB

	// webhookSecret is the shared secret inbound Razorpay webhooks are
	// signed with. Empty means no secret was configured, in which case
	// every webhook fails verification and is rejected — failing closed,
	// so a misconfigured deployment cannot be driven by unsigned requests.
	webhookSecret string
}

func NewHandlers(manager *campaign.Manager, db *store.DB, webhookSecret string) *Handlers {
	return &Handlers{manager: manager, db: db, webhookSecret: webhookSecret}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		if err := json.NewEncoder(w).Encode(body); err != nil {
			log.Printf("[API] failed to encode response: %v", err)
		}
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// --- POST /api/v1/campaigns ---

type createAccountRequest struct {
	CustomerName      string `json:"customer_name"`
	OutstandingPaise  int64  `json:"outstanding_paise"`
	ProductName       string `json:"product_name"`
	DueDate           string `json:"due_date"`
	RazorpayPaymentID string `json:"razorpay_payment_id"`
}

type createCampaignRequest struct {
	Name     string                  `json:"name"`
	Accounts []createAccountRequest  `json:"accounts"`
}

type createCampaignResponse struct {
	CampaignID string `json:"campaign_id"`
	Name       string `json:"name"`
	Total      int    `json:"total"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
}

func (h *Handlers) CreateCampaign(w http.ResponseWriter, r *http.Request) {
	var req createCampaignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || len(req.Accounts) == 0 {
		writeError(w, http.StatusBadRequest, "name and at least one account are required")
		return
	}

	accounts := make([]store.Account, len(req.Accounts))
	for i, a := range req.Accounts {
		accounts[i] = store.Account{
			CustomerName:      a.CustomerName,
			OutstandingPaise:  a.OutstandingPaise,
			ProductName:       a.ProductName,
			DueDate:           a.DueDate,
			RazorpayPaymentID: a.RazorpayPaymentID,
		}
	}

	c, err := h.manager.CreateCampaign(req.Name, accounts)
	if err != nil {
		log.Printf("[API] create campaign failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to create campaign")
		return
	}

	writeJSON(w, http.StatusCreated, createCampaignResponse{
		CampaignID: c.ID,
		Name:       c.Name,
		Total:      c.Total,
		Status:     c.Status,
		CreatedAt:  c.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// --- POST /api/v1/calls/assign ---

type assignRequest struct {
	CallSessionID string `json:"call_session_id"`
}

// assignResponse mirrors the SessionContext shape var-thon's recovery
// client unmarshals into (internal/recovery/client.go in var-thon).
type assignResponse struct {
	SessionID               string `json:"session_id"`
	CustomerName             string `json:"customer_name"`
	OutstandingAmountPaise   int64  `json:"outstanding_amount_paise"`
	ProductName              string `json:"product_name"`
	DueDate                  string `json:"due_date"`
	SystemPrompt             string `json:"system_prompt"`
}

func (h *Handlers) AssignCall(w http.ResponseWriter, r *http.Request) {
	var req assignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CallSessionID == "" {
		writeError(w, http.StatusBadRequest, "call_session_id is required")
		return
	}

	sess, err := h.manager.AssignSession(req.CallSessionID)
	if err != nil {
		log.Printf("[API] assign session failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to assign session")
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound, "no pending recovery session available")
		return
	}

	log.Printf("[Recovery] Session assigned: call_session_id=%s -> recovery_session_id=%s (%s)",
		req.CallSessionID, sess.ID, sess.CustomerName)

	writeJSON(w, http.StatusOK, assignResponse{
		SessionID:             sess.ID,
		CustomerName:           sess.CustomerName,
		OutstandingAmountPaise: sess.OutstandingPaise,
		ProductName:            sess.ProductName,
		DueDate:                sess.DueDate,
		SystemPrompt:           sess.SystemPrompt,
	})
}

// --- POST /api/v1/calls/{call_session_id}/end ---

type endCallRequest struct {
	Outcome     string `json:"outcome"`
	PromiseDate string `json:"promise_date"`
}

func (h *Handlers) EndCall(w http.ResponseWriter, r *http.Request) {
	callSessionID := r.PathValue("call_session_id")
	if callSessionID == "" {
		writeError(w, http.StatusBadRequest, "call_session_id is required")
		return
	}

	var req endCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	err := h.manager.EndSession(r.Context(), callSessionID, req.Outcome, req.PromiseDate)

	// An outcome already recorded is not a failure. var-thon reports every
	// session end exactly once, but a retry, a replay, or an operator
	// re-running the documented manual override would otherwise be logged
	// as an error for a request that behaved correctly by changing nothing.
	if errors.Is(err, campaign.ErrOutcomeAlreadyRecorded) {
		log.Printf("[Recovery] Call end ignored for session=%s: outcome already recorded.", callSessionID)
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "ok",
			"note":   "outcome already recorded, no change made",
		})
		return
	}
	if err != nil {
		log.Printf("[API] end session failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to end session")
		return
	}

	log.Printf("[Recovery] Call ended: session=%s, outcome=%s", callSessionID, req.Outcome)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- GET /api/v1/campaigns/{id}/metrics ---

func (h *Handlers) CampaignMetrics(w http.ResponseWriter, r *http.Request) {
	campaignID := r.PathValue("id")

	c, err := h.db.GetCampaign(campaignID)
	if err != nil {
		log.Printf("[API] get campaign failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load campaign")
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "campaign not found")
		return
	}

	m, err := h.manager.Metrics(campaignID)
	if err != nil {
		log.Printf("[API] campaign metrics failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to compute metrics")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"campaign_id":   c.ID,
		"campaign_name": c.Name,
		"total_accounts": m.TotalAccounts,
		"contacted":      m.Contacted,
		"breakdown": map[string]any{
			"recovered":              m.Recovered,
			"recovered_amount_paise": m.RecoveredAmountPaise,
			"promised":               m.Promised,
			"promised_amount_paise":  m.PromisedAmountPaise,
			"refused":                m.Refused,
			"unclear":                m.Unclear,
		},
		"pending":                     m.Pending,
		"stopped_max_attempts":        m.StoppedMaxAttempts,
		"payment_links_sent":          m.PaymentLinksSent,
		"razorpay_captured_confirmed": m.RazorpayCaptured,
	})
}

// --- GET /health ---

func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "db": "connected"})
}
