package notifications

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubResend captures the arguments of the last Send and returns a preset id/error,
// so a test can assert exactly what HTML the channel rendered and handed the provider.
type stubResend struct {
	from, to, subject, html string
	id                      string
	err                     error
	calls                   int
}

func (s *stubResend) Send(_ context.Context, from, to, subject, html string) (string, error) {
	s.calls++
	s.from, s.to, s.subject, s.html = from, to, subject, html
	return s.id, s.err
}

const fromAddr = "avisos@jusassessoria.test"

// AC5: the channel renders the member_joined template (subject + body from the
// payload) and hands the HTML to the provider, returning its message id.
func TestEmailChannel_Send_RendersMemberJoined(t *testing.T) {
	stub := &stubResend{id: "resend-abc"}
	ch, err := NewEmailChannel(fromAddr, stub)
	if err != nil {
		t.Fatalf("NewEmailChannel: %v", err)
	}

	id, err := ch.Send(context.Background(), EmailMessage{
		To:      "ana@escritorio.test",
		Type:    "member_joined",
		Payload: map[string]any{"member_name": "Ana", "org_name": "Advocacia Silva"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if id != "resend-abc" {
		t.Fatalf("provider id = %q, want resend-abc", id)
	}
	if stub.calls != 1 || stub.from != fromAddr || stub.to != "ana@escritorio.test" {
		t.Fatalf("send envelope = from %q to %q calls %d", stub.from, stub.to, stub.calls)
	}
	// The subject and body were rendered from the payload.
	if !strings.Contains(stub.subject, "Advocacia Silva") {
		t.Fatalf("subject = %q, want the org name", stub.subject)
	}
	if !strings.Contains(stub.html, "Ana") || !strings.Contains(stub.html, "Advocacia Silva") {
		t.Fatalf("body did not render payload: %q", stub.html)
	}
	if strings.Contains(stub.html, "member_joined.body") {
		t.Fatalf("body leaked the template define name: %q", stub.html)
	}
}

// Kind reports the channel value recorded on a delivery.
func TestEmailChannel_Kind(t *testing.T) {
	ch, err := NewEmailChannel(fromAddr, &stubResend{})
	if err != nil {
		t.Fatalf("NewEmailChannel: %v", err)
	}
	if ch.Kind() != ChannelEmail {
		t.Fatalf("Kind() = %q, want %q", ch.Kind(), ChannelEmail)
	}
}

// An unknown notification type has no template — Send fails typed, before any send.
func TestEmailChannel_Send_UnknownTemplate(t *testing.T) {
	stub := &stubResend{}
	ch, err := NewEmailChannel(fromAddr, stub)
	if err != nil {
		t.Fatalf("NewEmailChannel: %v", err)
	}

	_, err = ch.Send(context.Background(), EmailMessage{To: "x@y.test", Type: "does_not_exist"})
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("err = %v, want ErrTemplateNotFound", err)
	}
	if stub.calls != 0 {
		t.Fatalf("provider called %d times for a missing template, want 0", stub.calls)
	}
}

// NewEmailChannel validates its config, failing fast at boot on missing pieces.
func TestNewEmailChannel_Validation(t *testing.T) {
	if _, err := NewEmailChannel("", &stubResend{}); err == nil {
		t.Fatal("NewEmailChannel(\"\", client) = nil error, want a validation error")
	}
	if _, err := NewEmailChannel(fromAddr, nil); err == nil {
		t.Fatal("NewEmailChannel(from, nil) = nil error, want a validation error")
	}
}

// NewResendClient requires an API key (fast boot fail when unset).
func TestNewResendClient_RequiresKey(t *testing.T) {
	if _, err := NewResendClient(""); err == nil {
		t.Fatal("NewResendClient(\"\") = nil error, want a validation error")
	}
	if _, err := NewResendClient("re_test"); err != nil {
		t.Fatalf("NewResendClient(key) = %v, want nil", err)
	}
}
