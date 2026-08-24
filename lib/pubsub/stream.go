package pubsub

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// stream.go adiciona a variante REDIS STREAMS (XADD/XREAD) sobre o mesmo
// cliente. Diferença chave versus Publish/Subscribe:
//
//   pub/sub  → fire-and-forget: quem não estava conectado no momento da
//              publicação PERDE a mensagem
//   streams  → persistente com IDs sequenciais + XREAD BLOCK: cliente pode
//              retomar a partir de um Last-Event-ID e receber o que perdeu
//
// Usado pro streaming da geração de peça (SSE): quando o EventSource cai
// e reconecta (ex.: WiFi oscila), o browser envia Last-Event-ID auto e o
// SSE handler pede XREAD a partir desse ID — nenhum chunk é perdido.
//
// Retention: XADD com MAXLEN ~ 5000 (aprox = "não passa muito de 5000
// entries") + EXPIRE de 10min no key (post-generation ninguém precisa
// mais). Overhead de memória negligível.

const (
	// streamMaxLen limita entries por key. 5000 chunks cobre peça de
	// ~50k tokens (chunks ~10 tokens); geração real ~3-5k chunks.
	streamMaxLen = 5000
	// streamTTL zera a key depois desse tempo — pós-DRAFTED ninguém precisa.
	streamTTL = 10 * time.Minute
)

// StreamMessage carrega um chunk lido do stream + seu ID sequencial
// (formato Redis "millis-seq") pra client resumir via Last-Event-ID.
type StreamMessage struct {
	ID      string
	Payload []byte
}

// StreamPublisher: XADD com MAXLEN + EXPIRE. Retorna o ID do entry
// gravado (útil pra observabilidade e debug).
type StreamPublisher interface {
	XPublish(ctx context.Context, streamKey string, payload []byte) (string, error)
}

// StreamSubscriber: XREAD BLOCK a partir de lastID. lastID vazio = "0-0"
// (do começo — replay total; útil quando cliente reconecta e não tem
// nenhum ID). Envia zero-cópia via canal; goroutine morre quando ctx
// cancela ou o key expira.
type StreamSubscriber interface {
	XSubscribe(ctx context.Context, streamKey string, lastID string) (<-chan StreamMessage, error)
}

// Compile-time proof.
var (
	_ StreamPublisher  = (*redisPubSub)(nil)
	_ StreamSubscriber = (*redisPubSub)(nil)
)

// XPublish grava payload no stream. MAXLEN mantém tamanho bounded;
// EXPIRE é renovado a cada publish (idempotente — Redis sobrescreve).
func (r *redisPubSub) XPublish(ctx context.Context, streamKey string, payload []byte) (string, error) {
	id, err := r.client.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		MaxLen: streamMaxLen,
		Approx: true, // "~" MAXLEN — muito mais eficiente que exato
		Values: map[string]any{"data": payload},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("xadd %s: %w", streamKey, err)
	}
	// Best-effort renew TTL; ignora erro (não crítico).
	_ = r.client.Expire(ctx, streamKey, streamTTL).Err()
	return id, nil
}

// XSubscribe lê a partir de lastID em loop com XREAD BLOCK. "0" = do
// começo; "$" = só new messages a partir de agora. Fecha o canal quando
// ctx é cancelado ou o key some (delete/expire).
func (r *redisPubSub) XSubscribe(ctx context.Context, streamKey string, lastID string) (<-chan StreamMessage, error) {
	if lastID == "" {
		lastID = "0-0"
	}
	out := make(chan StreamMessage)
	go func() {
		defer close(out)
		cursor := lastID
		for {
			if err := ctx.Err(); err != nil {
				return
			}
			// XREAD BLOCK 5000 STREAMS key cursor — 5s block bounded pra
			// permitir checagem periódica de ctx.Done sem ficar preso.
			res, err := r.client.XRead(ctx, &redis.XReadArgs{
				Streams: []string{streamKey, cursor},
				Block:   5 * time.Second,
				Count:   100,
			}).Result()
			if err != nil {
				// redis.Nil = timeout do BLOCK sem novas mensagens; retenta
				if err == redis.Nil {
					continue
				}
				return
			}
			for _, s := range res {
				for _, msg := range s.Messages {
					cursor = msg.ID
					var payload []byte
					if v, ok := msg.Values["data"]; ok {
						switch t := v.(type) {
						case string:
							payload = []byte(t)
						case []byte:
							payload = t
						}
					}
					select {
					case out <- StreamMessage{ID: msg.ID, Payload: payload}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return out, nil
}
