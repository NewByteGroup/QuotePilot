package inbox

import (
	"errors"
	"fmt"
	"net/http"

	svix "github.com/svix/svix-webhooks/go"
)

// SignatureVerifier authenticates a webhook payload. It wraps Svix so the
// handler depends on a project-owned interface (AGENTS.md: third parties
// behind thin interfaces).
type SignatureVerifier interface {
	// Verify returns nil when the Svix headers authenticate payload.
	Verify(payload []byte, headers http.Header) error
}

// SvixVerifier verifies Resend inbound webhooks, which Svix signs
// (svix-id, svix-timestamp, svix-signature headers).
type SvixVerifier struct {
	wh *svix.Webhook
}

// NewSvixVerifier builds a verifier for a whsec_-prefixed endpoint secret.
//
//	v, err := inbox.NewSvixVerifier(os.Getenv("RESEND_WEBHOOK_SECRET"))
func NewSvixVerifier(secret string) (*SvixVerifier, error) {
	if secret == "" {
		return nil, errors.New("inbox: webhook secret must not be empty")
	}
	wh, err := svix.NewWebhook(secret)
	if err != nil {
		return nil, fmt.Errorf("inbox: invalid webhook secret (expected whsec_...): %w", err)
	}
	return &SvixVerifier{wh: wh}, nil
}

// Verify implements SignatureVerifier.
func (v *SvixVerifier) Verify(payload []byte, headers http.Header) error {
	if err := v.wh.Verify(payload, headers); err != nil {
		return fmt.Errorf("inbox: svix signature verification failed: %w", err)
	}
	return nil
}
