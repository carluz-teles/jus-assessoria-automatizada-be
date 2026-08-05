package pubsub_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/goleak"

	"github.com/jusassessoria/platform/lib/pubsub"
)

// TestRedisPubSub_RoundTrip publishes on a channel and asserts the Subscribe
// stream delivers the exact payload, then cancels the context and asserts the
// stream closes with no goroutine left running (goleak). This is the contract the
// in-app push (producer) and the SSE endpoint (consumer, later slice) rely on.
func TestRedisPubSub_RoundTrip(t *testing.T) {
	defer goleak.VerifyNone(t) // registered first → runs last, after the client and server are closed

	// Start miniredis manually (not RunT): RunT closes the server via t.Cleanup, which
	// runs AFTER this test's defers — so its accept goroutine would still be live when
	// goleak checks. A defer Close runs before the goleak defer (LIFO), so the server
	// is down first and only OUR goroutines are under scrutiny.
	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	ps := pubsub.NewRedisPubSub(client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const channel = "notif:tenant-x"
	sub, err := ps.Subscribe(ctx, channel)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	want := []byte(`{"id":"n1","type":"import_finished"}`)
	if err := ps.Publish(context.Background(), channel, want); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got, ok := <-sub:
		if !ok {
			t.Fatal("subscribe channel closed before the message arrived")
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the published message")
	}

	// Cancelling the context ends the subscription: the stream closes and the
	// forwarding goroutine returns (drain confirms it, goleak proves no leak).
	cancel()
	for range sub {
	}
}
