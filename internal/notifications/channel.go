package notifications

import "context"

// EmailMessage is what a delivery hands its channel: the resolved recipient address
// (To), the template selector (Type) and the template data (Payload). The channel
// owns rendering — the use case never builds a subject or body, so a new template
// or a new channel changes only the channel, not the use case.
type EmailMessage struct {
	To      string
	Type    string
	Payload map[string]any
}

// Channel is the delivery port: one implementation per medium. Kind reports the
// channel value it records on a delivery (e.g. EMAIL); Send delivers the message and
// returns the provider's message id for later correlation. The use case depends on
// this interface, never on a concrete provider SDK (docs §2.5) — EmailChannel is the
// v0 implementation, and IN_APP/SMS would be further implementations of the same port.
type Channel interface {
	Kind() string
	Send(ctx context.Context, msg EmailMessage) (providerMessageID string, err error)
}
