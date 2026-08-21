// Command cleanup-ingestao is a ONE-OFF operational tool that prunes the ingestion
// noise the lean-ingestion policy leaves behind: stale/COMUNICACAO intimations and
// long-dormant processos, per tenant, in a single RLS-scoped transaction each.
//
// It is NOT a migration (no schema change, not versioned in migrations/) — it is a
// scoped data cleanup an operator runs by hand after a snapshot. It defaults to
// --dry-run=true: it only COUNTS what it would delete and rolls the transaction back.
// Deleting for real requires --dry-run=false explicitly.
//
//	go run ./cmd/cleanup-ingestao                      # dry-run over ALL tenants (counts only)
//	go run ./cmd/cleanup-ingestao --tenant <uuid>      # dry-run over one tenant
//	go run ./cmd/cleanup-ingestao --dry-run=false      # ACTUALLY delete, ALL tenants
//	go run ./cmd/cleanup-ingestao --tenant <uuid> --dry-run=false  # delete one tenant
//
// SAFETY: before running with --dry-run=false, take a snapshot / pg_dump. The delete
// is irreversible. The command is idempotent — a second run finds nothing new.
//
// What it removes, per tenant, in ONE transaction with SET LOCAL app.tenant_id (RLS):
//
//	Predicate A — intimation noise: type='COMUNICACAO' OR published_at < now()-30d.
//	Predicate B — dormant processo (computed AFTER A): a court_record whose newest
//	  living signal GREATEST(MAX(docket_entry.occurred_at), MAX(intimation.published_at))
//	  is < now()-12 months, OR that has no living intimation/andamento left after A.
//
// It NEVER touches processed_event / outbox (idempotency; orphaned asynq marks hit
// ErrDeadlineNotFound → safe no-op) nor capture_run (kept as an audit trail).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/obs"
	"github.com/jusassessoria/platform/lib/telemetry"
)

const serviceName = "cleanup-ingestao"

func main() {
	logger := telemetry.SetupDefault(os.Stdout, config.LogLevelFromEnv())
	if err := run(logger); err != nil {
		logger.Error("cleanup-ingestao failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	tenantFlag := flag.String("tenant", "", "cleanup a single tenant uuid; empty = every tenant")
	dryRun := flag.Bool("dry-run", true, "count only and roll back; pass --dry-run=false to delete for real")
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

	uow := database.NewUnitOfWork(pool)

	if *dryRun {
		logger.WarnContext(ctx, "cleanup-ingestao: DRY RUN — counting only, nothing is deleted; pass --dry-run=false to delete",
			"service", serviceName)
	} else {
		logger.WarnContext(ctx, "cleanup-ingestao: LIVE RUN — rows WILL be deleted; ensure a snapshot/pg_dump exists",
			"service", serviceName)
	}

	tenants, err := tenantsToClean(ctx, pool, *tenantFlag)
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "cleanup-ingestao: starting", "tenants", len(tenants), "dry_run", *dryRun)

	var grand counts
	for _, tenantID := range tenants {
		c, err := cleanTenant(ctx, uow, tenantID, *dryRun)
		if err != nil {
			return fmt.Errorf("tenant %s: %w", tenantID, err)
		}
		grand = grand.add(c)
		logger.InfoContext(ctx, "cleanup-ingestao: tenant done",
			append([]any{obs.KeyTenantID, tenantID, "dry_run", *dryRun}, c.logArgs()...)...)
	}

	logger.InfoContext(ctx, "cleanup-ingestao: finished",
		append([]any{"tenants", len(tenants), "dry_run", *dryRun}, grand.logArgs()...)...)
	return nil
}

// tenantsToClean returns the tenant ids to process: the single --tenant when set,
// otherwise every tenant. Listing tenants is a cross-tenant read, so it runs on the
// pool directly (the `tenant` table itself is not RLS-scoped).
func tenantsToClean(ctx context.Context, pool queryable, only string) ([]string, error) {
	if only != "" {
		return []string{only}, nil
	}
	rows, err := pool.Query(ctx, "SELECT id::text FROM tenant ORDER BY id")
	if err != nil {
		return nil, apperr.NewInfra("cleanup: list tenants", err)
	}
	defer rows.Close()

	tenants := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, apperr.NewInfra("cleanup: scan tenant id", err)
		}
		tenants = append(tenants, id)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.NewInfra("cleanup: iterate tenants", err)
	}
	return tenants, nil
}

// queryable is the read subset of the pool tenantsToClean needs — narrowing the
// dependency keeps the enumeration unit-testable without a real pool.
type queryable interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// cleanTenant runs the whole prune for one tenant inside a single RLS-scoped
// transaction. On --dry-run it counts every predicate's target then returns an error
// sentinel that the unit of work rolls back on, so nothing is committed; on a live run
// it deletes child-first and commits. Either way it returns the per-table counts.
func cleanTenant(ctx context.Context, uow database.UnitOfWork, tenantID string, dryRun bool) (counts, error) {
	var c counts
	err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		if c, err = pruneInTx(ctx, tx, dryRun); err != nil {
			return err
		}
		if dryRun {
			// Force a rollback so a dry run never commits. errRollback is swallowed by
			// the caller — it is the signal, not a failure.
			return errRollback
		}
		return nil
	})
	if err != nil && !errors.Is(err, errRollback) {
		return counts{}, err
	}
	return c, nil
}

// errRollback is the sentinel cleanTenant returns from a dry-run so the unit of work
// rolls the transaction back. It never escapes cleanTenant.
var errRollback = fmt.Errorf("cleanup: dry-run rollback")

// counts is the per-table tally of what was (or, in a dry run, would be) deleted.
type counts struct {
	TaskItems             int64
	Tasks                 int64
	TaskSuggestions       int64
	Deadlines             int64
	DraftAttachments      int64
	Petitions             int64
	Reviews               int64
	Drafts                int64
	Documents             int64
	DocketEntries         int64
	Intimations           int64
	PartyCounsels         int64
	Parties               int64
	CaseLinks             int64
	SyncRunsCleared       int64
	CaseDraftLinksCleared int64
	CourtRecords          int64
	CourtCases            int64
}

func (c counts) add(o counts) counts {
	return counts{
		TaskItems:             c.TaskItems + o.TaskItems,
		Tasks:                 c.Tasks + o.Tasks,
		TaskSuggestions:       c.TaskSuggestions + o.TaskSuggestions,
		Deadlines:             c.Deadlines + o.Deadlines,
		DraftAttachments:      c.DraftAttachments + o.DraftAttachments,
		Petitions:             c.Petitions + o.Petitions,
		Reviews:               c.Reviews + o.Reviews,
		Drafts:                c.Drafts + o.Drafts,
		Documents:             c.Documents + o.Documents,
		DocketEntries:         c.DocketEntries + o.DocketEntries,
		Intimations:           c.Intimations + o.Intimations,
		PartyCounsels:         c.PartyCounsels + o.PartyCounsels,
		Parties:               c.Parties + o.Parties,
		CaseLinks:             c.CaseLinks + o.CaseLinks,
		SyncRunsCleared:       c.SyncRunsCleared + o.SyncRunsCleared,
		CaseDraftLinksCleared: c.CaseDraftLinksCleared + o.CaseDraftLinksCleared,
		CourtRecords:          c.CourtRecords + o.CourtRecords,
		CourtCases:            c.CourtCases + o.CourtCases,
	}
}

// logArgs flattens the counts into slog key/value pairs (low-cardinality keys, the
// numbers as attributes) for a single structured log line.
func (c counts) logArgs() []any {
	return []any{
		"task_items", c.TaskItems,
		"tasks", c.Tasks,
		"task_suggestions", c.TaskSuggestions,
		"deadlines", c.Deadlines,
		"draft_attachments", c.DraftAttachments,
		"petitions", c.Petitions,
		"reviews", c.Reviews,
		"drafts", c.Drafts,
		"documents", c.Documents,
		"docket_entries", c.DocketEntries,
		"intimations", c.Intimations,
		"party_counsels", c.PartyCounsels,
		"parties", c.Parties,
		"case_links", c.CaseLinks,
		"sync_runs_cleared", c.SyncRunsCleared,
		"case_draft_links_cleared", c.CaseDraftLinksCleared,
		"court_records", c.CourtRecords,
		"court_cases", c.CourtCases,
	}
}
