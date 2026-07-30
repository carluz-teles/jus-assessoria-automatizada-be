package notifications

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"

	"github.com/jusassessoria/platform/lib/apperr"
)

// templateFS embeds the e-mail templates at compile time, so a missing template is a
// build error, not a runtime file-I/O error (docs: //go:embed for static assets).
//
//go:embed templates/*.html
var templateFS embed.FS

// EmailChannel is the v0 Channel: it renders the Go template selected by the
// notification type and hands the result to the ResendClient. It owns rendering — the
// use case passes only the type and payload — so a new template or a subject tweak
// never touches the use case.
type EmailChannel struct {
	from      string
	client    ResendClient
	templates *template.Template
}

var _ Channel = (*EmailChannel)(nil)

// NewEmailChannel builds the channel from the sender address and the Resend client,
// parsing the embedded templates once. Missing config is an apperr.Invalid (a fast
// boot fail); a template parse fault is infra. Each type's file defines two
// associated templates: "<type>.subject" and "<type>.body".
func NewEmailChannel(from string, client ResendClient) (*EmailChannel, error) {
	if from == "" {
		return nil, apperr.NewInvalid("email channel: from address is required")
	}
	if client == nil {
		return nil, apperr.NewInvalid("email channel: resend client is required")
	}

	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, apperr.NewInfra("email channel: parse templates", err)
	}
	return &EmailChannel{from: from, client: client, templates: tmpl}, nil
}

// Kind reports the channel value recorded on a delivery.
func (c *EmailChannel) Kind() string { return ChannelEmail }

// Send renders the subject and body for msg.Type from its payload, then delivers via
// the provider, returning the provider message id. A type with no template is
// ErrTemplateNotFound (a config fault, surfaced rather than sent empty).
func (c *EmailChannel) Send(ctx context.Context, msg EmailMessage) (string, error) {
	subject, err := c.render(msg.Type+".subject", msg.Payload)
	if err != nil {
		return "", err
	}
	body, err := c.render(msg.Type+".body", msg.Payload)
	if err != nil {
		return "", err
	}
	return c.client.Send(ctx, c.from, msg.To, subject, body)
}

// render executes the named associated template against data. A missing template is
// the typed ErrTemplateNotFound; an execution fault (a template referencing a bad
// field) is infra. The name carries the offending template for the log.
func (c *EmailChannel) render(name string, data map[string]any) (string, error) {
	if c.templates.Lookup(name) == nil {
		return "", fmt.Errorf("%w: %q", ErrTemplateNotFound, name)
	}

	var buf bytes.Buffer
	if err := c.templates.ExecuteTemplate(&buf, name, data); err != nil {
		return "", apperr.NewInfra("email channel: render "+name, err)
	}
	return buf.String(), nil
}
