package razorpay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultAPIBase = "https://api.razorpay.com"

	// requestTimeout bounds a payment-link call. This runs during call
	// teardown, after the customer has already hung up, so a slow Razorpay
	// response delays the audit record rather than the caller, but it must
	// still be bounded so teardown cannot hang indefinitely.
	requestTimeout = 10 * time.Second

	// linkValidity keeps generated links from lingering in the test-mode
	// dashboard indefinitely. Razorpay requires expire_by to be at least
	// 15 minutes out; 24 hours is comfortably clear of that floor while
	// still expiring on its own.
	linkValidity = 24 * time.Hour
)

// LiveClient talks to Razorpay's real API. It is selected over StubClient
// only when credentials are supplied at startup; see cmd/main.go.
type LiveClient struct {
	keyID      string
	keySecret  string
	apiBase    string
	httpClient *http.Client
}

func NewLiveClient(keyID, keySecret string) *LiveClient {
	return &LiveClient{
		keyID:      keyID,
		keySecret:  keySecret,
		apiBase:    defaultAPIBase,
		httpClient: &http.Client{Timeout: requestTimeout},
	}
}

// IsTestMode reports whether the configured key is a test-mode key.
// Vasuli must never transact against live credentials; startup refuses to
// run without this holding.
func (c *LiveClient) IsTestMode() bool {
	return strings.HasPrefix(c.keyID, "rzp_test_")
}

type paymentLinkRequest struct {
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	Description    string            `json:"description"`
	Customer       paymentLinkCustom `json:"customer"`
	Notify         paymentLinkNotify `json:"notify"`
	ReminderEnable bool              `json:"reminder_enable"`
	ExpireBy       int64             `json:"expire_by"`
}

type paymentLinkCustom struct {
	Name string `json:"name"`
}

// Notify is disabled on both channels deliberately. The demo generates real
// test-mode links against synthetic customer records; enabling delivery
// would attempt to contact people who do not exist.
type paymentLinkNotify struct {
	SMS   bool `json:"sms"`
	Email bool `json:"email"`
}

type paymentLinkResponse struct {
	ID       string `json:"id"`
	ShortURL string `json:"short_url"`
	Status   string `json:"status"`
}

type apiError struct {
	Error struct {
		Code        string `json:"code"`
		Description string `json:"description"`
	} `json:"error"`
}

func (c *LiveClient) CreatePaymentLink(ctx context.Context, req PaymentLinkRequest) (*PaymentLinkResponse, error) {
	body, err := json.Marshal(paymentLinkRequest{
		Amount:         req.AmountPaise,
		Currency:       "INR",
		Description:    req.Description,
		Customer:       paymentLinkCustom{Name: req.CustomerName},
		Notify:         paymentLinkNotify{SMS: false, Email: false},
		ReminderEnable: false,
		ExpireBy:       time.Now().Add(linkValidity).Unix(),
	})
	if err != nil {
		return nil, fmt.Errorf("razorpay: encode payment link request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.apiBase+"/v1/payment_links", bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("razorpay: build payment link request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(
		[]byte(c.keyID+":"+c.keySecret),
	))

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("razorpay: payment link request failed: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("razorpay: read payment link response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Razorpay returns a structured error body; surfacing its
		// description makes a bad key or malformed amount diagnosable from
		// the audit log rather than requiring a dashboard visit.
		var apiErr apiError
		if json.Unmarshal(payload, &apiErr) == nil && apiErr.Error.Description != "" {
			return nil, fmt.Errorf("razorpay: payment link rejected (%s): %s",
				apiErr.Error.Code, apiErr.Error.Description)
		}
		return nil, fmt.Errorf("razorpay: payment link returned %s: %s",
			resp.Status, strings.TrimSpace(string(payload)))
	}

	var parsed paymentLinkResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, fmt.Errorf("razorpay: decode payment link response: %w", err)
	}
	if parsed.ID == "" {
		return nil, fmt.Errorf("razorpay: payment link response carried no id")
	}

	return &PaymentLinkResponse{ID: parsed.ID, ShortURL: parsed.ShortURL}, nil
}
