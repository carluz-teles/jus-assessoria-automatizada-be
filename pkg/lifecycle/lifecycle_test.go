package lifecycle_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/pkg/lifecycle"
)

// When serve returns on its own (no signal), the loop must still invoke shutdown
// so wired resources are released, then return promptly.
func TestRunWithGracefulShutdown_ServeReturns_CallsShutdown(t *testing.T) {
	var shutdownCalled bool

	lifecycle.RunWithGracefulShutdown(
		"test",
		func() error { return nil },
		func(context.Context) error {
			shutdownCalled = true
			return nil
		},
	)

	if !shutdownCalled {
		t.Error("shutdown was not called after serve returned")
	}
}

// A serve that fails to boot (returns an error) is not fatal to the drain path:
// shutdown still runs, and it receives a live, non-cancelled context.
func TestRunWithGracefulShutdown_ServeErrors_StillShutsDownWithLiveContext(t *testing.T) {
	var (
		shutdownCalled bool
		ctxErr         error
	)

	lifecycle.RunWithGracefulShutdown(
		"test",
		func() error { return errors.New("bind failed") },
		func(ctx context.Context) error {
			shutdownCalled = true
			ctxErr = ctx.Err()
			return nil
		},
	)

	if !shutdownCalled {
		t.Fatal("shutdown was not called after serve errored")
	}
	if ctxErr != nil {
		t.Errorf("shutdown context already cancelled: %v", ctxErr)
	}
}
