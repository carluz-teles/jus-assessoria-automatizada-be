// Command backfill-lifecycle-reconciliation is a ONE-OFF operational tool that applies
// Achado 2's deadline lifecycle reconciliation (fatia 2a/2b) RETROACTIVELY to
// court_records that already made their ARCHIVED/SUPERSEDED transition BEFORE that fatia
// existed — so no acquisition.court_record_archived/court_record_superseded event was ever
// published for them, and their pending deadlines were never reconciled (the production
// leak Achado 2 found: 100% of ARCHIVED processos hold a dangling deadline; 61 MISSED
// deadlines stranded on SUPERSEDED records).
//
// It drives the REAL use cases (no mock, no reimplemented logic) directly per record, so a
// record it touches gets EXACTLY the same treatment the live event path gives a NEW
// transition:
//
//	ARCHIVED   → deadline.UseCase.OnCourtRecordArchived — resolves every PENDING/OPEN/MISSED
//	             prazo to RESOLVED_ON_CONCLUSION, audits one deadline_event per row, and
//	             emits one deadline.resolved_on_conclusion each (which the relay/notifications
//	             consumer will turn into a low-priority aviso once it drains the outbox).
//	SUPERSEDED → acquisition's RepointDeadlines, from the superseded placeholder to the ONE
//	             other non-superseded court_record sharing its (tenant_id, cnj_number,
//	             degree) — the SAME natural-key correlation GetCourtRecordByKey uses at grade
//	             time (internal/acquisition/queries/sync.sql). A placeholder with zero or 2+
//	             candidate targets is logged and SKIPPED — never guessed.
//
// Idempotent: OnCourtRecordArchived dedups per record via a STABLE synthetic event id
// ("backfill-lifecycle-reconciliation:archived:<court_record_id>"), so a re-run after a
// partial failure is a safe no-op for records already reconciled. RepointDeadlines is
// naturally idempotent — the second run moves 0 rows once every deadline already followed
// the merge.
//
// SAFE BY DEFAULT: runs in --dry-run (report-only, counts what WOULD change, writes
// nothing) unless --dry-run=false is passed explicitly. NEVER wired into any binary/cmd
// this repo boots automatically — a human runs it deliberately, once, after reviewing the
// dry-run report.
//
//	go run ./cmd/backfill-lifecycle-reconciliation                 # dry-run, report only
//	go run ./cmd/backfill-lifecycle-reconciliation --dry-run=false # ACTUALLY writes
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/internal/deadline"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/telemetry"
)

const serviceName = "backfill-lifecycle-reconciliation"

func main() {
	logger := telemetry.SetupDefault(os.Stdout, config.LogLevelFromEnv())
	if err := run(logger); err != nil {
		logger.Error("backfill-lifecycle-reconciliation failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	dryRun := flag.Bool("dry-run", true, "count only, write nothing; pass --dry-run=false to actually reconcile")
	flag.Parse()

	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := health.WaitAll(ctx, cfg); err != nil {
		return fmt.Errorf("dependency health check: %w", err)
	}

	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}
	defer pool.Close()

	if *dryRun {
		logger.WarnContext(ctx, serviceName+": DRY RUN — counting only, nothing is written; pass --dry-run=false to reconcile",
			"service", serviceName)
	} else {
		logger.WarnContext(ctx, serviceName+": LIVE RUN — deadlines/court_records WILL be written",
			"service", serviceName)
	}

	archivedCount, err := reconcileArchived(ctx, logger, pool, *dryRun)
	if err != nil {
		return fmt.Errorf("archived: %w", err)
	}
	supersededCount, err := reconcileSuperseded(ctx, logger, pool, *dryRun)
	if err != nil {
		return fmt.Errorf("superseded: %w", err)
	}

	logger.InfoContext(ctx, serviceName+": finished",
		"dry_run", *dryRun, "archived_processed", archivedCount, "superseded_processed", supersededCount)
	return nil
}

// archivedRecord is one court_record already ARCHIVED and the id it needs to be re-driven
// through OnCourtRecordArchived — the dry-run report line and the live path's input.
type archivedRecord struct {
	id       string
	tenantID string
}

// reconcileArchived finds every court_record already ARCHIVED and — on a live run — drives
// deadline.UseCase.OnCourtRecordArchived for each, exactly like a live
// acquisition.court_record_archived event would. Returns how many records were processed
// (dry-run: how many WOULD be).
func reconcileArchived(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, dryRun bool) (int, error) {
	rows, err := pool.Query(ctx, `SELECT id::text, tenant_id::text FROM court_record WHERE lifecycle = 'ARCHIVED' ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("list ARCHIVED court_records: %w", err)
	}
	var recs []archivedRecord
	for rows.Next() {
		var r archivedRecord
		if err := rows.Scan(&r.id, &r.tenantID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan ARCHIVED court_record: %w", err)
		}
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate ARCHIVED court_records: %w", err)
	}

	var resolvableDeadlines int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM deadline d
		JOIN court_record cr ON cr.id = d.court_record_id
		WHERE cr.lifecycle = 'ARCHIVED' AND d.status IN ('PENDING', 'OPEN', 'MISSED')`,
	).Scan(&resolvableDeadlines); err != nil {
		return 0, fmt.Errorf("count resolvable deadlines on ARCHIVED records: %w", err)
	}

	logger.InfoContext(ctx, serviceName+": ARCHIVED court_records found",
		"court_records", len(recs), "resolvable_deadlines_pending_open_missed", resolvableDeadlines)
	if dryRun {
		logger.InfoContext(ctx, serviceName+": dry-run — would call OnCourtRecordArchived per record",
			"court_records", len(recs))
		return len(recs), nil
	}

	uc := deadline.NewUseCase(
		deadline.NewRepository(),
		nil, // OnCourtRecordArchived never reads the calendar
		events.NewOutbox(),
		deadline.NewDedup(),
		database.NewUnitOfWork(pool),
	)
	for _, r := range recs {
		ev := deadline.CourtRecordArchived{
			// Stable synthetic event id, scoped to this tool: a re-run after a partial
			// failure dedups the SAME (consumer, event_id) pair the deadline slice already
			// marks live archivals under — no double-resolve, no double aviso.
			Base:          events.Base{EventID: fmt.Sprintf("backfill-lifecycle-reconciliation:archived:%s", r.id), Aggregate: r.id},
			TenantID:      r.tenantID,
			CourtRecordID: r.id,
		}
		if err := uc.OnCourtRecordArchived(ctx, ev); err != nil {
			return 0, fmt.Errorf("OnCourtRecordArchived(%s): %w", r.id, err)
		}
		logger.InfoContext(ctx, serviceName+": reconciled ARCHIVED court_record",
			"court_record_id", r.id, "tenant_id", r.tenantID)
	}
	return len(recs), nil
}

// supersededRecord is one retired (SUPERSEDED) court_record placeholder and the natural
// key (tenant, cnj, degree) used to find the surviving record it merged into.
type supersededRecord struct {
	id, tenantID, cnj, degree string
}

// reconcileSuperseded finds every court_record already SUPERSEDED and — on a live run —
// repoints its deadlines onto the ONE other non-superseded court_record sharing its
// (tenant_id, cnj_number). GetCourtRecordByKey correlates on (cnj_number, degree) at grade
// time, but that degree is the DATAJUD-REVEALED one passed in by the caller — the
// PLACEHOLDER's own degree column stays 'UNKNOWN' forever (SupersedeCourtRecord never
// touches it), so a post-hoc correlation on the placeholder's stored degree would never
// match the graded survivor. cnj_number alone is the retroactive substitute: in practice a
// tenant holds at most one surviving (non-SUPERSEDED) record per cnj_number once a
// placeholder graduates. A placeholder with zero or 2+ candidate targets is logged and
// SKIPPED — never guessed; re-running this tool later (once the data anomaly is fixed by
// hand) will pick it up. Returns how many placeholders were repointed (dry-run: how many
// WOULD be).
func reconcileSuperseded(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, dryRun bool) (int, error) {
	rows, err := pool.Query(ctx,
		`SELECT id::text, tenant_id::text, cnj_number, degree FROM court_record WHERE lifecycle = 'SUPERSEDED' ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("list SUPERSEDED court_records: %w", err)
	}
	var placeholders []supersededRecord
	for rows.Next() {
		var p supersededRecord
		if err := rows.Scan(&p.id, &p.tenantID, &p.cnj, &p.degree); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan SUPERSEDED court_record: %w", err)
		}
		placeholders = append(placeholders, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate SUPERSEDED court_records: %w", err)
	}

	var strandedDeadlines int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deadline d JOIN court_record cr ON cr.id = d.court_record_id WHERE cr.lifecycle = 'SUPERSEDED'`,
	).Scan(&strandedDeadlines); err != nil {
		return 0, fmt.Errorf("count deadlines stranded on SUPERSEDED records: %w", err)
	}
	logger.InfoContext(ctx, serviceName+": SUPERSEDED court_records found",
		"court_records", len(placeholders), "deadlines_currently_stranded", strandedDeadlines)

	type plan struct{ from, to, tenantID string }
	var repointable []plan
	orphans, ambiguous := 0, 0
	for _, p := range placeholders {
		targetRows, err := pool.Query(ctx,
			`SELECT id::text FROM court_record
			 WHERE tenant_id = $1 AND cnj_number = $2 AND id <> $3 AND lifecycle <> 'SUPERSEDED'`,
			p.tenantID, p.cnj, p.id)
		if err != nil {
			return 0, fmt.Errorf("resolve target for %s: %w", p.id, err)
		}
		var targets []string
		for targetRows.Next() {
			var id string
			if err := targetRows.Scan(&id); err != nil {
				targetRows.Close()
				return 0, fmt.Errorf("scan target for %s: %w", p.id, err)
			}
			targets = append(targets, id)
		}
		targetRows.Close()
		if err := targetRows.Err(); err != nil {
			return 0, fmt.Errorf("iterate targets for %s: %w", p.id, err)
		}

		switch len(targets) {
		case 0:
			orphans++
			logger.WarnContext(ctx, serviceName+": SUPERSEDED record has no surviving target — orphan, skipping",
				"court_record_id", p.id, "cnj_number", p.cnj, "degree", p.degree)
		case 1:
			repointable = append(repointable, plan{from: p.id, to: targets[0], tenantID: p.tenantID})
		default:
			ambiguous++
			logger.WarnContext(ctx, serviceName+": SUPERSEDED record has multiple candidate targets — ambiguous, skipping (needs manual review)",
				"court_record_id", p.id, "cnj_number", p.cnj, "degree", p.degree, "candidates", len(targets))
		}
	}
	logger.InfoContext(ctx, serviceName+": SUPERSEDED reconciliation plan",
		"repointable", len(repointable), "orphans_no_target", orphans, "ambiguous_multiple_targets", ambiguous)

	if dryRun {
		logger.InfoContext(ctx, serviceName+": dry-run — would call RepointDeadlines per repointable record",
			"court_records", len(repointable))
		return len(repointable), nil
	}

	repo := acquisition.NewRepository(pool)
	uow := database.NewUnitOfWork(pool)
	for _, r := range repointable {
		var moved int
		err := uow.Do(ctx, r.tenantID, func(tx database.Tx) error {
			m, err := repo.RepointDeadlines(ctx, tx, r.tenantID, r.from, r.to)
			moved = m
			return err
		})
		if err != nil {
			return 0, fmt.Errorf("RepointDeadlines(%s -> %s): %w", r.from, r.to, err)
		}
		logger.InfoContext(ctx, serviceName+": repointed SUPERSEDED record's deadlines",
			"from_court_record_id", r.from, "to_court_record_id", r.to, "deadlines_moved", moved)
	}
	return len(repointable), nil
}
