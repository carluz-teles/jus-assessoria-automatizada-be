package notifications

import (
	"context"

	"github.com/resend/resend-go/v2"

	"github.com/jusassessoria/platform/lib/apperr"
)

// resend.go is the ONLY place the Resend SDK is imported (the domain and EmailChannel
// depend on the ResendClient port, never the SDK) — the same "porta p/ provedor
// externo" shape as lib/storage over the AWS SDK.

// ResendClient is the narrow port over the e-mail provider: send one message, get
// back the provider's id. Keeping it this small lets EmailChannel be unit-tested with
// a stub and keeps the SDK out of the domain.
type ResendClient interface {
	Send(ctx context.Context, from, to, subject, html string) (providerMessageID string, err error)
}

// resendClient is the concrete adapter over resend-go.
type resendClient struct {
	api *resend.Client
}

var _ ResendClient = (*resendClient)(nil)

// NewResendClient builds the client from the API key. A missing key is a caller
// mistake (config), so it returns an apperr.Invalid — construction touches no
// network — which the worker boot turns into a fast fail (não sobe pela metade).
func NewResendClient(apiKey string) (ResendClient, error) {
	if apiKey == "" {
		return nil, apperr.NewInvalid("resend: api key is required")
	}
	return &resendClient{api: resend.NewClient(apiKey)}, nil
}

// Send delivers one HTML e-mail via Resend and returns its message id. A provider
// failure is an external-dependency fault (KindUnavailable), not a bug in our code —
// the delivery is recorded FAILED and the caller may act on it later.
func (c *resendClient) Send(ctx context.Context, from, to, subject, html string) (string, error) {
	sent, err := c.api.Emails.SendWithContext(ctx, &resend.SendEmailRequest{
		From:    from,
		To:      []string{to},
		Subject: subject,
		Html:    html,
	})
	if err != nil {
		return "", apperr.NewUnavailable("resend: send email", err)
	}
	return sent.Id, nil
}
