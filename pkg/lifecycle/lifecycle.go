// Package lifecycle holds the graceful-shutdown boot loop shared by every
// binary (docs/erd-backend.md §5b.1): install signal handling, run the process
// in the foreground, and on SIGTERM/SIGINT drain within a bounded window before
// exit. "Não sobe pela metade" — and it does not die by halves either.
package lifecycle

import (
	"context"
	"log/slog"
	"os/signal"
	"syscall"
	"time"
)

// shutdownTimeout bounds the drain window (docs §5b.1: graceful drain 30s). A
// hung shutdown step cannot hold the process open past this.
const shutdownTimeout = 30 * time.Second

// RunWithGracefulShutdown runs serve in the foreground until either the process
// receives SIGTERM/SIGINT or serve returns on its own, then invokes shutdown
// with a bounded context so in-flight work drains before exit. name labels the
// binary in the start/stop logs.
//
// serve MUST block for the process's lifetime (an http.Server's Listen, a
// worker's ticker loop) and return when its shutdown hook releases it; a serve
// that returns immediately with an error is a boot failure — it is logged and
// shutdown still runs to release whatever was already wired.
func RunWithGracefulShutdown(name string, serve func() error, shutdown func(context.Context) error) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("service starting", "service", name)

	// Buffered so the goroutine never blocks sending once we have moved on to
	// shutdown — otherwise it would leak waiting for a receiver.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serve()
	}()

	select {
	case err := <-serveErr:
		// serve exited before any signal: a bind failure, or a loop that ended.
		// Non-nil is a real fault; either way fall through to shutdown so already
		// wired resources are released.
		if err != nil {
			slog.Error("service stopped unexpectedly", "service", name, "error", err)
		}
	case <-ctx.Done():
		slog.Info("shutdown signal received", "service", name)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "service", name, "error", err)
	}

	slog.Info("service stopped", "service", name)
}
