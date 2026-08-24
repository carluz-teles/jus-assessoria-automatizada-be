package draft

import (
	"bufio"
	"context"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/pubsub"
)

// generation_stream.go — SSE endpoint que empurra os chunks brutos do LLM
// pro FE em tempo real (Fatia 2 do streaming). Pareado com o worker-ai que
// publica cada delta num Redis Stream `draft:<id>:stream` (via lib/pubsub).
//
// Cliente:
//   const es = new EventSource('/v1/pecas/<id>/generation-stream')
//   es.addEventListener('chunk', e => appendToEditor(e.data))
//   es.addEventListener('done',  e => es.close())
//
// Redis Streams garantem que uma queda momentânea do EventSource é
// recuperável: o browser envia Last-Event-ID no reconnect automático e
// o handler retoma XREAD a partir daquele ID (nada perdido dentro do TTL
// de 10 minutos do stream).
//
// Fecha quando: (a) cliente desconecta (Flush falha), (b) heartbeat falha,
// (c) saga_state chega em DRAFTED/FAILED (o event `done` é emitido antes).

const (
	genStreamHeartbeat    = 15 * time.Second
	genStreamSagaPoll     = 2 * time.Second
	genStreamSSEHeartbeat = ": ping\n\n"
)

// generationStream implementa GET /v1/pecas/:id/generation-stream. tenant_id
// vem do principal (nunca do body/path).
func (h *Handler) generationStream(c *fiber.Ctx) error {
	if h.chunkSub == nil {
		return httpx.WriteError(c, apperr.NewInvalid("streaming da geração não está habilitado"))
	}
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	// Last-Event-ID: EventSource envia automático no reconnect. Aceita
	// também via query (?last_event_id=…) pra facilitar teste manual.
	lastID := c.Get("Last-Event-ID")
	if lastID == "" {
		lastID = c.Query("last_event_id")
	}

	ctx, cancel := context.WithCancel(context.Background())
	msgs, err := h.chunkSub.XSubscribe(ctx, chunkChannel(draftID), lastID)
	if err != nil {
		cancel()
		return apperr.NewInfra("subscribe generation stream", err)
	}

	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set(fiber.HeaderCacheControl, "no-cache")
	c.Set(fiber.HeaderConnection, "keep-alive")

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer cancel()
		writeGenerationStreamSSE(w, msgs, ctx, tenantID, draftID, h.getSaga)
	})
	return nil
}

// writeGenerationStreamSSE é o loop de escrita — separado do handler pra ser
// testável sem rede. Emite:
//
//   - `id: <redis-id>\nevent: chunk\ndata: <raw>\n\n` pra cada msg do Stream
//   - `: ping\n\n` a cada `genStreamHeartbeat` sem mensagem (evita idle)
//   - `event: done\ndata: <saga_state>\n\n` quando saga_state != EXTRACTING
//
// O `id:` é o que o browser reflete em Last-Event-ID no próximo GET —
// permite XREAD retomar exatamente dali no reconnect.
func writeGenerationStreamSSE(
	w *bufio.Writer,
	msgs <-chan pubsub.StreamMessage,
	ctx context.Context,
	tenantID, draftID string,
	getSaga getSagaFn,
) {
	heartbeat := time.NewTicker(genStreamHeartbeat)
	defer heartbeat.Stop()
	sagaCheck := time.NewTicker(genStreamSagaPoll)
	defer sagaCheck.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case msg, ok := <-msgs:
			if !ok {
				return
			}
			if err := writeChunkFrame(w, msg.ID, msg.Payload); err != nil {
				return
			}

		case <-heartbeat.C:
			if _, err := w.WriteString(genStreamSSEHeartbeat); err != nil {
				return
			}
			if err := w.Flush(); err != nil {
				return
			}

		case <-sagaCheck.C:
			if getSaga == nil {
				continue
			}
			saga, err := getSaga(ctx, tenantID, draftID)
			if err != nil {
				continue
			}
			if saga == SagaStateDrafted || saga == SagaStateFailed {
				_ = writeDoneFrame(w, saga)
				return
			}
		}
	}
}

// writeChunkFrame emite `id: <redis-id>\nevent: chunk\ndata: <payload>\n\n`.
// Se payload tem \n internas, cada linha vira `data: ` própria (SSE não
// aceita \n cru dentro de um único data field).
func writeChunkFrame(w *bufio.Writer, id string, payload []byte) error {
	var b strings.Builder
	if id != "" {
		b.WriteString("id: ")
		b.WriteString(id)
		b.WriteByte('\n')
	}
	b.WriteString("event: chunk\n")
	for _, line := range strings.Split(string(payload), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	if _, err := w.WriteString(b.String()); err != nil {
		return err
	}
	return w.Flush()
}

// writeDoneFrame emite `event: done\ndata: <saga>\n\n` — cliente ouve e fecha
// o EventSource sem tentar reconectar.
func writeDoneFrame(w *bufio.Writer, saga string) error {
	frame := "event: done\ndata: " + saga + "\n\n"
	if _, err := w.WriteString(frame); err != nil {
		return err
	}
	return w.Flush()
}
