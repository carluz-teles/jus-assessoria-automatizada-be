// Command api is the HTTP entrypoint. It is the only binary that runs
// migrations, and the fully wired one: config → health → migrate → bootstrap →
// serve, with graceful shutdown draining in-flight requests (docs/erd-backend.md
// §5b.1, §1). The workers/relay/scheduler share the same boot lifecycle but are
// boot skeletons — their domain listeners land with later feature slices.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/internal/identity"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
	"github.com/jusassessoria/platform/lib/storage"
	"github.com/jusassessoria/platform/lib/telemetry"
	"github.com/jusassessoria/platform/pkg/lifecycle"
)

const serviceName = "api"

func main() {
	logger := telemetry.SetupDefault(os.Stdout, slog.LevelInfo)
	if err := run(logger); err != nil {
		logger.Error("api boot failed", "error", err)
		os.Exit(1)
	}
}

// run performs the boot sequence and blocks in the serve loop until shutdown.
// Every boot step returns its error rather than calling Fatal itself (single
// handling rule) — main logs once and exits non-zero. It never returns until a
// signal drains the server, at which point it returns nil.
func run(logger *slog.Logger) error {
	ctx := context.Background()

	// 1. Config — a missing required var dies here, not mid-request.
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// 2. Health — refuse to boot against unreachable dependencies.
	if err := health.CheckAll(ctx, cfg); err != nil {
		return fmt.Errorf("dependency health check: %w", err)
	}

	// 3. Migrations — the api, and only the api, applies the schema (§5b.3).
	if err := database.Up(ctx, cfg.DatabaseURL); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	// 4. Bootstrap — telemetry first so every later component logs with trace
	// correlation and the shutdown hook can flush the providers.
	telemetryShutdown, err := telemetry.Setup(ctx, cfg, serviceName)
	if err != nil {
		return fmt.Errorf("setup telemetry: %w", err)
	}

	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}

	redisOpt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis uri: %w", err)
	}
	asynqClient := asynq.NewClient(redisOpt)

	// Identity wiring: the slice owns the domain; the binary only assembles it.
	uow := database.NewUnitOfWork(pool)
	repo := identity.NewRepository(pool)
	uc := identity.NewUseCase(repo, uow)
	verifier := middleware.NewClerkVerifier(cfg.ClerkSecret, cfg.ClerkIssuer)
	resolver := identity.NewResolver(uc)
	webhook := identity.NewWebhookHandler(cfg.ClerkWebhookSecret, uc)

	// Storage is optional at v0: only wired when S3 is fully configured. No route
	// consumes it yet — the upload slice injects it — so it is built to fail fast
	// on bad credentials at boot and logged as ready.
	if cfg.S3Enabled() {
		if _, err := storage.New(ctx, storage.Options{
			Endpoint:  cfg.S3Endpoint,
			Region:    cfg.S3Region,
			Bucket:    cfg.S3Bucket,
			AccessKey: cfg.S3AccessKey,
			SecretKey: cfg.S3SecretKey,
		}); err != nil {
			return fmt.Errorf("init storage: %w", err)
		}
		logger.Info("storage configured", "bucket", cfg.S3Bucket)
	}

	// 5. Router — the testable seam; no I/O happens here.
	app := newRouter(routerDeps{
		logger:   logger,
		verifier: verifier,
		resolver: resolver,
		webhook:  webhook,
	})

	// 6. Serve with graceful shutdown. Listen blocks until ShutdownWithContext
	// returns it; the shutdown hook drains HTTP first, then releases the pool,
	// asynq client and telemetry providers.
	lifecycle.RunWithGracefulShutdown(
		serviceName,
		func() error {
			if err := app.Listen(":" + cfg.Port); err != nil {
				return fmt.Errorf("http listen: %w", err)
			}
			return nil
		},
		func(shutdownCtx context.Context) error {
			var errs []error
			if err := app.ShutdownWithContext(shutdownCtx); err != nil {
				errs = append(errs, fmt.Errorf("shutdown http: %w", err))
			}
			pool.Close()
			if err := asynqClient.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close asynq client: %w", err))
			}
			if err := telemetryShutdown(shutdownCtx); err != nil {
				errs = append(errs, fmt.Errorf("shutdown telemetry: %w", err))
			}
			return errors.Join(errs...)
		},
	)

	return nil
}
