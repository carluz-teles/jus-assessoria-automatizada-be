package telemetry

// White-box tests for fanoutHandler (unexported). The black-box slog_test.go
// covers the public NewLogger/stdout contract; here we pin the fanout's dispatch
// semantics directly: Enabled is OR across bases, Handle delivers the same record
// to every ENABLED base and joins their errors, and WithAttrs/WithGroup derive a
// fanout that still fans out.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// testTime is a fixed timestamp for records under test; the spy never inspects
// it, so its value only needs to be stable.
func testTime() time.Time { return time.Time{} }

// spyRecord captures what a spyHandler observed on a single Handle call, so a
// test can assert the record (and the handler's accumulated attrs/groups)
// reached each base.
type spyRecord struct {
	id       string
	msg      string
	level    slog.Level
	numAttrs int
	attrs    []slog.Attr
	groups   []string
}

// spyHandler is a minimal slog.Handler that records every Handle it receives into
// a shared sink. Enabled is gated on a minimum level so tests can exercise the
// OR semantics and the per-base level filtering. Derived handlers (WithAttrs /
// WithGroup) share the same sink so the test sees calls no matter which
// derivation produced them.
type spyHandler struct {
	id     string
	min    slog.Level
	attrs  []slog.Attr
	groups []string
	err    error
	sink   *[]spyRecord
}

var _ slog.Handler = (*spyHandler)(nil)

func (s *spyHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= s.min
}

func (s *spyHandler) Handle(_ context.Context, rec slog.Record) error {
	*s.sink = append(*s.sink, spyRecord{
		id:       s.id,
		msg:      rec.Message,
		level:    rec.Level,
		numAttrs: rec.NumAttrs(),
		attrs:    s.attrs,
		groups:   s.groups,
	})
	return s.err
}

func (s *spyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *s
	next.attrs = append(append([]slog.Attr{}, s.attrs...), attrs...)
	return &next
}

func (s *spyHandler) WithGroup(name string) slog.Handler {
	next := *s
	next.groups = append(append([]string{}, s.groups...), name)
	return &next
}

func TestFanoutHandler_Enabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mins []slog.Level
		want bool
	}{
		{name: "no bases", mins: nil, want: false},
		{name: "all disabled", mins: []slog.Level{slog.LevelError, slog.LevelWarn}, want: false},
		{name: "one enabled", mins: []slog.Level{slog.LevelError, slog.LevelInfo}, want: true},
		{name: "all enabled", mins: []slog.Level{slog.LevelInfo, slog.LevelDebug}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			bases := make([]slog.Handler, len(tt.mins))
			for i, m := range tt.mins {
				bases[i] = &spyHandler{min: m}
			}
			h := newFanoutHandler(bases...)

			if got := h.Enabled(context.Background(), slog.LevelInfo); got != tt.want {
				t.Errorf("Enabled(Info) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFanoutHandler_Handle_DispatchesToEnabledBasesOnly(t *testing.T) {
	t.Parallel()

	var sink []spyRecord
	// a accepts Info; b only Error — so an Info record must reach a, not b. This
	// pins that the fanout honors each base's Enabled (level filtering survives
	// the fan-out; a base that would drop the record never sees it).
	a := &spyHandler{id: "a", min: slog.LevelInfo, sink: &sink}
	b := &spyHandler{id: "b", min: slog.LevelError, sink: &sink}
	h := newFanoutHandler(a, b)

	rec := slog.NewRecord(testTime(), slog.LevelInfo, "hello", 0)
	rec.AddAttrs(slog.String("k", "v"))
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(sink) != 1 {
		t.Fatalf("got %d deliveries, want 1 (only the Info-enabled base)", len(sink))
	}
	if sink[0].id != "a" || sink[0].msg != "hello" {
		t.Errorf("delivery = %+v, want id=a msg=hello", sink[0])
	}
	// The record must arrive intact (same attr the caller set), and the caller's
	// record must not be mutated by the fan-out.
	if sink[0].numAttrs != 1 {
		t.Errorf("delivered record NumAttrs = %d, want 1", sink[0].numAttrs)
	}
	if rec.NumAttrs() != 1 {
		t.Errorf("caller record mutated: NumAttrs = %d, want 1", rec.NumAttrs())
	}
}

func TestFanoutHandler_Handle_DeliversSameRecordToAllEnabled(t *testing.T) {
	t.Parallel()

	var sink []spyRecord
	a := &spyHandler{id: "a", min: slog.LevelDebug, sink: &sink}
	b := &spyHandler{id: "b", min: slog.LevelDebug, sink: &sink}
	h := newFanoutHandler(a, b)

	rec := slog.NewRecord(testTime(), slog.LevelInfo, "hello", 0)
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(sink) != 2 {
		t.Fatalf("got %d deliveries, want 2 (both bases enabled)", len(sink))
	}
	for _, r := range sink {
		if r.msg != "hello" || r.level != slog.LevelInfo {
			t.Errorf("delivery = %+v, want msg=hello level=Info", r)
		}
	}
}

func TestFanoutHandler_Handle_JoinsErrors(t *testing.T) {
	t.Parallel()

	errA := errors.New("base a failed")
	errB := errors.New("base b failed")
	var sink []spyRecord
	a := &spyHandler{id: "a", min: slog.LevelDebug, err: errA, sink: &sink}
	b := &spyHandler{id: "b", min: slog.LevelDebug, err: errB, sink: &sink}
	h := newFanoutHandler(a, b)

	err := h.Handle(context.Background(), slog.NewRecord(testTime(), slog.LevelInfo, "hi", 0))
	if err == nil {
		t.Fatal("Handle returned nil, want joined error")
	}
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Errorf("joined error = %v, want both base errors", err)
	}
}

func TestFanoutHandler_WithAttrs_PropagatesToAllBases(t *testing.T) {
	t.Parallel()

	var sink []spyRecord
	a := &spyHandler{id: "a", min: slog.LevelDebug, sink: &sink}
	b := &spyHandler{id: "b", min: slog.LevelDebug, sink: &sink}

	derived := newFanoutHandler(a, b).WithAttrs([]slog.Attr{slog.String("component", "repo")})
	if err := derived.Handle(context.Background(), slog.NewRecord(testTime(), slog.LevelInfo, "hi", 0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(sink) != 2 {
		t.Fatalf("got %d deliveries, want 2", len(sink))
	}
	for _, r := range sink {
		if len(r.attrs) != 1 || r.attrs[0].Key != "component" {
			t.Errorf("base %s attrs = %v, want [component]", r.id, r.attrs)
		}
	}
}

func TestFanoutHandler_WithGroup_PropagatesToAllBases(t *testing.T) {
	t.Parallel()

	var sink []spyRecord
	a := &spyHandler{id: "a", min: slog.LevelDebug, sink: &sink}
	b := &spyHandler{id: "b", min: slog.LevelDebug, sink: &sink}

	derived := newFanoutHandler(a, b).WithGroup("request")
	if err := derived.Handle(context.Background(), slog.NewRecord(testTime(), slog.LevelInfo, "hi", 0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(sink) != 2 {
		t.Fatalf("got %d deliveries, want 2", len(sink))
	}
	for _, r := range sink {
		if len(r.groups) != 1 || r.groups[0] != "request" {
			t.Errorf("base %s groups = %v, want [request]", r.id, r.groups)
		}
	}
}
