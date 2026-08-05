package notifications

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/goleak"

	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
	"github.com/jusassessoria/platform/lib/pubsub"
)

// --- test doubles ------------------------------------------------------------

// closedSubscriber is the inbox tests' benign subscriber: Subscribe hands back an
// already-closed channel so a stream, if ever hit, ends at once. The inbox tests
// never open the stream, so it only exists to satisfy the constructor.
type closedSubscriber struct{}

func (closedSubscriber) Subscribe(context.Context, string) (<-chan []byte, error) {
	ch := make(chan []byte)
	close(ch)
	return ch, nil
}

// fakeSubscriber records the channel the handler joined (to assert per-tenant
// isolation) and hands back a caller-supplied stream, so a handler test feeds the
// stream deterministically.
type fakeSubscriber struct {
	ch         chan []byte
	err        error
	gotChannel string
}

func (f *fakeSubscriber) Subscribe(_ context.Context, channel string) (<-chan []byte, error) {
	f.gotChannel = channel
	return f.ch, f.err
}

// errWriter fails every write, so a bufio.Writer over it makes Flush return an error
// — the fasthttp "client hung up" signal writeSSE must stop on.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

// newStreamApp builds an app whose /v1 group mirrors production: Auth resolves a
// principal, then the notifications routes mount under it with the given subscriber.
func newStreamApp(sub subscriber, user, tenant string, opts ...Option) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{user: user, tenant: tenant}))
	NewHandler(&fakeReader{}, sub, opts...).Register(v1)
	return app
}

// --- HTTP surface tests ------------------------------------------------------

// AC1: GET /v1/notifications/stream answers 200 with the SSE content type and
// no-cache, and subscribes to the caller's tenant channel (notif:<tenant>) read from
// the principal — never the body. A closed stream lets app.Test read the completed
// response instead of hanging on an open stream.
func TestStream_OpensEventStream(t *testing.T) {
	t.Parallel()

	ch := make(chan []byte)
	close(ch)
	fs := &fakeSubscriber{ch: ch}
	app := newStreamApp(fs, "u-1", "tenant-9")

	req := httptest.NewRequest(http.MethodGet, "/v1/notifications/stream", nil)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer jwt")
	resp, err := app.Test(req, 2000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get(fiber.HeaderContentType); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get(fiber.HeaderCacheControl); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if fs.gotChannel != "notif:tenant-9" {
		t.Errorf("subscribed channel = %q, want notif:tenant-9 (principal tenant)", fs.gotChannel)
	}
}

// AC1: without a bearer token the auth boundary 401s before the handler runs — the
// subscriber is never touched.
func TestStream_NoToken_401(t *testing.T) {
	t.Parallel()

	fs := &fakeSubscriber{ch: make(chan []byte)}
	app := newStreamApp(fs, "u-1", "tenant-1")

	req := httptest.NewRequest(http.MethodGet, "/v1/notifications/stream", nil)
	resp, err := app.Test(req, 2000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if fs.gotChannel != "" {
		t.Errorf("subscribed with channel %q, want no subscription before auth", fs.gotChannel)
	}
}

// --- writeSSE unit tests -----------------------------------------------------

// AC2: each channel message becomes exactly one SSE "notification" frame —
// id/event/data lines then a blank line — and multiple messages produce frames in
// order. Closing msgs ends the loop after draining.
func TestWriteSSE_FormatsNotificationFrames(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	msgs := make(chan []byte, 2)
	msgs <- []byte(`{"id":"n-1","type":"import_finished"}`)
	msgs <- []byte(`{"id":"n-2","type":"new_andamento"}`)
	close(msgs)

	writeSSE(w, msgs, make(chan struct{}), nil)

	want := "id: n-1\nevent: notification\ndata: {\"id\":\"n-1\",\"type\":\"import_finished\"}\n\n" +
		"id: n-2\nevent: notification\ndata: {\"id\":\"n-2\",\"type\":\"new_andamento\"}\n\n"
	if got := buf.String(); got != want {
		t.Fatalf("frames =\n%q\nwant\n%q", got, want)
	}
}

// AC3: a heartbeat tick emits the comment ping and flushes it. The tick is delivered
// on an unbuffered channel, so once the send returns writeSSE has consumed it; closing
// done then stops the loop, leaving exactly one ping.
func TestWriteSSE_EmitsHeartbeat(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	ticks := make(chan time.Time)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		writeSSE(w, nil, done, ticks)
		close(finished)
	}()

	ticks <- time.Now() // consumed by writeSSE → ping written and flushed
	close(done)         // stop the loop
	<-finished

	if got := buf.String(); got != sseHeartbeat {
		t.Fatalf("heartbeat = %q, want %q", got, sseHeartbeat)
	}
}

// AC4: writeSSE returns promptly when done closes (a server-side cancel), without
// blocking.
func TestWriteSSE_ReturnsOnDone(t *testing.T) {
	t.Parallel()

	w := bufio.NewWriter(&bytes.Buffer{})
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		writeSSE(w, nil, done, nil)
		close(finished)
	}()

	close(done)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("writeSSE did not return when done closed")
	}
}

// AC4: writeSSE returns when a Flush fails — the reliable "client is gone" signal in
// fasthttp. Neither done nor msgs is closed here, so the flush error is the only exit.
func TestWriteSSE_ReturnsOnFlushError(t *testing.T) {
	t.Parallel()

	w := bufio.NewWriter(errWriter{})
	msgs := make(chan []byte, 1)
	msgs <- []byte(`{"id":"n-1"}`)

	finished := make(chan struct{})
	go func() {
		writeSSE(w, msgs, make(chan struct{}), nil)
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("writeSSE did not return on flush error")
	}
}

// AC5: subscribing via the real lib/pubsub Subscriber (slice 2b) over miniredis and
// running writeSSE, then cancelling, ends both the subscription forwarder and the
// writer — goleak proves no goroutine outlives the cancel. Not parallel: goleak
// inspects every live goroutine, so it must run without siblings executing.
func TestStream_SubscriptionAndWriterStopOnCancel_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t) // registered first → runs last, after client/server are closed

	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	sub := pubsub.NewRedisPubSub(client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msgs, err := sub.Subscribe(ctx, pushChannel("tenant-x"))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	w := bufio.NewWriter(&bytes.Buffer{})
	finished := make(chan struct{})
	go func() {
		writeSSE(w, msgs, ctx.Done(), nil)
		close(finished)
	}()

	// Cancelling ends the subscription (msgs closes) AND signals done, so writeSSE
	// returns and the pubsub forwarding goroutine exits.
	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("writeSSE did not return after cancel")
	}
}
