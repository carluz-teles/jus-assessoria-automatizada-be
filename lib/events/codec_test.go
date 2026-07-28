package events

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/hibiken/asynq"
)

func TestEncodeDecode_RoundTrip(t *testing.T) {
	type payload struct {
		Foo string `json:"foo"`
		N   int    `json:"n"`
	}
	want := payload{Foo: "bar", N: 7}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	task := Encode("minuta.revised", raw)
	if task.Type() != "minuta.revised" {
		t.Errorf("task.Type() = %q, want %q", task.Type(), "minuta.revised")
	}

	got, err := Decode[payload](task)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if got != want {
		t.Errorf("Decode() = %+v, want %+v", got, want)
	}
}

// A malformed payload is terminal — Decode wraps asynq.SkipRetry so the consumer
// archives the task rather than retrying a permanently-bad message.
func TestDecode_BadPayload_SkipsRetry(t *testing.T) {
	type payload struct {
		A int `json:"a"`
	}
	task := asynq.NewTask("minuta.revised", []byte("{not valid json"))

	_, err := Decode[payload](task)
	if err == nil {
		t.Fatal("Decode() error = nil, want error")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("Decode() error = %v, want it to wrap asynq.SkipRetry", err)
	}
}
