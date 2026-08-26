// Package recovery is Orchestrator-Go's client for the Recovery
// Orchestrator's call-lifecycle API. It is the only place in this service
// that speaks HTTP — every other boundary is gRPC.
package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// requestTimeout bounds every call to the Recovery Orchestrator. A live
// audio session is blocked waiting on AssignSession, so an unreachable or
// slow orchestrator must fail fast and let the caller fall back to its
// static profile rather than stall the call while the caller is speaking.
const requestTimeout = 3 * time.Second

// ErrNoPendingSession reports an empty recovery queue. This is an expected
// operating condition, not a failure: the caller falls back to its static
// agent profile and the call proceeds normally.
var ErrNoPendingSession = errors.New("recovery: no pending session available")

// SessionContext is the per-customer call context returned by
// POST /api/v1/calls/assign. SystemPrompt arrives fully rendered — this
// service never performs template substitution.
type SessionContext struct {
	SessionID              string `json:"session_id"`
	CustomerName           string `json:"customer_name"`
	OutstandingAmountPaise int64  `json:"outstanding_amount_paise"`
	ProductName            string `json:"product_name"`
	DueDate                string `json:"due_date"`
	SystemPrompt           string `json:"system_prompt"`
}

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{Timeout: requestTimeout},
	}
}

// AssignSession binds callSessionID to the next eligible customer in the
// recovery queue and returns that customer's call context. It returns
// ErrNoPendingSession when the queue is empty.
func (c *Client) AssignSession(callSessionID string) (*SessionContext, error) {
	var out SessionContext

	status, err := c.doPost(
		"/api/v1/calls/assign",
		map[string]string{"call_session_id": callSessionID},
		&out,
	)
	if err != nil {
		if status == http.StatusNotFound {
			return nil, ErrNoPendingSession
		}
		return nil, err
	}

	// A blank prompt would silently degrade the agent to its fallback
	// persona mid-campaign, which is far harder to diagnose from a call
	// recording than an explicit error here.
	if out.SystemPrompt == "" {
		return nil, errors.New("recovery: assign response carried an empty system prompt")
	}

	return &out, nil
}

// EndSession records the outcome of a completed call. promiseDate is only
// meaningful for the PROMISED outcome and may be empty otherwise.
func (c *Client) EndSession(callSessionID, outcome, promiseDate string) error {
	path := "/api/v1/calls/" + url.PathEscape(callSessionID) + "/end"

	_, err := c.doPost(path, map[string]string{
		"outcome":      outcome,
		"promise_date": promiseDate,
	}, nil)

	return err
}

// doPost performs one JSON request/response cycle and reports the HTTP
// status alongside any error, so callers can distinguish an expected 404
// from a genuine transport failure. When out is non-nil a 2xx body is
// decoded into it.
func (c *Client) doPost(path string, payload, out any) (int, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("recovery: encode request: %w", err)
	}

	// The request context is derived from Background rather than from the
	// caller's gRPC stream context on purpose. EndSession runs precisely
	// while that stream is tearing down, and inheriting an already-cancelled
	// context would abort the HTTP call before it left the process — the
	// call outcome would never reach the audit trail.
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return 0, fmt.Errorf("recovery: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("recovery: POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf(
			"recovery: POST %s returned %s: %s",
			path, resp.Status, readSnippet(resp.Body),
		)
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("recovery: decode %s response: %w", path, err)
		}
	}

	return resp.StatusCode, nil
}

// readSnippet caps how much of an error body is pulled into a log line so a
// misconfigured endpoint returning an HTML error page cannot flood the logs.
func readSnippet(r io.Reader) string {
	buf, err := io.ReadAll(io.LimitReader(r, 512))
	if err != nil {
		return "<unreadable body>"
	}
	return strings.TrimSpace(string(buf))
}
