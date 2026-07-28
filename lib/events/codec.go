package events

import (
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
)

// Encode builds an asynq task from a dotted event type and its JSON payload. It is
// the symmetric counterpart of Decode: the payload passed here is exactly what
// Decode unmarshals on the consumer side. The relay is the sole producer of tasks
// in this architecture (events reach asynq only through the outbox), so it also
// stamps the trace header; Encode keeps the codec's two halves defined together.
func Encode(typ string, payload []byte, opts ...asynq.Option) *asynq.Task {
	return asynq.NewTask(typ, payload, opts...)
}

// Decode unmarshals an asynq task's payload into T. A malformed payload can never
// succeed on retry, so the error wraps asynq.SkipRetry: the consumer archives the
// task instead of burning its retry budget on a permanently-bad message (docs
// erd-backend §4c.4). Infra-shaped failures stay retryable and are never produced
// here — this only fires on a decode fault, which is terminal.
func Decode[T any](t *asynq.Task) (T, error) {
	var out T
	if err := json.Unmarshal(t.Payload(), &out); err != nil {
		return out, fmt.Errorf("decode %s payload: %v: %w", t.Type(), err, asynq.SkipRetry)
	}
	return out, nil
}
