// Command worker-court consumes the "court_fetch" asynq queue: FetchAutosBatch's
// self-re-enqueuing job and its per-item retries (internal/court). Boot skeleton:
// same lifecycle as every other worker — config → health → bootstrap → serve. No
// migrations (only cmd/api runs them).
//
// Deliberately isolated from worker-ingestao/worker-ai by LEAST PRIVILEGE: this is
// the only process that talks to the cofre (certificate + TOTP seed + session) AND
// to a tribunal portal — narrowing that surface to one small binary is the same
// motivation the ERD already documented for this slice.
//
// Concurrency is intentionally low: the real ceiling on parallelism is how many
// distinct court_connection exist (each one's session is serialized to exactly one
// in-flight fetch at a time — see fetchAutosRequested's stable step-0 id), not how
// many worker slots are spun up. Raising it past the number of active connections
// buys nothing.
package main

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/certificate"
	"github.com/jusassessoria/platform/internal/court"
	"github.com/jusassessoria/platform/internal/document"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/storage"
	"github.com/jusassessoria/platform/lib/telemetry"
	"github.com/jusassessoria/platform/lib/vault"
	"github.com/jusassessoria/platform/pkg/lifecycle"
)

const (
	serviceName = "worker-court"
	queueName   = "court_fetch"
	concurrency = 4
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

	// Court needs BOTH the cofre (vault — seals/opens the TOTP seed and the
	// persisted session) AND certUC (signs with the advogado's A1 certificate).
	// Without either, authenticating against a tribunal is impossible, so the
	// listener stays unmounted rather than booting half-up — same optional-adapter
	// convention cmd/api uses for this same slice.
	var vlt *vault.Vault
	if cfg.VaultKEK != "" {
		v, err := vault.New(cfg.VaultKEK)
		if err != nil {
			return fmt.Errorf("init vault: %w", err)
		}
		vlt = v
		logger.Info("vault configured")
	} else {
		logger.Warn("VAULT_KEK_BASE64 unset — court FetchAutos listener will be disabled")
	}

	var certUC *certificate.UseCase
	var certCipher certificate.Cipher
	if cfg.GCPKMSKeyName != "" {
		if err := materializeGCPCredentials(cfg.GCPKMSCredentialsJSON); err != nil {
			return fmt.Errorf("materialize gcp creds: %w", err)
		}
		cipher, err := certificate.NewEnvelopeCipher(ctx, cfg.GCPKMSKeyName)
		if err != nil {
			return fmt.Errorf("init cert envelope cipher: %w", err)
		}
		certCipher = cipher
		certUC = certificate.NewUseCase(certificate.NewRepository(), database.NewUnitOfWork(pool), cipher, events.NewOutbox())
		logger.Info("certificate signer ready", "kms_key", cfg.GCPKMSKeyName)
	} else {
		logger.Warn("GCP_KMS_KEY_NAME unset — court FetchAutos listener will be disabled")
	}

	// Storage/document: optional, mirroring cmd/api's own posture — without S3
	// configured, FetchAutos still fetches process/docket metadata normally, it
	// just skips document download (docWriter stays nil, EprocProvider's own
	// nil-check no-ops it).
	var documentWriter court.DocumentWriter
	if cfg.S3Enabled() {
		storageClient, err := storage.New(ctx, storage.Options{
			Endpoint:     cfg.S3Endpoint,
			Region:       cfg.S3Region,
			Bucket:       cfg.S3Bucket,
			AccessKey:    cfg.S3AccessKey,
			SecretKey:    cfg.S3SecretKey,
			UsePathStyle: cfg.S3UsePathStyle,
		})
		if err != nil {
			return fmt.Errorf("init storage: %w", err)
		}
		documentUC := document.NewUseCase(document.NewRepository(), storageClient, events.NewOutbox(), database.NewUnitOfWork(pool))
		documentWriter = documentWriterAdapter{uc: documentUC, storage: storageClient}
		logger.Info("storage configured — court document download enabled", "bucket", cfg.S3Bucket)
	} else {
		logger.Warn("S3 not configured — court FetchAutos will skip document download")
	}

	if vlt != nil && certUC != nil {
		uow := database.NewUnitOfWork(pool)
		courtUC := court.NewUseCase(court.NewRepository(), uow, vlt, events.NewOutbox())
		recordWriter := courtRecordWriterAdapter{uow: database.NewUnitOfWork(pool)}
		partyWriter := partyWriterAdapter{uow: database.NewUnitOfWork(pool)}
		courtUC.RegisterProvider("EPROC", court.NewEprocProvider(
			courtCertSignerFunc(certUC.NewSigner),
			documentWriter,
			court.WithCourtRecordWriter(recordWriter),
			court.WithPartyWriter(partyWriter),
		))

		listener := court.NewListener(courtUC)
		listener.Register(mux)
		logger.Info("court FetchAutos listener registered", "providers", []string{"EPROC"})
	}

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
			if certCipher != nil {
				if err := certCipher.Close(); err != nil {
					return fmt.Errorf("close cert cipher: %w", err)
				}
			}
			if err := telemetryShutdown(shutdownCtx); err != nil {
				return fmt.Errorf("shutdown telemetry: %w", err)
			}
			return nil
		},
	)

	return nil
}

// materializeGCPCredentials mirrors cmd/api's own helper exactly: in PaaS
// (Railway/Fly/Render) the GCP service account JSON arrives as a base64 env var
// (GCP_KMS_CREDENTIALS_JSON), so it is written to disk and GOOGLE_APPLICATION_CREDENTIALS
// pointed at it before the KMS SDK is instantiated (ADC reads that env var). Local
// Docker Compose already mounts the file and sets the env var directly, so this is
// a no-op there. A little copying beats a shared-package dependency for ~15 lines.
func materializeGCPCredentials(b64 string) error {
	if b64 == "" {
		return nil // caller relies on the mounted file path already in env
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode GCP_KMS_CREDENTIALS_JSON base64: %w", err)
	}
	path := "/tmp/gcp-kms.json"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write GCP creds to %s: %w", path, err)
	}
	if err := os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path); err != nil {
		return fmt.Errorf("set GOOGLE_APPLICATION_CREDENTIALS: %w", err)
	}
	return nil
}

// courtCertSignerFunc adapts certificate.UseCase.NewSigner to the court.CertSigner
// port — same technique (and same import-cycle reason) as cmd/api's own
// courtCertSignerFunc, duplicated here because main packages can't import each
// other.
type courtCertSignerFunc func(ctx context.Context, tenantID, id string) (crypto.Signer, *x509.Certificate, []*x509.Certificate, certificate.SignerInfo, error)

func (f courtCertSignerFunc) NewSigner(ctx context.Context, tenantID, id string) (crypto.Signer, *x509.Certificate, []*x509.Certificate, error) {
	signer, leaf, chain, _, err := f(ctx, tenantID, id)
	return signer, leaf, chain, err
}

// documentWriterAdapter satisfies court.DocumentWriter by driving the EXISTING
// document.UseCase write path (Start → PutBytes → Complete) — the same one
// manual uploads already go through, so a downloaded auto feeds the same
// extraction/indexing pipeline (worker-documents) with zero new code
// downstream. Same cross-slice technique as courtCertSignerFunc: only this
// main package is allowed to know about both internal/court and
// internal/document.
type documentWriterAdapter struct {
	uc      *document.UseCase
	storage *storage.Client
}

// courtRecordWriterAdapter satisfies court.CourtRecordWriter by enriching the
// court_record (owned by internal/acquisition) with the eproc capa metadata FetchAutos
// reads. It writes directly through a tenant-scoped UoW (RLS) here — the cross-slice
// glue layer, same posture as documentWriterAdapter — rather than routing through an
// acquisition use case, which does not yet expose this narrow enrichment; promoting it
// to a proper acquisition write path (sqlc query + use case) is the follow-up. Shared
// fields (class, judging_body, filed_at) are filled ONLY when empty so an authoritative
// DATAJUD value is never clobbered by the secondary eproc source; the eproc-only fields
// (magistrate, court_situation, competence) refresh when present and are otherwise kept.
type courtRecordWriterAdapter struct {
	uow database.UnitOfWork
}

// partyWriterAdapter satisfies court.PartyWriter by persisting the eproc capa parties
// (autor/réu + CPF/CNPJ + advogados) into the SAME party/party_counsel tables DJEN
// writes (owned by internal/acquisition), with source='EPROC'. Like courtRecordWriterAdapter
// it is the cross-slice glue: it writes directly through a tenant-scoped UoW (RLS) rather
// than routing through an acquisition use case (which does not yet expose this narrow
// write) — promoting it to a proper acquisition write path is the follow-up.
//
// CONSISTENCY (fill-if-missing — never clobber DATAJUD/DJEN):
//   - party is upserted on its natural key (tenant, case, role, name). ON CONFLICT only
//     fills document when it is currently NULL, so the CPF/CNPJ eproc discloses lands on a
//     DJEN-created (document-less) row without touching its source/name. A brand-new party
//     is inserted with source='EPROC'.
//   - party_counsel is inserted ON CONFLICT (tenant, party, oab, uf) DO NOTHING — a DJEN
//     advogado already on file is left intact; a new one lands with source='EPROC'.
//
// eproc's polo ("AUTOR"/"REU") is mapped onto the persistence role enum
// (PLAINTIFF/DEFENDANT) here so eproc parties share the DJEN unique key and the
// fill-if-missing merge actually finds the matching row. A polo we don't map is skipped
// rather than written under an unknown role.
type partyWriterAdapter struct {
	uow database.UnitOfWork
}

func (a partyWriterAdapter) UpsertParties(ctx context.Context, tenantID, courtRecordID string, parties []court.ProcessParty) error {
	if len(parties) == 0 {
		return nil
	}
	return a.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var caseID string
		err := tx.QueryRow(ctx,
			`SELECT case_id FROM court_record WHERE id = $1 AND tenant_id = $2`,
			courtRecordID, tenantID,
		).Scan(&caseID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // court_record gone (or foreign) — nothing to attach parties to
		}
		if err != nil {
			return fmt.Errorf("resolve case_id for court_record: %w", err)
		}

		for _, party := range parties {
			role, ok := partyRoleFromPolo(party.Role)
			if !ok {
				continue // unknown polo — don't invent a role
			}
			if err := a.upsertParty(ctx, tx, tenantID, caseID, role, party); err != nil {
				return err
			}
		}
		return nil
	})
}

// upsertParty inserts (or fill-if-missing updates) one party and its advogados.
func (a partyWriterAdapter) upsertParty(ctx context.Context, tx database.Tx, tenantID, caseID, role string, party court.ProcessParty) error {
	const upsertPartyQ = `
		INSERT INTO party (tenant_id, case_id, role, name, document, source)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), 'EPROC')
		ON CONFLICT (tenant_id, case_id, role, name)
		DO UPDATE SET document = COALESCE(party.document, NULLIF(EXCLUDED.document, ''))
		RETURNING id`
	var partyID string
	err := tx.QueryRow(ctx, upsertPartyQ,
		tenantID, caseID, role, party.Name, party.Document,
	).Scan(&partyID)
	if err != nil {
		return fmt.Errorf("upsert party: %w", err)
	}

	const upsertCounselQ = `
		INSERT INTO party_counsel (tenant_id, party_id, name, oab, uf, source)
		VALUES ($1, $2, $3, $4, $5, 'EPROC')
		ON CONFLICT (tenant_id, party_id, oab, uf) DO NOTHING`
	for _, c := range party.Counsels {
		if c.OAB == "" && c.UF == "" {
			continue // no natural key to dedup on — skip rather than write an anonymous row
		}
		if _, err := tx.Exec(ctx, upsertCounselQ, tenantID, partyID, c.Name, c.OAB, c.UF); err != nil {
			return fmt.Errorf("upsert party counsel: %w", err)
		}
	}
	return nil
}

// partyRoleFromPolo maps eproc's normalized polo ("AUTOR"/"REU") onto the party.role
// enum DJEN also writes (PLAINTIFF/DEFENDANT), so eproc parties merge into the SAME row
// DJEN created for that participant (shared unique key). An unrecognized polo yields
// ok=false so the caller skips it rather than writing under an unknown role.
func partyRoleFromPolo(polo string) (role string, ok bool) {
	switch strings.ToUpper(strings.TrimSpace(polo)) {
	case "AUTOR":
		return "PLAINTIFF", true
	case "REU":
		return "DEFENDANT", true
	default:
		return "", false
	}
}

func (a courtRecordWriterAdapter) UpdateProcessMetadata(ctx context.Context, tenantID, courtRecordID string, meta court.ProcessMetadata) error {
	var filedAt *time.Time
	if !meta.FiledAt.IsZero() {
		filedAt = &meta.FiledAt
	}
	const q = `UPDATE court_record SET
		class            = COALESCE(NULLIF(class, ''), NULLIF($2, '')),
		judging_body     = COALESCE(NULLIF(judging_body, ''), NULLIF($3, '')),
		magistrate       = COALESCE(NULLIF($4, ''), magistrate),
		court_situation  = COALESCE(NULLIF($5, ''), court_situation),
		competence       = COALESCE(NULLIF($6, ''), competence),
		filed_at         = COALESCE(filed_at, $7)
	WHERE id = $1 AND tenant_id = $8`
	return a.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		_, err := tx.Exec(ctx, q,
			courtRecordID, meta.Class, meta.JudgingBody, meta.Magistrate,
			meta.Situation, meta.Competence, filedAt, tenantID)
		return err
	})
}

func (a documentWriterAdapter) WriteDocument(ctx context.Context, tenantID, courtRecordID, mimeType, checksum, title, documentType string, data []byte) (string, error) {
	started, err := a.uc.Start(ctx, document.StartUploadCommand{
		TenantID:      tenantID,
		CourtRecordID: courtRecordID,
		Origin:        document.OriginCourt,
		Title:         title,
		DocumentType:  documentType,
		MimeType:      mimeType,
		SizeBytes:     int64(len(data)),
	})
	if err != nil {
		return "", fmt.Errorf("start document upload: %w", err)
	}

	if err := a.storage.PutBytes(ctx, started.StorageKey, mimeType, data); err != nil {
		return "", fmt.Errorf("put document bytes: %w", err)
	}

	view, err := a.uc.Complete(ctx, document.CompleteCommand{
		TenantID:   tenantID,
		DocumentID: started.DocumentID,
		Checksum:   checksum,
	})
	if err != nil {
		return "", fmt.Errorf("complete document upload: %w", err)
	}
	return view.ID, nil
}
