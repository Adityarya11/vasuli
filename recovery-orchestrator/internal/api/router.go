package api

import "net/http"

// NewRouter builds the HTTP routing table using the standard library's
// method + path pattern matching (Go 1.22+). No third-party router — four
// endpoints and a health check don't justify a dependency.
func NewRouter(h *Handlers) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/campaigns", h.CreateCampaign)
	mux.HandleFunc("GET /api/v1/campaigns/{id}/metrics", h.CampaignMetrics)
	mux.HandleFunc("POST /api/v1/calls/assign", h.AssignCall)
	mux.HandleFunc("POST /api/v1/calls/{call_session_id}/end", h.EndCall)
	mux.HandleFunc("POST /webhooks/razorpay", h.RazorpayWebhook)
	mux.HandleFunc("GET /health", h.Health)

	return mux
}
