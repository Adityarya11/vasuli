// Package razorpay isolates payment-link creation behind a small interface.
//
// This interface earns its place rather than being speculative abstraction:
// there are exactly two concrete implementations on the roadmap (Stub now,
// Live once test-mode credentials exist in M6), campaign.Manager must
// compile and be testable today without real credentials, and the seam
// between them is exactly one method wide. That is a narrow, known need —
// not a guess at future requirements.
package razorpay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

type PaymentLinkRequest struct {
	AmountPaise  int64
	Description  string
	CustomerName string
}

type PaymentLinkResponse struct {
	ID       string
	ShortURL string
}

type Client interface {
	CreatePaymentLink(ctx context.Context, req PaymentLinkRequest) (*PaymentLinkResponse, error)
}

// StubClient stands in for the real Razorpay API until test-mode
// credentials are available. It performs no network call and always
// succeeds, so campaign.Manager's AGREED-outcome path can be built and
// exercised end-to-end before M6 wires in real credentials.
type StubClient struct{}

func NewStubClient() *StubClient {
	return &StubClient{}
}

func (c *StubClient) CreatePaymentLink(ctx context.Context, req PaymentLinkRequest) (*PaymentLinkResponse, error) {
	id, err := randomID("plink_stub_")
	if err != nil {
		return nil, fmt.Errorf("razorpay: stub id generation: %w", err)
	}
	return &PaymentLinkResponse{
		ID:       id,
		ShortURL: "https://rzp.io/i/" + id[len(id)-8:],
	}, nil
}

func randomID(prefix string) (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf), nil
}
