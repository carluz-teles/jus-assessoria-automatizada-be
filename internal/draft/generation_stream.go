package draft

import (
	"bufio"
	"context"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// generation_stream.go — SSE endpoint que empurra os chunks brutos do LLM
// pro FE em tempo real (Fatia 2 do streaming). Pareado com o worker-ai que
// publica cada delta no canal pub/sub `draft:<id>:stream` (via lib/pubsub).
//
// Cliente:
//   const es = new EventSource('/v1/pecas/<id>/generation-stream')
//   es.addEventListener('chunk', e => appendToEditor(e.data))
//   es.addEventListener('done',  e => es.close())
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

	ctx, cancel := context.WithCancel(context.Background())
	msgs, err := h.chunkSub.Subscribe(ctx, chunkChannel(draftID))
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
//   - `event: chunk\ndata: <raw>\n\n` pra cada mensagem do pubsub
//   - `: ping\n\n` a cada `genStreamHeartbeat` sem mensagem (evita idle)
//   - `event: done\ndata: <saga_state>\n\n` quando saga_state != EXTRACTING
//
// Sai quando: contexto cancelado, canal fechado, saga terminada, ou Flush falha.
func writeGenerationStreamSSE(
	w *bufio.Writer,
	msgs <-chan []byte,
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

		case payload, ok := <-msgs:
			if !ok {
				return
			}
			if err := writeChunkFrame(w, payload); err != nil {
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

// writeChunkFrame emite `event: chunk\ndata: <payload>\n\n`. Se payload tem
// \n internas, cada linha vira `data: ` própria (SSE não aceita \n cru).
func writeChunkFrame(w *bufio.Writer, payload []byte) error {
	var b strings.Builder
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
