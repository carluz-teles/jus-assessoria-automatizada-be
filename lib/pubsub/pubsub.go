// Package pubsub is a thin Redis pub/sub client — infra, not domain. Slices inject
// a Publisher/Subscriber and never see go-redis directly.
//
// Unlike the transactional outbox (lib/events), this carries NO delivery guarantee:
// a publish is best-effort, fired AFTER the write commits. The database stays the
// source of truth; a dropped message just means the client reloads on the next
// fetch instead of getting the real-time push. It backs the in-app SSE fan-out —
// the consumer publishes an aviso the moment it persists, the SSE endpoint (a later
// slice) subscribes per tenant and relays to the browser.
package pubsub

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Publisher fires one message to a channel, best-effort. The caller owns the
// decision to swallow-and-log the error (the write already committed) — Publish
// only reports it.
type Publisher interface {
	Publish(ctx context.Context, channel string, payload []byte) error
}

// Subscriber streams a channel's messages until ctx is cancelled. The returned
// channel is closed when ctx ends or the connection drops, so the caller ranges
// over it without a second stop signal.
type Subscriber interface {
	Subscribe(ctx context.Context, channel string) (<-chan []byte, error)
}

// redisPubSub implements both halves over a single *redis.Client (safe for
// concurrent use — the client pools connections). The client is injected so its
// lifecycle (and the graceful-shutdown Close) stays with the composition root.
type redisPubSub struct {
	client *redis.Client
}

// Compile-time proof redisPubSub satisfies both ports.
var (
	_ Publisher  = (*redisPubSub)(nil)
	_ Subscriber = (*redisPubSub)(nil)
)

// NewRedisPubSub wraps an already-built redis client. The client is owned by the
// caller (it is closed at shutdown there), not by this wrapper.
func NewRedisPubSub(client *redis.Client) *redisPubSub {
	return &redisPubSub{client: client}
}

// Publish sends payload to channel. The returned int (subscriber count) is
// intentionally dropped — a best-effort push does not care whether anyone is
// listening right now (the DB row is already the source of truth).
func (r *redisPubSub) Publish(ctx context.Context, channel string, payload []byte) error {
	if err := r.client.Publish(ctx, channel, payload).Err(); err != nil {
		return fmt.Errorf("publish to %s: %w", channel, err)
	}
	return nil
}

// Subscribe joins channel and returns a stream of raw payloads. It waits for the
// subscription confirmation (Receive) before returning, so a Publish that races
// the Subscribe is not lost (the go-redis canonical pattern). A single goroutine
// then forwards messages onto the returned channel and exits — closing that
// channel and the underlying subscription — the moment ctx is cancelled, so no
// goroutine outlives the caller's context.
func (r *redisPubSub) Subscribe(ctx context.Context, channel string) (<-chan []byte, error) {
	sub := r.client.Subscribe(ctx, channel)
	// Block until Redis confirms the subscription, so a message published right
	// after Subscribe returns cannot slip in before we are actually listening.
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, fmt.Errorf("subscribe to %s: %w", channel, err)
	}

	out := make(chan []byte)
	go func() {
		defer close(out)
		defer sub.Close() // stops go-redis's Channel goroutine — no leak
		in := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-in:
				if !ok {
					return // connection dropped; go-redis closed the channel
				}
				select {
				case out <- []byte(msg.Payload):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}
