package notifications

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// stream.go is the notifications real-time push: GET /v1/notifications/stream, a
// Server-Sent Events endpoint. The browser opens the stream once; the handler
// subscribes to the caller's tenant push channel (notif:<tenant>, the very channel
// the in-app consumer publishes on in slice 2b) and relays every fresh aviso the
// moment it lands. tenant_id comes from the verified principal, never the body, so a
// caller only ever sees its own tenant's pushes.

const (
	// defaultHeartbeat is how often the stream emits a comment ping when no aviso is
	// flowing. It keeps intermediaries from idling the connection out AND is the
	// reliable client-gone probe: a ping whose Flush fails means the browser hung up,
	// which ends the loop and tears the subscription down.
	defaultHeartbeat = 20 * time.Second

	// sseHeartbeat is an SSE comment line (":" prefix) — EventSource ignores it, but
	// it is enough traffic to keep the connection alive and to surface a dead peer.
	sseHeartbeat = ": ping\n\n"
)

// stream handles GET /v1/notifications/stream. It subscribes to the caller's tenant
// push channel and streams each aviso as an SSE frame until the client disconnects
// or the subscription ends.
//
// fasthttp streams the body from a goroutine that runs AFTER this handler returns,
// so the subscription is bound to a fresh cancelable context (not the recycled
// request ctx). writeSSE returns when the client goes away (a Flush fault) or the
// stream closes; the deferred cancel then stops the subscription — no goroutine
// outlives the connection.
func (h *Handler) stream(c *fiber.Ctx) error {
	p, _ := httpx.PrincipalFromCtx(c)

	ctx, cancel := context.WithCancel(context.Background())
	msgs, err := h.sub.Subscribe(ctx, pushChannel(p.TenantID))
	if err != nil {
		cancel()
		return apperr.NewInfra("subscribe to notifications stream", err)
	}

	// SSE headers, set before the body stream writer is attached.
	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set(fiber.HeaderCacheControl, "no-cache")
	c.Set(fiber.HeaderConnection, "keep-alive")

	heartbeat := h.heartbeat
	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()
		defer cancel() // stop the subscription when the stream ends → no leak
		writeSSE(w, msgs, ctx.Done(), ticker.C)
	})
	return nil
}

// writeSSE is the SSE write loop, extracted pure so it is testable without a network:
// it formats and flushes frames drawn from msgs and heartbeat ticks, owning no I/O
// setup — the handler wires the channels; this only formats and flushes.
//
// It returns when: (a) done is closed or msgs is closed (a server-side cancel or the
// subscription ending), or (b) a Flush fails — in fasthttp a Flush error is the
// reliable signal that the browser disconnected. Either way the caller's deferred
// cancel tears the subscription down, so nothing leaks.
func writeSSE(w *bufio.Writer, msgs <-chan []byte, done <-chan struct{}, ticks <-chan time.Time) {
	for {
		select {
		case <-done:
			return
		case <-ticks:
			if err := writeFrame(w, sseHeartbeat); err != nil {
				return
			}
		case payload, ok := <-msgs:
			if !ok {
				return
			}
			if err := writeFrame(w, formatEvent(payload)); err != nil {
				return
			}
		}
	}
}

// writeFrame writes one already-formatted SSE frame and flushes it. A flush error
// (the client hung up) is returned so writeSSE stops and the subscription unwinds.
func writeFrame(w *bufio.Writer, frame string) error {
	if _, err := w.WriteString(frame); err != nil {
		return err
	}
	return w.Flush()
}

// formatEvent renders one push payload as an SSE "notification" frame:
//
//	id: <notification_id>\nevent: notification\ndata: <raw payload>\n\n
//
// The id lets EventSource resume via Last-Event-ID; it is the aviso's id, pulled from
// the payload. The payload (already single-line JSON from the publisher) is relayed
// verbatim as the data field. A payload with no recoverable id still ships — just
// without the id line — so a malformed push never wedges the stream.
func formatEvent(payload []byte) string {
	var b strings.Builder
	if id := extractID(payload); id != "" {
		b.WriteString("id: ")
		b.WriteString(id)
		b.WriteByte('\n')
	}
	b.WriteString("event: notification\ndata: ")
	b.Write(payload)
	b.WriteString("\n\n")
	return b.String()
}

// extractID pulls the aviso id out of a raw push payload for the SSE id field. A parse
// fault returns "" (the frame ships without an id line) rather than an error — the
// push is best-effort and the data field still carries the full payload.
func extractID(payload []byte) string {
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	return m.ID
}
