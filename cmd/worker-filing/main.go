// Command worker-filing consome a fila "filing" do asynq: executa o protocolo
// automático no e-SAJ (RPA chromedp) para as peças que o usuário aprovou.
// Lifecycle idêntico aos demais workers — config → health → bootstrap → serve.
// Concorrência 1: o e-SAJ é stateful por sessão e o protocolo é sequencial.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"

	"github.com/hibiken/asynq"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jusassessoria/platform/internal/certificate"
	"github.com/jusassessoria/platform/internal/draft"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/storage"
	"github.com/jusassessoria/platform/lib/telemetry"
	"github.com/jusassessoria/platform/pkg/lifecycle"
)

const (
	serviceName = "worker-filing"
	queueName   = "filing"
	// concurrency 1: o RPA e-SAJ é sequencial por sessão; paralelizar só criaria
	// contenção de login/estado no tribunal.
	concurrency = 1
)

func main() {
	logger := telemetry.SetupDefault(os.Stdout, config.LogLevelFromEnv())
	if err := run(logger); err != nil {
		logger.Error("worker boot failed", "service", serviceName, "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if err := health.WaitAll(ctx, cfg); err != nil {
		return fmt.Errorf("dependency health check: %w", err)
	}

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

	srv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: concurrency,
		Queues:      map[string]int{queueName: concurrency},
		Logger:      events.NewAsynqLogger(logger),
		LogLevel:    asynq.ErrorLevel,
	})

	mux := asynq.NewServeMux()
	mux.Use(events.Observe(logger))

	// O protocolo automático precisa de storage (ler o PDF congelado + salvar
	// screenshots) e do cofre KMS (decifrar a senha e-SAJ). Sem ambos, o worker
	// sobe ocioso em vez de crash-loop; o protocolo manual (POST /v1/pecas/:id/file)
	// continua funcionando independentemente.
	if !cfg.S3Enabled() {
		logger.Warn("S3 not configured — filing worker listeners not registered")
		return serve(srv, mux, pool, telemetryShutdown, logger)
	}
	if cfg.GCPKMSKeyName == "" {
		logger.Warn("GCP_KMS_KEY_NAME unset — cannot decrypt e-SAJ credentials; filing worker idle")
		return serve(srv, mux, pool, telemetryShutdown, logger)
	}
	if err := materializeGCPCredentials(cfg.GCPKMSCredentialsJSON); err != nil {
		return fmt.Errorf("materialize gcp creds: %w", err)
	}
	cipher, err := certificate.NewEnvelopeCipher(ctx, cfg.GCPKMSKeyName)
	if err != nil {
		return fmt.Errorf("init cert envelope cipher: %w", err)
	}
	defer cipher.Close()
	vault := secretVaultAdapter{cipher}

	storageClient, err := storage.New(ctx, storage.Options{
		Endpoint:     cfg.S3Endpoint,
		Region:       cfg.S3Region,
		Bucket:       cfg.S3Bucket,
		AccessKey:    cfg.S3AccessKey,
		SecretKey:    cfg.S3SecretKey,
		UsePathStyle: cfg.S3UsePathStyle, // MinIO needs path-style; parity with the other workers
	})
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}

	uow := database.NewUnitOfWork(pool)
	outbox := events.NewOutbox()
	draftRepo := draft.NewRepository()

	// Gateway real (chromedp). Em staging/dev com CHROME visível, passe false.
	// O gateway acumula os buffers de screenshot; o worker (WithFilingStorage)
	// persiste e grava os keys na attempt.
	gateway := draft.NewChromedpFilingGateway(true)
	filingUC := draft.NewFilingUseCase(
		uow,
		draftRepo,
		draft.WithFilingOutbox(outbox),
		draft.WithFilingStorage(storageClient),
		draft.WithFilingVault(vault),
		draft.WithFilingGateway(gateway),
	)
	filingUC.Register(mux)
	logger.Info("filing worker ready", "queue", queueName, "kms_key", cfg.GCPKMSKeyName)

	return serve(srv, mux, pool, telemetryShutdown, logger)
}

func serve(srv *asynq.Server, mux *asynq.ServeMux, pool *pgxpool.Pool, telemetryShutdown func(context.Context) error, logger *slog.Logger) error {
	if err := srv.Start(mux); err != nil {
		return fmt.Errorf("start asynq server: %w", err)
	}
	stopped := make(chan struct{})
	lifecycle.RunWithGracefulShutdown(
		serviceName,
		func() error {
			<-stopped
			return nil
		},
		func(shutdownCtx context.Context) error {
			srv.Shutdown()
			close(stopped)
			pool.Close()
			if err := telemetryShutdown(shutdownCtx); err != nil {
				return fmt.Errorf("shutdown telemetry: %w", err)
			}
			return nil
		},
	)
	return nil
}

// secretVaultAdapter adapta certificate.Cipher (KMS envelope) ao port
// draft.SecretVault para decifrar a senha e-SAJ no worker.
type secretVaultAdapter struct {
	cipher certificate.Cipher
}

func (a secretVaultAdapter) Seal(ctx context.Context, plaintext []byte) (*draft.Envelope, error) {
	env, err := a.cipher.Seal(ctx, plaintext)
	if err != nil {
		return nil, err
	}
	return &draft.Envelope{Ciphertext: env.Ciphertext, Nonce: env.Nonce, WrappedDEK: env.WrappedDEK, KEKRef: env.KEKRef}, nil
}

func (a secretVaultAdapter) Open(ctx context.Context, env *draft.Envelope) ([]byte, error) {
	return a.cipher.Open(ctx, &certificate.Envelope{Ciphertext: env.Ciphertext, Nonce: env.Nonce, WrappedDEK: env.WrappedDEK, KEKRef: env.KEKRef})
}

func (a secretVaultAdapter) Close() error { return a.cipher.Close() }

// materializeGCPCredentials adapts PaaS-style env-only credentials into the
// disk-file shape the GCP SDK expects (same workaround used by cmd/api).
func materializeGCPCredentials(b64 string) error {
	if b64 == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode GCP_KMS_CREDENTIALS_JSON base64: %w", err)
	}
	path := "/tmp/gcp-kms.json"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write GCP creds to %s: %w", path, err)
	}
	return os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
}
