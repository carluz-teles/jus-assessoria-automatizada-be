package notifications

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	svix "github.com/svix/svix-webhooks/go"

	"github.com/jusassessoria/platform/lib/apperr"
)

// fakeVerifier is a signatureVerifier whose verdict the test fixes: nil accepts,
// err rejects. It lets the dispatch tests skip signing every body — the real svix
// path is exercised separately below.
type fakeVerifier struct{ err error }

func (v fakeVerifier) Verify([]byte, http.Header) error { return v.err }

// spyMarker records the (providerID, status, reason) the handler asked to record, so
// a dispatch test asserts the event → outcome mapping without a database.
type spyMarker struct {
	calls []markerCall
	err   error
}

type markerCall struct {
	providerID string
	status     DeliveryStatus
	reason     string
}

func (m *spyMarker) MarkDeliveryOutcome(_ context.Context, providerID string, status DeliveryStatus, reason string) error {
	m.calls = append(m.calls, markerCall{providerID, status, reason})
	return m.err
}

// doResendWebhook runs req through a fiber app mounting the handler via Register (the
// same entry point the api composes) and returns the response.
func doResendWebhook(t *testing.T, h *WebhookHandler, req *http.Request) *http.Response {
	t.Helper()
	app := fiber.New()
	h.Register(app)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// postResend builds a POST /webhooks/resend carrying body (no svix headers — the
// fake verifier ignores them).
func postResend(body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/resend", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestWebhookHandler_Handle_Dispatch(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCalls  []markerCall
	}{
		{
			name:       "email.bounced flips the delivery to BOUNCED by its provider id",
			body:       `{"type":"email.bounced","data":{"email_id":"resend-1","bounce":{"message":"mailbox full"}}}`,
			wantStatus: http.StatusOK,
			wantCalls:  []markerCall{{"resend-1", DeliveryBounced, "mailbox full"}},
		},
		{
			name:       "email.bounced without a bounce message falls back to the default reason",
			body:       `{"type":"email.bounced","data":{"email_id":"resend-2"}}`,
			wantStatus: http.StatusOK,
			wantCalls:  []markerCall{{"resend-2", DeliveryBounced, bounceReason}},
		},
		{
			name:       "email.complained flips the delivery to COMPLAINED",
			body:       `{"type":"email.complained","data":{"email_id":"resend-3"}}`,
			wantStatus: http.StatusOK,
			wantCalls:  []markerCall{{"resend-3", DeliveryComplained, complaintReason}},
		},
		{
			name:       "an unhandled event type is acknowledged and changes nothing",
			body:       `{"type":"email.delivered","data":{"email_id":"resend-4"}}`,
			wantStatus: http.StatusOK,
			wantCalls:  nil,
		},
		{
			name:       "a malformed body is a 400 and changes nothing",
			body:       `{"type":`,
			wantStatus: http.StatusBadRequest,
			wantCalls:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marker := &spyMarker{}
			h := NewWebhookHandler(fakeVerifier{}, marker)

			resp := doResendWebhook(t, h, postResend([]byte(tt.body)))

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if len(marker.calls) != len(tt.wantCalls) {
				t.Fatalf("marker calls = %+v, want %+v", marker.calls, tt.wantCalls)
			}
			for i, want := range tt.wantCalls {
				if marker.calls[i] != want {
					t.Fatalf("call[%d] = %+v, want %+v", i, marker.calls[i], want)
				}
			}
		})
	}
}

// AC3: an invalid signature is rejected with 401 before any dispatch — the verifier's
// Unauthorized rides its Kind to the status, and the use case is never called.
func TestWebhookHandler_Handle_InvalidSignatureIs401(t *testing.T) {
	marker := &spyMarker{}
	h := NewWebhookHandler(fakeVerifier{err: apperr.NewUnauthorized("invalid signature")}, marker)

	body := []byte(`{"type":"email.bounced","data":{"email_id":"resend-1"}}`)
	resp := doResendWebhook(t, h, postResend(body))

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if len(marker.calls) != 0 {
		t.Fatal("dispatched despite an invalid signature")
	}
}

// AC3: a use-case fault (e.g. the tenant-scoped write failed) surfaces so Resend
// retries — a non-2xx, not a swallowed 200.
func TestWebhookHandler_Handle_UseCaseErrorSurfaces(t *testing.T) {
	marker := &spyMarker{err: apperr.NewInfra("db down", errors.New("boom"))}
	h := NewWebhookHandler(fakeVerifier{}, marker)

	body := []byte(`{"type":"email.bounced","data":{"email_id":"resend-1"}}`)
	resp := doResendWebhook(t, h, postResend(body))

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = %d, want a non-2xx so Resend retries", resp.StatusCode)
	}
}

// testResendSecret is svix's documented example signing secret; the test signs and
// verifies with it, so no real Resend instance is needed. Resend signs webhooks with
// svix, so this exercises the exact production verification path.
const testResendSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"

// signedResend builds a POST carrying body plus valid svix-id/-timestamp/-signature
// headers computed over body with secret — the shape Resend sends.
func signedResend(t *testing.T, secret string, body []byte) *http.Request {
	t.Helper()

	wh, err := svix.NewWebhook(secret)
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	now := time.Now()
	sig, err := wh.Sign("msg_test", now, body)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/resend", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("svix-id", "msg_test")
	req.Header.Set("svix-timestamp", strconv.FormatInt(now.Unix(), 10))
	req.Header.Set("svix-signature", sig)
	return req
}

// AC3: the REAL svix verifier accepts a correctly-signed body and rejects a tampered
// one with 401 — proving NewSvixVerifier wires the signature check the api relies on.
func TestWebhookHandler_Handle_RealSvixSignature(t *testing.T) {
	body := []byte(`{"type":"email.bounced","data":{"email_id":"resend-signed"}}`)

	t.Run("a valid signature is accepted and dispatched", func(t *testing.T) {
		marker := &spyMarker{}
		h := NewWebhookHandler(NewSvixVerifier(testResendSecret), marker)

		resp := doResendWebhook(t, h, signedResend(t, testResendSecret, body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if len(marker.calls) != 1 || marker.calls[0].providerID != "resend-signed" {
			t.Fatalf("calls = %+v, want one for resend-signed", marker.calls)
		}
	})

	t.Run("a tampered signature is rejected with 401", func(t *testing.T) {
		marker := &spyMarker{}
		h := NewWebhookHandler(NewSvixVerifier(testResendSecret), marker)

		req := signedResend(t, testResendSecret, body)
		req.Header.Set("svix-signature", "v1,deadbeefdeadbeefdeadbeefdeadbeef") // corrupt it
		resp := doResendWebhook(t, h, req)

		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if len(marker.calls) != 0 {
			t.Fatal("dispatched despite a tampered signature")
		}
	})
}
