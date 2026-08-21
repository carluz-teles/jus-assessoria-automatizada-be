package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/acquisition/acquisitiondb"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// Repository is the persistence port the use case depends on (it never sees the
// concrete impl). The two upsert-path methods receive the caller's transaction —
// the use case owns the boundary, the repo only participates — so the row read
// and its upsert share one tx and the outbox write commits with them. List is a
// plain read on the pool, scoped by tenant_id (isolation barrier 1).
type Repository interface {
	GetBySource(ctx context.Context, tx database.Tx, tenantID, source string) (*Integration, error)
	Upsert(ctx context.Context, tx database.Tx, tenantID, source string, scope Scope) (*Integration, error)
	List(ctx context.Context, tenantID string) ([]*Integration, error)

	// Backfill onboarding — the listener's first-activation guard and job insert,
	// plus the completion counter's atomic increment/finalize. Grouped here as the
	// slice's single persistence port; the backfill use case depends on the narrow
	// backfillRepo view of these methods.
	BackfillJobExistsByIntegration(ctx context.Context, tx database.Tx, integrationID string) (bool, error)
	InsertBackfillJob(ctx context.Context, tx database.Tx, params BackfillJobParams) (id string, err error)
	IncrementBackfillSlicesOK(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (BackfillCounters, error)
	IncrementBackfillSlicesError(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (BackfillCounters, error)
	FinalizeBackfillJob(ctx context.Context, tx database.Tx, tenantID, backfillJobID, status string) error

	// Sync cycle — the sync listener's run bookkeeping and consolidation upserts.
	// The sync use case depends on the narrow syncRepo view of these methods.
	// AcquireTenantWriteLock serializes concurrent write txs per tenant (no 40P01);
	// both the sync and enrichment use cases take it as their first tx statement.
	AcquireTenantWriteLock(ctx context.Context, tx database.Tx, tenantID string) error
	InsertSyncRun(ctx context.Context, tx database.Tx, params SyncRunParams) (id string, err error)
	FindSyncRunByEventID(ctx context.Context, tx database.Tx, eventID string) (*SyncRun, error)
	UpdateSyncRun(ctx context.Context, tx database.Tx, outcome SyncRunOutcome) (closed bool, err error)
	FindOrCreateCourtRecord(ctx context.Context, tx database.Tx, params FindOrCreateCourtRecordParams) (record *CourtRecord, created bool, err error)
	// BatchUpsertCourtRecords is the SET-BASED write path (repository_batch.go): it
	// resolves-or-creates a whole window's records in a handful of statements so the
	// per-tenant write lock is held for milliseconds, not seconds. FindOrCreateCourtRecord
	// stays for the enrichment path (one record at a time).
	BatchUpsertCourtRecords(ctx context.Context, tx database.Tx, tenantID string, activeLimit int, params []FindOrCreateCourtRecordParams) (outcomes []CourtRecordOutcome, newCount int, err error)
	UpsertDocketEntries(ctx context.Context, tx database.Tx, params []DocketEntryParams) (newEntries []DocketEntry, err error)
	UpsertIntimations(ctx context.Context, tx database.Tx, params []IntimationParams) (newRows, cancelledRows []IntimationChange, err error)
	// UpsertParties materializes the process's partes (autor/réu/terceiro + advogados)
	// idempotently in the sync tx (repository_batch.go). No event — the cockpit reads
	// them straight (no cross-slice consumer in v0).
	UpsertParties(ctx context.Context, tx database.Tx, params []PartyParams) error

	// Capture audit trail — the per-tenant capture writes, in the caller's tx (the same
	// tx as the domain write they audit, under the tenant advisory lock).
	// WriteDailyCaptureRun opens+closes a DAILY_CAPTURE row for a day's fan-out;
	// IncrementImportEnrichmentRunBy bumps an IMPORT's ENRICHMENT row (keyed by backfill_job)
	// by the batch step's graded count (kept RUNNING). CloseImportEnrichmentRun + the
	// discovered total + the empty-close insert serve the batch job's deterministic fecho.
	WriteDailyCaptureRun(ctx context.Context, tx database.Tx, params DailyCaptureParams) error
	IncrementImportEnrichmentRun(ctx context.Context, tx database.Tx, tenantID, backfillJobID string, at time.Time) error
	IncrementImportEnrichmentRunBy(ctx context.Context, tx database.Tx, tenantID, backfillJobID string, delta, errs int, at time.Time) error
	GetImportEnrichmentCounter(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (updated, errs int, found bool, err error)
	CountImportDiscoveredRecords(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (int, error)
	CloseImportEnrichmentRun(ctx context.Context, tx database.Tx, params CloseEnrichmentRunParams) (rows int, err error)
	InsertEmptyImportEnrichmentRun(ctx context.Context, tx database.Tx, params CloseEnrichmentRunParams) error

	// Batch enrichment scan — the scan the batch job drives: SelectDueForEnrichment reads a
	// tribunal's due records; CountRemainingDueForJob is the whole-import remaining count that
	// gates the fecho; MarkEnrichmentAttempted stamps a batch's visited records so they drop
	// out of the scan.
	SelectDueForEnrichment(ctx context.Context, tx database.Tx, tenantID, court string, limit int) ([]DueRecord, error)
	CountRemainingDueForJob(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (int, error)
	MarkEnrichmentAttempted(ctx context.Context, tx database.Tx, tenantID string, recordIDs []string, at time.Time) error

	// DATAJUD enrichment — the in-place grade (FIX B) plus the merge fallback for a
	// rare grade conflict. The enrichment use case depends on the narrow enrichRepo
	// view of these. GetCourtRecordByKey is the conflict lookup (natural-key resolve);
	// UpdateCourtRecordGrade mutates the placeholder in place.
	GetCourtRecordByKey(ctx context.Context, tx database.Tx, tenantID, cnjNumber, degree string) (record *CourtRecord, found bool, err error)
	UpdateCourtRecordGrade(ctx context.Context, tx database.Tx, params GradeParams) (*CourtRecord, error)
	RepointIntimations(ctx context.Context, tx database.Tx, tenantID, fromRecordID, toRecordID string) (moved int, err error)
	SupersedeCourtRecord(ctx context.Context, tx database.Tx, tenantID, recordID string) error

	// Re-poll scheduler — the system-scoped due scan and claim. The scheduler use
	// case depends on the narrow schedulerRepo view of these.
	DueCourtRecordsForResync(ctx context.Context, tx database.Tx, limit int) ([]DueRecord, error)
	ClaimCourtRecordResync(ctx context.Context, tx database.Tx, recordID string, nextSyncAt time.Time) error

	// Responsável do processo (case-level) — the assign write path. The resolve hops
	// court_record → court_case; the membership guard checks the target user belongs to
	// the tenant; the update sets/clears assigned_user_id. All tx-taking (the use case
	// owns the tx+RLS boundary). No outbox event yet — auditoria/evento is a future slice.
	ResolveCaseIDByCourtRecord(ctx context.Context, tx database.Tx, tenantID, courtRecordID string) (caseID string, err error)
	AppUserInTenant(ctx context.Context, tx database.Tx, tenantID, appUserID string) (bool, error)
	AssignCaseResponsible(ctx context.Context, tx database.Tx, tenantID, caseID string, assignedUserID *string) error

	// Triagem da intimação (user_status) — the resolve/ignore/reopen write path. It
	// sets ONLY user_status (the DJEN cancellation `status` is untouched), tenant-scoped;
	// a miss/foreign row surfaces as ErrIntimationNotFound. Tx-taking (the use case owns
	// the tx+RLS boundary). No outbox event yet — a future slice.
	SetIntimationUserStatus(ctx context.Context, tx database.Tx, tenantID, intimationID, userStatus string) error

	// AssignIntimacaoResponsaveis sets (or clears with nil) the conductor_user_id and
	// reviewer_user_id of one intimação, tenant-scoped. The use case already validated
	// membership before calling; a miss or foreign row is ErrIntimationNotFound.
	// Tx-taking (the use case owns the tx+RLS boundary).
	AssignIntimacaoResponsaveis(ctx context.Context, tx database.Tx, tenantID, intimationID string, conductorUserID, reviewerUserID *string) error

	// Screen reads — the keyset-paginated read models (off the write path). The read
	// use case depends on the narrow readRepo view of these.
	ListProcessos(ctx context.Context, q ProcessosQuery) ([]ProcessoView, error)
	GetProcesso(ctx context.Context, tenantID, id string) (ProcessoView, error)
	ListIntimacoes(ctx context.Context, q IntimacoesQuery) ([]IntimacaoView, error)
	GetIntimacao(ctx context.Context, tenantID, id string) (IntimacaoDetailView, error)
	ListAndamentosByProcesso(ctx context.Context, q AndamentosQuery) ([]AndamentoView, error)
	ListIntimacoesByProcesso(ctx context.Context, q IntimacoesByProcessoQuery) ([]IntimacaoView, error)
	ListPartesByProcesso(ctx context.Context, tenantID, courtRecordID string) ([]PartyRow, error)
	CountProcessos(ctx context.Context, q ProcessosQuery) (totalCount, total int64, err error)
	CountIntimacoes(ctx context.Context, q IntimacoesQuery) (totalCount, total int64, err error)
	BucketIntimacoes(ctx context.Context, q IntimacoesQuery) (IntimacaoBucketsView, error)
	// Filter options for the list envelopes — the distinct-value reads that back the
	// chips. Each is tenant-scoped and matches the list's own context predicate.
	ListProcessoCourts(ctx context.Context, tenantID string) ([]string, error)
	ListProcessoDegrees(ctx context.Context, tenantID string) ([]string, error)
	ListProcessoAssignees(ctx context.Context, tenantID string) ([]AssigneeOption, error)
	ListIntimacaoCourts(ctx context.Context, tenantID string) ([]string, error)
	SummarizeProcessos(ctx context.Context, tenantID string) (ProcessosSummaryView, error)
	SummarizeIntimacoes(ctx context.Context, tenantID string) (IntimacoesSummaryView, error)
	CountAndamentosByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error)
	CountIntimacoesByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error)
	// GetResumoContext assembles the full context for the AI process summary (base row
	// + cached ai_resume + last 10 andamentos + active intimações + open prazos). Used
	// by the resumo use case through the readRepo port.
	GetResumoContext(ctx context.Context, tenantID, courtRecordID string) (ProcessoResumoCtx, error)
	// GetIntimacaoAnaliseContext assembles the context for the AI intimation analysis (the
	// teor + the court record identification). Used by the analise use case through the
	// analiseReader port. A miss/foreign row → ErrIntimationNotFound.
	GetIntimacaoAnaliseContext(ctx context.Context, tenantID, intimationID string) (IntimacaoAnaliseCtx, error)
	GetImportStatus(ctx context.Context, tenantID string) (ImportStatusView, error)
	GetReconciliationTotals(ctx context.Context, tenantID string) (ReconciliationTotals, error)
	ListReconciliations(ctx context.Context, tenantID string, limit int) ([]ReconciliationView, error)
	GetReconciliation(ctx context.Context, tenantID, jobID string) (ReconciliationView, error)
	ListSyncRunsByJob(ctx context.Context, tenantID, jobID string) ([]ReconciliationRunView, error)
	ListProcessosBySyncRun(ctx context.Context, tenantID, syncRunID string) ([]ProcessoLineView, error)
	ListIntimacoesBySyncRun(ctx context.Context, tenantID, syncRunID string) ([]IntimacaoLineView, error)
	// Captures screen — the audit-trail reads (capture_run UNION backfill_job + the
	// per-row/KPI count derivations). Backed by the readRepo port of the read use case.
	ListCaptureRuns(ctx context.Context, tenantID string, limit int) ([]CaptureRunRow, error)
	GetCaptureRun(ctx context.Context, tenantID, id string) (CaptureRunRow, error)
	GetCaptureSummary(ctx context.Context, tenantID string) (CaptureSummaryRow, error)
	CountDeadlinesCreatedBetween(ctx context.Context, tenantID string, from, to time.Time) (int, error)
	CountTasksCreatedBetween(ctx context.Context, tenantID string, from, to time.Time) (int, error)
	CountDeadlinesCreatedToday(ctx context.Context, tenantID string) (int, error)
	CountWatchedOABsForDJEN(ctx context.Context, tenantID string) (int, error)
	ListWatchedOABsWithName(ctx context.Context, tenantID string) ([]WatchedOABView, error)

	// Publication — the national DJEN firehose ingestion write (tenant-agnostic). The
	// ingestion use case depends on the narrow ingestRepo view of this.
	InsertPublications(ctx context.Context, tx database.Tx, params []PublicationParams) (newCount int, err error)

	// watched_oab — replace an integration's watched OAB set (populate on activation),
	// the per-tenant index the national match joins against.
	ReplaceWatchedOABs(ctx context.Context, tx database.Tx, tenantID, integrationID string, oabKeys []string) error

	// Match — the national join (publication.recipient_oabs ∩ watched_oab), read
	// system-wide (uow.DoSystem). Drives the per-tenant fan-out to intimações.
	MatchPublicationsByDay(ctx context.Context, tx database.Tx, day time.Time) ([]PublicationMatch, error)
	MatchPublicationsForTenantSince(ctx context.Context, tx database.Tx, tenantID string, since time.Time) ([]PublicationMatch, error)
}

// pgRepository is the sqlc-backed implementation. q is bound to the pool for
// reads; the tx-taking writes rebind the generated queries to the passed tx.
type pgRepository struct {
	q *acquisitiondb.Queries
}

var _ Repository = (*pgRepository)(nil)

// NewRepository binds the generated queries to pool (used for reads). Inject a
// *pgxpool.Pool in production; both it and a mock satisfy acquisitiondb.DBTX.
func NewRepository(pool acquisitiondb.DBTX) Repository {
	return &pgRepository{q: acquisitiondb.New(pool)}
}

// GetBySource loads the current integration for (tenant, source) inside the
// caller's tx. A missing row is the typed ErrIntegrationNotFound, never
// (nil, nil), so the use case can branch on "first activation".
func (r *pgRepository) GetBySource(ctx context.Context, tx database.Tx, tenantID, source string) (*Integration, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	row, err := acquisitiondb.New(tx).GetIntegrationBySource(ctx, acquisitiondb.GetIntegrationBySourceParams{
		TenantID: tid,
		Source:   source,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIntegrationNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return integrationToEntity(row)
}

// Upsert activates or re-activates (tenant, source) inside the caller's tx,
// setting the scope and forcing status ACTIVE. RETURNING always yields a row, so
// there is no not-found branch. credential_ref is never written here.
func (r *pgRepository) Upsert(ctx context.Context, tx database.Tx, tenantID, source string, scope Scope) (*Integration, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	raw, err := encodeScope(scope)
	if err != nil {
		return nil, err
	}
	row, err := acquisitiondb.New(tx).UpsertIntegration(ctx, acquisitiondb.UpsertIntegrationParams{
		TenantID: tid,
		Source:   source,
		Scope:    raw,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return integrationToEntity(row)
}

// List returns all of the tenant's integrations, oldest first, filtered by
// tenant_id on the pool. Read models never assemble an aggregate; this returns
// the mapped entities the read handler renders to a view.
func (r *pgRepository) List(ctx context.Context, tenantID string) ([]*Integration, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListIntegrations(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]*Integration, 0, len(rows))
	for _, row := range rows {
		ent, err := integrationToEntity(row)
		if err != nil {
			return nil, err
		}
		out = append(out, ent)
	}
	return out, nil
}

// GetImportStatus returns the tenant's most recent backfill state for the FE banner,
// a plain read on the pool scoped by tenant_id (isolation barrier 1). No job ever →
// pgx.ErrNoRows, mapped to the NONE sentinel (not importing) rather than an error, so
// a tenant that never onboarded a source simply gets a hidden banner.
func (r *pgRepository) GetImportStatus(ctx context.Context, tenantID string) (ImportStatusView, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return ImportStatusView{}, database.WrapInfra(err)
	}
	row, err := r.q.GetLatestBackfillStatus(ctx, tid)
	if errors.Is(err, pgx.ErrNoRows) {
		return ImportStatusView{Status: importStatusNone}, nil
	}
	if err != nil {
		return ImportStatusView{}, database.WrapInfra(err)
	}
	return ImportStatusView{
		Importing:   row.Status == BackfillStatusRunning,
		Status:      row.Status,
		TotalSlices: int(row.TotalSlices),
		SlicesOK:    int(row.SlicesOk),
		SlicesError: int(row.SlicesError),
	}, nil
}

// ListSyncRunsByJob returns the windows (sync_runs) of one import, chronological,
// for the reconciliation detail screen — a pool read scoped by tenant_id (barrier
// 1). The mapper absorbs the pgtype columns: NULL windows become empty strings, the
// COALESCEd empty error message becomes a nil pointer (JSON null).
func (r *pgRepository) ListSyncRunsByJob(ctx context.Context, tenantID, jobID string) ([]ReconciliationRunView, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListSyncRunsByJob(ctx, acquisitiondb.ListSyncRunsByJobParams{
		TenantID:      tid,
		BackfillJobID: nullUUID(jobID),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	views := make([]ReconciliationRunView, 0, len(rows))
	for _, row := range rows {
		v := ReconciliationRunView{
			ID:            row.ID.String(),
			Source:        row.Source,
			Status:        row.Status,
			ProcessosNew:  int(row.CourtRecordsNew),
			IntimacoesNew: int(row.IntimationsNew),
			StartedAt:     row.StartedAt.Time,
		}
		if row.WindowFrom.Valid {
			v.WindowFrom = row.WindowFrom.Time.Format(dateLayout)
		}
		if row.WindowTo.Valid {
			v.WindowTo = row.WindowTo.Time.Format(dateLayout)
		}
		if row.ErrorMessage != "" {
			msg := row.ErrorMessage
			v.Error = &msg
		}
		if row.FinishedAt.Valid {
			finished := row.FinishedAt.Time
			v.FinishedAt = &finished
		}
		views = append(views, v)
	}
	return views, nil
}

// ListReconciliations returns one reconciliation per import (backfill_job) for the
// reconciliations screen — a pool read scoped by tenant_id (barrier 1). Each row
// aggregates its windows' processos/intimações; the mapper absorbs the pgtype cols.
func (r *pgRepository) ListReconciliations(ctx context.Context, tenantID string, limit int) ([]ReconciliationView, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListReconciliations(ctx, acquisitiondb.ListReconciliationsParams{
		TenantID: tid,
		Limit:    int32(limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	views := make([]ReconciliationView, 0, len(rows))
	for _, row := range rows {
		views = append(views, ReconciliationView{
			ID:          row.ID.String(),
			Source:      row.Source,
			Status:      row.Status,
			WindowFrom:  dateOrEmpty(row.WindowFrom),
			WindowTo:    dateOrEmpty(row.WindowTo),
			Processos:   int(row.Processos),
			Intimacoes:  int(row.Intimacoes),
			TotalSlices: int(row.TotalSlices),
			SlicesOK:    int(row.SlicesOk),
			SlicesError: int(row.SlicesError),
			StartedAt:   row.StartedAt.Time,
			FinishedAt:  timestampPtr(row.FinishedAt),
		})
	}
	return views, nil
}

// GetReconciliation returns one import's reconciliation header — the detail
// screen's card. A miss (unknown/other-tenant job) is the typed ENTITY_NOT_FOUND.
func (r *pgRepository) GetReconciliation(ctx context.Context, tenantID, jobID string) (ReconciliationView, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return ReconciliationView{}, database.WrapInfra(err)
	}
	jid, err := uuid.Parse(jobID)
	if err != nil {
		return ReconciliationView{}, apperr.NewNotFound("reconciliação não encontrada")
	}
	row, err := r.q.GetReconciliation(ctx, acquisitiondb.GetReconciliationParams{
		TenantID: tid,
		ID:       jid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ReconciliationView{}, apperr.NewNotFound("reconciliação não encontrada")
	}
	if err != nil {
		return ReconciliationView{}, database.WrapInfra(err)
	}
	return ReconciliationView{
		ID:          row.ID.String(),
		Source:      row.Source,
		Status:      row.Status,
		WindowFrom:  dateOrEmpty(row.WindowFrom),
		WindowTo:    dateOrEmpty(row.WindowTo),
		Processos:   int(row.Processos),
		Intimacoes:  int(row.Intimacoes),
		TotalSlices: int(row.TotalSlices),
		SlicesOK:    int(row.SlicesOk),
		SlicesError: int(row.SlicesError),
		StartedAt:   row.StartedAt.Time,
		FinishedAt:  timestampPtr(row.FinishedAt),
	}, nil
}

// ListProcessosBySyncRun / ListIntimacoesBySyncRun back the collapse: the processes
// and intimations a window first discovered (court_record/intimation.sync_run_id),
// scoped by tenant_id (barrier 1). Empty (never nil) when the window brought none.
func (r *pgRepository) ListProcessosBySyncRun(ctx context.Context, tenantID, syncRunID string) ([]ProcessoLineView, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListProcessosBySyncRun(ctx, acquisitiondb.ListProcessosBySyncRunParams{
		TenantID:  tid,
		SyncRunID: nullUUID(syncRunID),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	views := make([]ProcessoLineView, 0, len(rows))
	for _, row := range rows {
		views = append(views, ProcessoLineView{
			ID:        row.ID.String(),
			CNJNumber: row.CnjNumber,
			Court:     row.Court,
			Degree:    row.Degree,
			Class:     derefString(row.Class),
		})
	}
	return views, nil
}

func (r *pgRepository) ListIntimacoesBySyncRun(ctx context.Context, tenantID, syncRunID string) ([]IntimacaoLineView, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListIntimacoesBySyncRun(ctx, acquisitiondb.ListIntimacoesBySyncRunParams{
		TenantID:  tid,
		SyncRunID: nullUUID(syncRunID),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	views := make([]IntimacaoLineView, 0, len(rows))
	for _, row := range rows {
		views = append(views, IntimacaoLineView{
			ID:              row.ID.String(),
			CNJNumber:       row.CnjNumber,
			Court:           row.Court,
			Degree:          row.Degree,
			Type:            derefString(row.Type),
			Status:          row.Status,
			MadeAvailableAt: row.MadeAvailableAt.Time,
		})
	}
	return views, nil
}

// ListCaptureRuns returns the tenant's captures (daily/enrichment + initial loads),
// newest first, for the Captures screen — a pool read scoped by tenant_id (barrier 1).
// The mapper absorbs the pgtype columns: NULL windows/finished_at become nil pointers.
func (r *pgRepository) ListCaptureRuns(ctx context.Context, tenantID string, limit int) ([]CaptureRunRow, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListCaptureRuns(ctx, acquisitiondb.ListCaptureRunsParams{
		TenantID: tid,
		Limit:    int32(limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]CaptureRunRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, CaptureRunRow{
			ID:                  row.ID.String(),
			Source:              row.Source,
			Kind:                row.Kind,
			WindowFrom:          dateStrPtr(row.WindowFrom),
			WindowTo:            dateStrPtr(row.WindowTo),
			StartedAt:           row.StartedAt.Time,
			FinishedAt:          timestampPtr(row.FinishedAt),
			Status:              row.Status,
			CourtRecordsNew:     int(row.CourtRecordsNew),
			IntimationsNew:      int(row.IntimationsNew),
			CourtRecordsUpdated: int(row.CourtRecordsUpdated),
			Errors:              int(row.Errors),
		})
	}
	return out, nil
}

// GetCaptureRun returns one capture (a DAILY_CAPTURE or ENRICHMENT capture_run) by id,
// tenant-scoped. A non-uuid id is client input → the typed 404 (a capture that cannot
// exist); a miss or a foreign-tenant row is likewise the typed ENTITY_NOT_FOUND.
func (r *pgRepository) GetCaptureRun(ctx context.Context, tenantID, id string) (CaptureRunRow, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return CaptureRunRow{}, database.WrapInfra(err)
	}
	cid, err := uuid.Parse(id)
	if err != nil {
		return CaptureRunRow{}, apperr.NewNotFound("captura não encontrada")
	}
	row, err := r.q.GetCaptureRunByID(ctx, acquisitiondb.GetCaptureRunByIDParams{
		TenantID: tid,
		ID:       cid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return CaptureRunRow{}, apperr.NewNotFound("captura não encontrada")
	}
	if err != nil {
		return CaptureRunRow{}, database.WrapInfra(err)
	}
	return CaptureRunRow{
		ID:                  row.ID.String(),
		Source:              row.Source,
		Kind:                row.Kind,
		WindowFrom:          dateStrPtr(row.WindowFrom),
		WindowTo:            dateStrPtr(row.WindowTo),
		StartedAt:           row.StartedAt.Time,
		FinishedAt:          timestampPtr(row.FinishedAt),
		Status:              row.Status,
		CourtRecordsNew:     int(row.CourtRecordsNew),
		IntimationsNew:      int(row.IntimationsNew),
		CourtRecordsUpdated: int(row.CourtRecordsUpdated),
		Errors:              int(row.Errors),
	}, nil
}

// GetCaptureSummary returns the Captures KPI aggregates (last successful capture +
// today's new intimations), tenant-scoped. A single aggregate read on the pool.
func (r *pgRepository) GetCaptureSummary(ctx context.Context, tenantID string) (CaptureSummaryRow, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return CaptureSummaryRow{}, database.WrapInfra(err)
	}
	row, err := r.q.GetCaptureSummary(ctx, tid)
	if err != nil {
		return CaptureSummaryRow{}, database.WrapInfra(err)
	}
	return CaptureSummaryRow{
		LastCaptureAt:       timestampPtr(row.LastCaptureAt),
		IntimationsNewToday: int(row.IntimationsNewToday),
	}, nil
}

// CountDeadlinesCreatedBetween / CountTasksCreatedBetween count the prazos/tarefas a
// capture derived over its [started_at, finished_at] window, tenant-scoped (barrier 1).
func (r *pgRepository) CountDeadlinesCreatedBetween(ctx context.Context, tenantID string, from, to time.Time) (int, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	n, err := r.q.CountDeadlinesCreatedBetween(ctx, acquisitiondb.CountDeadlinesCreatedBetweenParams{
		TenantID:    tid,
		CreatedAt:   pgtype.Timestamptz{Time: from, Valid: true},
		CreatedAt_2: pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return int(n), nil
}

func (r *pgRepository) CountTasksCreatedBetween(ctx context.Context, tenantID string, from, to time.Time) (int, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	n, err := r.q.CountTasksCreatedBetween(ctx, acquisitiondb.CountTasksCreatedBetweenParams{
		TenantID:    tid,
		CreatedAt:   pgtype.Timestamptz{Time: from, Valid: true},
		CreatedAt_2: pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return int(n), nil
}

// CountDeadlinesCreatedToday backs the "prazos derivados hoje" KPI; CountWatchedOABsForDJEN
// backs the "OABs" card. Both tenant-scoped pool reads (barrier 1).
func (r *pgRepository) CountDeadlinesCreatedToday(ctx context.Context, tenantID string) (int, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	n, err := r.q.CountDeadlinesCreatedToday(ctx, tid)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return int(n), nil
}

func (r *pgRepository) CountWatchedOABsForDJEN(ctx context.Context, tenantID string) (int, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	n, err := r.q.CountWatchedOABsForDJEN(ctx, tid)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return int(n), nil
}

// ListWatchedOABsWithName returns the tenant's DJEN-monitored OABs with the
// most-frequent lawyer name from party_counsel. The generated row has Name as
// string (sqlc can't infer nullability for mode() even with ::text cast), so the
// scan uses *string; an empty string from mode() over no rows becomes nil.
func (r *pgRepository) ListWatchedOABsWithName(ctx context.Context, tenantID string) ([]WatchedOABView, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListWatchedOABsWithName(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	views := make([]WatchedOABView, 0, len(rows))
	for _, row := range rows {
		view := WatchedOABView{OAB: row.Oab}
		if row.Name != "" {
			view.Name = &row.Name
		}
		views = append(views, view)
	}
	return views, nil
}

// GetReconciliationTotals counts the tenant's acquired acervo (ACTIVE processes +
// intimations) for the reconciliations summary — two pool reads scoped by
// tenant_id (barrier 1).
func (r *pgRepository) GetReconciliationTotals(ctx context.Context, tenantID string) (ReconciliationTotals, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return ReconciliationTotals{}, database.WrapInfra(err)
	}
	records, err := r.q.CountActiveCourtRecordsByTenant(ctx, tid)
	if err != nil {
		return ReconciliationTotals{}, database.WrapInfra(err)
	}
	intimations, err := r.q.CountIntimationsByTenant(ctx, tid)
	if err != nil {
		return ReconciliationTotals{}, database.WrapInfra(err)
	}
	return ReconciliationTotals{
		CourtRecords: int(records),
		Intimations:  int(intimations),
	}, nil
}

// BackfillJobExistsByIntegration reports whether any backfill_job already exists
// for the integration, inside the caller's tx. It is the listener's
// first-activation guard: true means a re-activation, so no new backfill.
func (r *pgRepository) BackfillJobExistsByIntegration(ctx context.Context, tx database.Tx, integrationID string) (bool, error) {
	iid, err := uuid.Parse(integrationID)
	if err != nil {
		return false, database.WrapInfra(err)
	}
	exists, err := acquisitiondb.New(tx).BackfillJobExistsByIntegration(ctx, iid)
	if err != nil {
		return false, database.WrapInfra(err)
	}
	return exists, nil
}

// InsertBackfillJob creates the backfill_job inside the caller's tx and returns
// its new id. The date-only window bounds are lifted to pgtype.Date here (this
// is a write param, so the conversion lives with the query, not the mapper).
func (r *pgRepository) InsertBackfillJob(ctx context.Context, tx database.Tx, params BackfillJobParams) (string, error) {
	tid, err := uuid.Parse(params.TenantID)
	if err != nil {
		return "", database.WrapInfra(err)
	}
	iid, err := uuid.Parse(params.IntegrationID)
	if err != nil {
		return "", database.WrapInfra(err)
	}
	id, err := acquisitiondb.New(tx).InsertBackfillJob(ctx, acquisitiondb.InsertBackfillJobParams{
		TenantID:      tid,
		IntegrationID: iid,
		WindowFrom:    pgtype.Date{Time: params.WindowFrom, Valid: true},
		WindowTo:      pgtype.Date{Time: params.WindowTo, Valid: true},
		TotalSlices:   int32(params.TotalSlices),
		Status:        params.Status,
	})
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return id.String(), nil
}

// IncrementBackfillSlicesOK bumps slices_ok inside the caller's tx and returns
// the job's tallies read back atomically (the UPDATE's row lock serializes
// concurrent slice closes). A row invisible under this tenant (RLS) or gone
// yields pgx.ErrNoRows, mapped to the typed ErrBackfillJobNotFound.
func (r *pgRepository) IncrementBackfillSlicesOK(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (BackfillCounters, error) {
	tid, jid, err := parseTenantJob(tenantID, backfillJobID)
	if err != nil {
		return BackfillCounters{}, err
	}
	row, err := acquisitiondb.New(tx).IncrementBackfillSlicesOK(ctx, acquisitiondb.IncrementBackfillSlicesOKParams{
		ID:       jid,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return BackfillCounters{}, ErrBackfillJobNotFound
	}
	if err != nil {
		return BackfillCounters{}, database.WrapInfra(err)
	}
	return BackfillCounters{
		TotalSlices: int(row.TotalSlices),
		SlicesOK:    int(row.SlicesOk),
		SlicesError: int(row.SlicesError),
		Status:      row.Status,
	}, nil
}

// IncrementBackfillSlicesError bumps slices_error inside the caller's tx with the
// same atomic lock-and-read-back contract as IncrementBackfillSlicesOK.
func (r *pgRepository) IncrementBackfillSlicesError(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (BackfillCounters, error) {
	tid, jid, err := parseTenantJob(tenantID, backfillJobID)
	if err != nil {
		return BackfillCounters{}, err
	}
	row, err := acquisitiondb.New(tx).IncrementBackfillSlicesError(ctx, acquisitiondb.IncrementBackfillSlicesErrorParams{
		ID:       jid,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return BackfillCounters{}, ErrBackfillJobNotFound
	}
	if err != nil {
		return BackfillCounters{}, database.WrapInfra(err)
	}
	return BackfillCounters{
		TotalSlices: int(row.TotalSlices),
		SlicesOK:    int(row.SlicesOk),
		SlicesError: int(row.SlicesError),
		Status:      row.Status,
	}, nil
}

// FinalizeBackfillJob flips the job to status inside the caller's tx, guarded to
// the RUNNING → terminal transition by the query's WHERE clause (so it is a
// no-op if some other delivery already finalized). Scoped by tenant_id.
func (r *pgRepository) FinalizeBackfillJob(ctx context.Context, tx database.Tx, tenantID, backfillJobID, status string) error {
	tid, jid, err := parseTenantJob(tenantID, backfillJobID)
	if err != nil {
		return err
	}
	err = acquisitiondb.New(tx).FinalizeBackfillJob(ctx, acquisitiondb.FinalizeBackfillJobParams{
		ID:       jid,
		TenantID: tid,
		Status:   status,
	})
	return database.WrapInfra(err)
}

// parseTenantJob parses the tenant and backfill-job ids shared by the counter
// path, wrapping a bad uuid as a typed infra error.
func parseTenantJob(tenantID, backfillJobID string) (tid, jid uuid.UUID, err error) {
	tid, err = uuid.Parse(tenantID)
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, database.WrapInfra(err)
	}
	jid, err = uuid.Parse(backfillJobID)
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, database.WrapInfra(err)
	}
	return tid, jid, nil
}

// InsertSyncRun opens a sync_run (RUNNING) inside the caller's tx and returns its
// id. court_record_id is left NULL by the query (window discovery is not yet tied
// to one record); the clock-stamped started_at is lifted to pgtype here.
func (r *pgRepository) InsertSyncRun(ctx context.Context, tx database.Tx, params SyncRunParams) (string, error) {
	tid, err := uuid.Parse(params.TenantID)
	if err != nil {
		return "", database.WrapInfra(err)
	}
	iid, err := uuid.Parse(params.IntegrationID)
	if err != nil {
		return "", database.WrapInfra(err)
	}
	id, err := acquisitiondb.New(tx).InsertSyncRun(ctx, acquisitiondb.InsertSyncRunParams{
		TenantID:         tid,
		IntegrationID:    iid,
		ConnectorID:      params.ConnectorID,
		ConnectorVersion: params.ConnectorVersion,
		StartedAt:        pgtype.Timestamptz{Time: params.StartedAt, Valid: true},
		Status:           params.Status,
		EventID:          nullString(params.EventID),
		WindowFrom:       wireDateOrNull(params.WindowFrom),
		WindowTo:         wireDateOrNull(params.WindowTo),
		BackfillJobID:    nullUUID(params.BackfillJobID),
	})
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return id.String(), nil
}

// FindSyncRunByEventID resolves the sync_run opened by eventID inside the caller's
// tx, mapping a miss (pgx.ErrNoRows) to the typed ErrSyncRunNotFound rather than
// (nil, nil). The lookup is scoped by the tx's RLS tenant; event_id is globally
// unique (partial index), so at most one row matches.
func (r *pgRepository) FindSyncRunByEventID(ctx context.Context, tx database.Tx, eventID string) (*SyncRun, error) {
	row, err := acquisitiondb.New(tx).FindSyncRunByEventID(ctx, &eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSyncRunNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return syncRunToEntity(row), nil
}

// UpdateSyncRun closes a sync_run inside the caller's tx, guarded to the RUNNING →
// terminal transition by the query's WHERE clause (compare-and-swap). It reports
// whether THIS call won the close: a matched row means it flipped RUNNING to the
// outcome (closed=true), while pgx.ErrNoRows means the run was already closed by a
// concurrent execution (closed=false) — the caller then skips the terminal event
// so it is published exactly once. The error reason (empty on OK) is encoded to
// the error jsonb column; NULL when there is none.
func (r *pgRepository) UpdateSyncRun(ctx context.Context, tx database.Tx, outcome SyncRunOutcome) (bool, error) {
	id, err := uuid.Parse(outcome.ID)
	if err != nil {
		return false, database.WrapInfra(err)
	}
	errJSON, err := encodeSyncError(outcome.Error)
	if err != nil {
		return false, err
	}
	_, err = acquisitiondb.New(tx).UpdateSyncRun(ctx, acquisitiondb.UpdateSyncRunParams{
		ID:                  id,
		Status:              outcome.Status,
		ItemsNew:            int32(outcome.ItemsNew),
		ItemsDeduped:        int32(outcome.ItemsDeduped),
		CourtRecordsNew:     int32(outcome.CourtRecordsNew),
		CourtRecordsUpdated: int32(outcome.CourtRecordsUpdated),
		IntimationsNew:      int32(outcome.IntimationsNew),
		FinishedAt:          pgtype.Timestamptz{Time: outcome.FinishedAt, Valid: true},
		Error:               errJSON,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, database.WrapInfra(err)
	}
	return true, nil
}

// WriteDailyCaptureRun records a day's DAILY_CAPTURE audit row in the caller's tx —
// the same tx as the fan-out write it audits (writeForTenant), under the tenant
// advisory lock. It opens+closes in one statement (the fan-out is synchronous, so the
// tallies and OK status are known); a re-run of the same day SUMS onto the counters
// (ON CONFLICT), so an at-least-once redelivery never zeroes the day.
func (r *pgRepository) WriteDailyCaptureRun(ctx context.Context, tx database.Tx, params DailyCaptureParams) error {
	tid, err := uuid.Parse(params.TenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	err = acquisitiondb.New(tx).InsertDailyCaptureRun(ctx, acquisitiondb.InsertDailyCaptureRunParams{
		TenantID:            tid,
		WindowFrom:          pgtype.Date{Time: params.Window, Valid: true},
		WindowTo:            pgtype.Date{Time: params.Window, Valid: true},
		StartedAt:           pgtype.Timestamptz{Time: params.StartedAt, Valid: true},
		FinishedAt:          pgtype.Timestamptz{Time: params.FinishedAt, Valid: true},
		CourtRecordsNew:     int32(params.CourtRecordsNew),
		IntimationsNew:      int32(params.IntimationsNew),
		CourtRecordsUpdated: int32(params.CourtRecordsUpdated),
	})
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// IncrementImportEnrichmentRun bumps an IMPORT's ENRICHMENT capture row (keyed by
// backfillJobID) by one applied enrichment, in the caller's tx (the applyEnrichment tx,
// under the tenant advisory lock). The first enrichment of the import creates the row
// RUNNING; the rest increment court_records_updated and advance finished_at, staying
// RUNNING — the terminal status is stamped by the ETA close. `at` is the enrichment's
// wall-clock time (started_at/finished_at).
func (r *pgRepository) IncrementImportEnrichmentRun(ctx context.Context, tx database.Tx, tenantID, backfillJobID string, at time.Time) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	bid, err := uuid.Parse(backfillJobID)
	if err != nil {
		return database.WrapInfra(err)
	}
	err = acquisitiondb.New(tx).IncrementImportEnrichmentRun(ctx, acquisitiondb.IncrementImportEnrichmentRunParams{
		TenantID:      tid,
		BackfillJobID: pgtype.UUID{Bytes: bid, Valid: true},
		At:            pgtype.Timestamptz{Time: at, Valid: true},
	})
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// IncrementImportEnrichmentRunBy bumps an IMPORT's ENRICHMENT capture row by `delta`
// applied enrichments (records the step graded) AND `errs` real parse errors (hits DATAJUD
// returned but we could not parse), in the caller's tx under the tenant advisory lock. Same
// insert-or-increment semantics as the +1 variant, batched. The caller skips it when both
// delta and errs are 0 (an empty step must not mint a spurious RUNNING row).
func (r *pgRepository) IncrementImportEnrichmentRunBy(ctx context.Context, tx database.Tx, tenantID, backfillJobID string, delta, errs int, at time.Time) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	bid, err := uuid.Parse(backfillJobID)
	if err != nil {
		return database.WrapInfra(err)
	}
	err = acquisitiondb.New(tx).IncrementImportEnrichmentRunBy(ctx, acquisitiondb.IncrementImportEnrichmentRunByParams{
		TenantID:      tid,
		BackfillJobID: pgtype.UUID{Bytes: bid, Valid: true},
		At:            pgtype.Timestamptz{Time: at, Valid: true},
		Delta:         int32(delta),
		Errs:          int32(errs),
	})
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// SelectDueForEnrichment reads up to `limit` court_records of one tribunal (court) still
// due for a batch enrichment (completeness < 0.9, never attempted, not superseded), in the
// caller's tx. Scoped by tenant_id (RLS barrier 1); the scan partial index serves it.
func (r *pgRepository) SelectDueForEnrichment(ctx context.Context, tx database.Tx, tenantID, court string, limit int) ([]DueRecord, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := acquisitiondb.New(tx).SelectDueForEnrichment(ctx, acquisitiondb.SelectDueForEnrichmentParams{
		TenantID: tid,
		Court:    court,
		Lim:      int32(limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]DueRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, DueRecord{
			ID:        row.ID.String(),
			TenantID:  tenantID,
			CaseID:    row.CaseID.String(),
			CNJNumber: row.CnjNumber,
			Degree:    row.Degree,
			Court:     row.Court,
		})
	}
	return out, nil
}

// CountRemainingDueForJob counts the court_records of the whole import (the tenant's
// enrichment backlog) still due, in the caller's tx. The batch job closes the ENRICHMENT
// row only when this reaches 0. Scoped by tenant_id.
func (r *pgRepository) CountRemainingDueForJob(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (int, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	// backfillJobID is not a filter (the tenant runs one import at a time in v0); parse it
	// only to reject a malformed id early, keeping the caller's contract honest.
	if _, err := uuid.Parse(backfillJobID); err != nil {
		return 0, database.WrapInfra(err)
	}
	remaining, err := acquisitiondb.New(tx).CountRemainingDueForJob(ctx, tid)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return int(remaining), nil
}

// MarkEnrichmentAttempted stamps enrichment_attempted_at on every record a batch visited
// (graded or not), in the caller's tx, so they drop out of the scan index. Scoped by
// tenant_id. An empty id list is a no-op.
func (r *pgRepository) MarkEnrichmentAttempted(ctx context.Context, tx database.Tx, tenantID string, recordIDs []string, at time.Time) error {
	if len(recordIDs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	ids := make([]uuid.UUID, 0, len(recordIDs))
	for _, id := range recordIDs {
		rid, perr := uuid.Parse(id)
		if perr != nil {
			return database.WrapInfra(perr)
		}
		ids = append(ids, rid)
	}
	err = acquisitiondb.New(tx).MarkEnrichmentAttempted(ctx, acquisitiondb.MarkEnrichmentAttemptedParams{
		At:       pgtype.Timestamptz{Time: at, Valid: true},
		TenantID: tid,
		Ids:      ids,
	})
	return database.WrapInfra(err)
}

// GetImportEnrichmentCounter reads the ENRICHMENT row's counters off an import: the
// applied-enrichment count (court_records_updated) and the REAL parse-error count (errors),
// which is what the fecho uses to decide OK vs PARTIAL. found=false (pgx.ErrNoRows) means the
// import applied no enrichment — the close use case then records an honest empty terminal row.
func (r *pgRepository) GetImportEnrichmentCounter(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (updated, errs int, found bool, err error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, 0, false, database.WrapInfra(err)
	}
	bid, err := uuid.Parse(backfillJobID)
	if err != nil {
		return 0, 0, false, database.WrapInfra(err)
	}
	row, err := acquisitiondb.New(tx).GetImportEnrichmentCounter(ctx, acquisitiondb.GetImportEnrichmentCounterParams{
		TenantID:      tid,
		BackfillJobID: pgtype.UUID{Bytes: bid, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, database.WrapInfra(err)
	}
	return int(row.CourtRecordsUpdated), int(row.Errors), true, nil
}

// CountImportDiscoveredRecords sums the processes an import discovered
// (SUM(sync_run.court_records_new)) — the coverage total the ETA close compares the
// applied-enrichment counter against.
func (r *pgRepository) CountImportDiscoveredRecords(ctx context.Context, tx database.Tx, tenantID, backfillJobID string) (int, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	bid, err := uuid.Parse(backfillJobID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	total, err := acquisitiondb.New(tx).CountImportDiscoveredRecords(ctx, acquisitiondb.CountImportDiscoveredRecordsParams{
		TenantID:      tid,
		BackfillJobID: pgtype.UUID{Bytes: bid, Valid: true},
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return int(total), nil
}

// CloseImportEnrichmentRun stamps the terminal status (OK/PARTIAL) + finished_at on the
// import's ENRICHMENT row. Returns the number of rows affected: 0 means the import applied
// no enrichment (no row exists), which the use case handles via InsertEmptyImportEnrichmentRun.
func (r *pgRepository) CloseImportEnrichmentRun(ctx context.Context, tx database.Tx, params CloseEnrichmentRunParams) (int, error) {
	tid, err := uuid.Parse(params.TenantID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	bid, err := uuid.Parse(params.BackfillJobID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	rows, err := acquisitiondb.New(tx).CloseImportEnrichmentRun(ctx, acquisitiondb.CloseImportEnrichmentRunParams{
		Status:        params.Status,
		At:            pgtype.Timestamptz{Time: params.At, Valid: true},
		TenantID:      tid,
		BackfillJobID: pgtype.UUID{Bytes: bid, Valid: true},
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return int(rows), nil
}

// InsertEmptyImportEnrichmentRun creates the terminal ENRICHMENT row for an import that
// applied no enrichment (court_records_updated=0, status stamped by the close). ON CONFLICT
// DO NOTHING guards the race with a late application landing between the close UPDATE and
// this INSERT.
func (r *pgRepository) InsertEmptyImportEnrichmentRun(ctx context.Context, tx database.Tx, params CloseEnrichmentRunParams) error {
	tid, err := uuid.Parse(params.TenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	bid, err := uuid.Parse(params.BackfillJobID)
	if err != nil {
		return database.WrapInfra(err)
	}
	err = acquisitiondb.New(tx).InsertEmptyImportEnrichmentRun(ctx, acquisitiondb.InsertEmptyImportEnrichmentRunParams{
		TenantID:      tid,
		BackfillJobID: pgtype.UUID{Bytes: bid, Valid: true},
		At:            pgtype.Timestamptz{Time: params.At, Valid: true},
		Status:        params.Status,
	})
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// FindOrCreateCourtRecord resolves a court record by its natural key inside the
// caller's tx, creating the case+record on a miss, then marks it synced
// (completeness + next_sync_at) whether new or found. It returns the entity the
// cycle keys its docket/intimation upserts and observed events on.
func (r *pgRepository) FindOrCreateCourtRecord(ctx context.Context, tx database.Tx, params FindOrCreateCourtRecordParams) (*CourtRecord, bool, error) {
	tid, err := uuid.Parse(params.TenantID)
	if err != nil {
		return nil, false, database.WrapInfra(err)
	}
	q := acquisitiondb.New(tx)

	row, err := q.GetCourtRecordByKey(ctx, acquisitiondb.GetCourtRecordByKeyParams{
		TenantID:  tid,
		CnjNumber: params.CNJNumber,
		Degree:    params.Degree,
	})
	switch {
	case err == nil:
		// HIT — reobservation of a record already tracked. Never gated: mark it synced
		// and return created=false (the sync_run_id/count belong to its first discoverer).
		if merr := r.markCourtRecordSynced(ctx, q, row.ID, params); merr != nil {
			return nil, false, merr
		}
		return newCourtRecordEntity(row.ID, row.CaseID, params), false, nil
	case errors.Is(err, pgx.ErrNoRows):
		// MISS — a brand-new process. This is the ONLY place the billing entitlement
		// gates a create: refuse with ErrProcessLimitReached when the tenant is already
		// at its active_process_limit, in the same tx as the pending INSERT.
		if gerr := r.enforceProcessLimit(ctx, q, tid, params.ActiveProcessLimit); gerr != nil {
			return nil, false, gerr
		}
		return r.createCourtRecord(ctx, q, tid, params)
	default:
		return nil, false, database.WrapInfra(err)
	}
}

// enforceProcessLimit counts the tenant's ACTIVE court records in the caller's tx and
// returns the typed ErrProcessLimitReached when that count already meets the limit —
// so the pending create would push the tenant over its plan ceiling. The repo only
// compares numbers; the use case owns where the limit comes from (the billing
// entitlement). A count query fault is a plain infra error.
func (r *pgRepository) enforceProcessLimit(ctx context.Context, q *acquisitiondb.Queries, tenantID uuid.UUID, limit int) error {
	count, err := q.CountActiveCourtRecordsByTenant(ctx, tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	if count >= int64(limit) {
		return ErrProcessLimitReached
	}
	return nil
}

// createCourtRecord seeds a fresh case + record (v0 has no consolidation, so one
// case per record) and marks it synced.
// createCourtRecord seeds a fresh case + record (v0 has no consolidation, so one
// case per record) and marks it synced. Idempotent under the concurrent-discovery
// race: if a rival window won the insert (ON CONFLICT DO NOTHING → no id back), it
// drops the orphan case, re-resolves the winner, marks it synced and reports
// created=false — so no 23505 rollback and the new-count is not double-attributed.
func (r *pgRepository) createCourtRecord(ctx context.Context, q *acquisitiondb.Queries, tenantID uuid.UUID, params FindOrCreateCourtRecordParams) (*CourtRecord, bool, error) {
	caseID, err := q.InsertCourtCase(ctx, tenantID)
	if err != nil {
		return nil, false, database.WrapInfra(err)
	}
	recID, err := q.InsertCourtRecord(ctx, acquisitiondb.InsertCourtRecordParams{
		TenantID:     tenantID,
		CaseID:       caseID,
		CnjNumber:    params.CNJNumber,
		Degree:       params.Degree,
		Court:        params.Court,
		Class:        nullString(params.Class),
		Subject:      nullString(params.Subject),
		Completeness: params.Completeness,
		JudgingBody:  nullString(params.JudgingBody),
		SyncRunID:    nullUUID(params.SyncRunID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Lost the create race — a concurrent window inserted this record first. Drop
		// our now-orphan case and re-resolve the winner (mark synced, created=false).
		if derr := q.DeleteCourtCase(ctx, caseID); derr != nil {
			return nil, false, database.WrapInfra(derr)
		}
		row, gerr := q.GetCourtRecordByKey(ctx, acquisitiondb.GetCourtRecordByKeyParams{
			TenantID:  tenantID,
			CnjNumber: params.CNJNumber,
			Degree:    params.Degree,
		})
		if gerr != nil {
			return nil, false, database.WrapInfra(gerr)
		}
		if merr := r.markCourtRecordSynced(ctx, q, row.ID, params); merr != nil {
			return nil, false, merr
		}
		return newCourtRecordEntity(row.ID, row.CaseID, params), false, nil
	}
	if err != nil {
		return nil, false, database.WrapInfra(err)
	}
	if err := r.markCourtRecordSynced(ctx, q, recID, params); err != nil {
		return nil, false, err
	}
	return newCourtRecordEntity(recID, caseID, params), true, nil
}

// markCourtRecordSynced writes the post-sync completeness and next-sweep time.
func (r *pgRepository) markCourtRecordSynced(ctx context.Context, q *acquisitiondb.Queries, id uuid.UUID, params FindOrCreateCourtRecordParams) error {
	err := q.MarkCourtRecordSynced(ctx, acquisitiondb.MarkCourtRecordSyncedParams{
		ID:           id,
		Completeness: params.Completeness,
		NextSyncAt:   pgtype.Timestamptz{Time: params.NextSyncAt, Valid: true},
		JudgingBody:  nullString(params.JudgingBody),
	})
	return database.WrapInfra(err)
}

// UpsertDocketEntries inserts each andamento ON CONFLICT DO NOTHING inside the
// caller's tx. A conflict returns pgx.ErrNoRows (the row already existed), which
// is folded into "deduped"; the returned slice is the entries that were ACTUALLY
// new (each with its assigned id), so the use case emits an observed event only
// for those and tallies new vs. deduped.
// UpsertDocketEntries is the SET-BASED bulk insert — see repository_batch.go.

// UpsertIntimations inserts-or-updates each intimação ON CONFLICT DO UPDATE inside
// the caller's tx and returns the rows event-worthy for the producer (see the
// SET-BASED impl in repository_batch.go): the FIRST inserts (xmax = 0 → observed)
// and the ACTIVE → CANCELLED transitions (→ cancelled). The DO UPDATE always returns
// a row (unlike the docket entries' DO NOTHING), so newness is read from the query's
// `inserted` flag rather than a pgx.ErrNoRows miss; a retraction updates the row in
// place (so the deadline slice can revoke its prazo).
// AcquireTenantWriteLock takes the per-tenant advisory lock inside the caller's
// tx so concurrent sync/enrichment write transactions serialize their court_record
// writes (no 40P01 deadlock) while their fetches still overlap. Released at tx end.
func (r *pgRepository) AcquireTenantWriteLock(ctx context.Context, tx database.Tx, tenantID string) error {
	q := acquisitiondb.New(tx)
	if err := q.AcquireTenantWriteLock(ctx, tenantID); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// InsertPublications lands national DJEN communications in the firehose store inside
// the caller's tx, idempotent on the DJEN hash (ON CONFLICT DO NOTHING): a re-ingested
// day (the daily lookback) is a no-op. Returns how many rows were ACTUALLY new — a
// conflict yields pgx.ErrNoRows for that row, read as already-ingested. The store is
// national reference data (no tenant scope), so the tx needs no tenant RLS.
func (r *pgRepository) InsertPublications(ctx context.Context, tx database.Tx, params []PublicationParams) (int, error) {
	q := acquisitiondb.New(tx)
	newCount := 0
	for _, p := range params {
		oabs := p.RecipientOABs
		if oabs == nil {
			oabs = []string{} // NOT NULL text[]: a no-recipient item lands with an empty set
		}
		payload := []byte(p.Payload)
		if len(payload) == 0 {
			payload = []byte("{}") // NOT NULL jsonb guard
		}
		_, err := q.InsertPublication(ctx, acquisitiondb.InsertPublicationParams{
			Hash:            p.Hash,
			Court:           p.Court,
			CnjNumber:       p.CNJNumber,
			MadeAvailableAt: pgtype.Date{Time: p.MadeAvailableAt, Valid: true},
			RecipientOabs:   oabs,
			Payload:         payload,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue // already ingested — deduped, not new
		}
		if err != nil {
			return 0, database.WrapInfra(err)
		}
		newCount++
	}
	return newCount, nil
}

// ReplaceWatchedOABs sets an integration's watched OAB set inside the caller's tx by
// clearing it and re-inserting the given normalized keys — so a scope change (a
// removed OAB) stops matching. Idempotent per key. Runs in the tenant's tx (RLS by
// app.tenant_id), driven by the integration_activated event.
// MatchPublicationsByDay returns every (tenant, matched OAB, payload) for a day's
// publications, joining the firehose against watched_oab. Runs inside a system tx
// (uow.DoSystem) so the watched_oab RLS opens across tenants.
func (r *pgRepository) MatchPublicationsByDay(ctx context.Context, tx database.Tx, day time.Time) ([]PublicationMatch, error) {
	q := acquisitiondb.New(tx)
	rows, err := q.MatchPublicationsByDay(ctx, pgtype.Date{Time: day, Valid: true})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]PublicationMatch, 0, len(rows))
	for _, row := range rows {
		out = append(out, PublicationMatch{
			TenantID: row.TenantID.String(),
			OABKey:   row.OabKey,
			Payload:  row.Payload,
		})
	}
	return out, nil
}

// MatchPublicationsForTenantSince returns one tenant's matches from a date forward —
// the onboarding history catch-up against the already-stored firehose. System tx.
func (r *pgRepository) MatchPublicationsForTenantSince(ctx context.Context, tx database.Tx, tenantID string, since time.Time) ([]PublicationMatch, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	q := acquisitiondb.New(tx)
	rows, err := q.MatchPublicationsForTenantSince(ctx, acquisitiondb.MatchPublicationsForTenantSinceParams{
		TenantID:        tid,
		MadeAvailableAt: pgtype.Date{Time: since, Valid: true},
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]PublicationMatch, 0, len(rows))
	for _, row := range rows {
		out = append(out, PublicationMatch{TenantID: tenantID, OABKey: row.OabKey, Payload: row.Payload})
	}
	return out, nil
}

func (r *pgRepository) ReplaceWatchedOABs(ctx context.Context, tx database.Tx, tenantID, integrationID string, oabKeys []string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	iid, err := uuid.Parse(integrationID)
	if err != nil {
		return database.WrapInfra(err)
	}
	q := acquisitiondb.New(tx)
	if err := q.DeleteWatchedOABsByIntegration(ctx, iid); err != nil {
		return database.WrapInfra(err)
	}
	for _, key := range oabKeys {
		if err := q.InsertWatchedOAB(ctx, acquisitiondb.InsertWatchedOABParams{
			TenantID: tid, IntegrationID: iid, OabKey: key,
		}); err != nil {
			return database.WrapInfra(err)
		}
	}
	return nil
}

// UpsertIntimations is the SET-BASED bulk upsert — see repository_batch.go.

// GetCourtRecordByKey resolves a court record by its natural key (tenant, cnj,
// degree) inside the caller's tx, returning found=false (not an error) on a miss so
// the enrichment can branch on "does a graded record already hold this grade?" (the
// merge guard for the (tenant, cnj, degree) UNIQUE). Scoped by tenant_id (RLS
// barrier 1).
func (r *pgRepository) GetCourtRecordByKey(ctx context.Context, tx database.Tx, tenantID, cnjNumber, degree string) (*CourtRecord, bool, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, false, database.WrapInfra(err)
	}
	row, err := acquisitiondb.New(tx).GetCourtRecordByKey(ctx, acquisitiondb.GetCourtRecordByKeyParams{
		TenantID:  tid,
		CnjNumber: cnjNumber,
		Degree:    degree,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, database.WrapInfra(err)
	}
	return &CourtRecord{
		ID:        row.ID.String(),
		TenantID:  tenantID,
		CaseID:    row.CaseID.String(),
		CNJNumber: cnjNumber,
		Degree:    degree,
	}, true, nil
}

// UpdateCourtRecordGrade grades a court_record IN PLACE inside the caller's tx: it
// mutates the row named by params.CourtRecordID — the DJEN UNKNOWN placeholder — to
// the DATAJUD-revealed degree and authoritative fields, keeping the SAME id so every
// child already anchored to it stays correct (FIX B). It is NOT entitlement-gated
// (the process already counted against the plan as a placeholder). A miss (the row
// vanished under RLS or was deleted) is the typed ErrCourtRecordNotFound, never
// (nil, nil).
func (r *pgRepository) UpdateCourtRecordGrade(ctx context.Context, tx database.Tx, params GradeParams) (*CourtRecord, error) {
	tid, err := uuid.Parse(params.TenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rid, err := uuid.Parse(params.CourtRecordID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	row, err := acquisitiondb.New(tx).UpdateCourtRecordGrade(ctx, acquisitiondb.UpdateCourtRecordGradeParams{
		CourtRecordID: rid,
		TenantID:      tid,
		Degree:        params.Degree,
		Court:         params.Court,
		Class:         nullString(params.Class),
		Subject:       nullString(params.Subject),
		JudgingBody:   nullString(params.JudgingBody),
		FiledAt:       nullDate(params.FiledAt),
		Secrecy:       params.Secrecy,
		Completeness:  params.Completeness,
		NextSyncAt:    pgtype.Timestamptz{Time: params.NextSyncAt, Valid: !params.NextSyncAt.IsZero()},
		// Empty string when graded.Lifecycle is unset: the SQL NULLIF('')→NULL and
		// COALESCE falls back to the existing lifecycle column value (preserves SUPERSEDED).
		Lifecycle: params.Lifecycle,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCourtRecordNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return &CourtRecord{
		ID:       row.ID.String(),
		TenantID: params.TenantID,
		CaseID:   row.CaseID.String(),
		Degree:   params.Degree,
		Court:    params.Court,
	}, nil
}

// RepointIntimations moves the placeholder's intimations onto the graded record
// inside the caller's tx and returns how many moved. Scoped by tenant_id (RLS
// barrier 1); dedup (tenant, case_id, hash) is unaffected by the court_record_id swap.
func (r *pgRepository) RepointIntimations(ctx context.Context, tx database.Tx, tenantID, fromRecordID, toRecordID string) (int, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	from, err := uuid.Parse(fromRecordID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	to, err := uuid.Parse(toRecordID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	moved, err := acquisitiondb.New(tx).RepointIntimations(ctx, acquisitiondb.RepointIntimationsParams{
		TenantID:          tid,
		FromCourtRecordID: from,
		ToCourtRecordID:   to,
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return int(moved), nil
}

// SupersedeCourtRecord retires the UNKNOWN placeholder inside the caller's tx,
// scoped by tenant_id. It is a no-op if the row is invisible (RLS) or gone.
func (r *pgRepository) SupersedeCourtRecord(ctx context.Context, tx database.Tx, tenantID, recordID string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	rid, err := uuid.Parse(recordID)
	if err != nil {
		return database.WrapInfra(err)
	}
	err = acquisitiondb.New(tx).SupersedeCourtRecord(ctx, acquisitiondb.SupersedeCourtRecordParams{
		TenantID: tid,
		ID:       rid,
	})
	return database.WrapInfra(err)
}

// DueCourtRecordsForResync reads the ACTIVE records whose next_sync_at is due,
// across every tenant, inside the caller's SYSTEM tx (app.system='on'). The
// scheduler runs it under DoSystem; the partial next_sync_at index serves it.
func (r *pgRepository) DueCourtRecordsForResync(ctx context.Context, tx database.Tx, limit int) ([]DueRecord, error) {
	rows, err := acquisitiondb.New(tx).DueCourtRecordsForResync(ctx, int32(limit))
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]DueRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, DueRecord{
			ID:        row.ID.String(),
			TenantID:  row.TenantID.String(),
			CaseID:    row.CaseID.String(),
			CNJNumber: row.CnjNumber,
			Degree:    row.Degree,
			Court:     row.Court,
		})
	}
	return out, nil
}

// ClaimCourtRecordResync pushes a record's next_sync_at forward inside the caller's
// SYSTEM tx as its re-poll is enqueued, so a later tick does not re-enqueue it.
func (r *pgRepository) ClaimCourtRecordResync(ctx context.Context, tx database.Tx, recordID string, nextSyncAt time.Time) error {
	rid, err := uuid.Parse(recordID)
	if err != nil {
		return database.WrapInfra(err)
	}
	err = acquisitiondb.New(tx).ClaimCourtRecordResync(ctx, acquisitiondb.ClaimCourtRecordResyncParams{
		ID:         rid,
		NextSyncAt: pgtype.Timestamptz{Time: nextSyncAt, Valid: true},
	})
	return database.WrapInfra(err)
}

// ResolveCaseIDByCourtRecord resolves the court_case behind a court_record inside the
// caller's tx, scoped by tenant_id (barrier 1, RLS barrier 2). The FE addresses a process
// by its court_record id, but the responsável lives on court_case, so the write hops
// record → case first. A miss — unknown id or a foreign tenant's record — is the typed
// ErrProcessoNotFound (→ 404), never (nil, nil).
func (r *pgRepository) ResolveCaseIDByCourtRecord(ctx context.Context, tx database.Tx, tenantID, courtRecordID string) (string, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return "", database.WrapInfra(err)
	}
	rid, err := uuid.Parse(courtRecordID)
	if err != nil {
		return "", apperr.NewInvalid("id de processo inválido")
	}
	caseID, err := acquisitiondb.New(tx).GetCaseIDByCourtRecord(ctx, acquisitiondb.GetCaseIDByCourtRecordParams{
		ID:       rid,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrProcessoNotFound
	}
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return caseID.String(), nil
}

// AppUserInTenant reports whether appUserID is an app_user of tenantID, inside the
// caller's tx. It is the membership guard for the assign write (only consulted for a
// non-null user_id); a non-uuid user id is client input → the typed KindInvalid.
func (r *pgRepository) AppUserInTenant(ctx context.Context, tx database.Tx, tenantID, appUserID string) (bool, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return false, database.WrapInfra(err)
	}
	uid, err := uuid.Parse(appUserID)
	if err != nil {
		return false, apperr.NewInvalid("id de usuário inválido")
	}
	present, err := acquisitiondb.New(tx).AppUserExistsInTenant(ctx, acquisitiondb.AppUserExistsInTenantParams{
		ID:       uid,
		TenantID: tid,
	})
	if err != nil {
		return false, database.WrapInfra(err)
	}
	return present, nil
}

// AssignCaseResponsible sets (or clears, when assignedUserID is nil) the responsável on a
// court_case inside the caller's tx, scoped by tenant_id (barrier 1, RLS barrier 2). The
// caseID is the one ResolveCaseIDByCourtRecord returned; the WHERE ties the write to the
// caller's own tenant so a mismatched (case, tenant) touches nothing.
func (r *pgRepository) AssignCaseResponsible(ctx context.Context, tx database.Tx, tenantID, caseID string, assignedUserID *string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	cid, err := uuid.Parse(caseID)
	if err != nil {
		return database.WrapInfra(err)
	}

	// A nil user_id is desatribuir → SQL NULL; a non-nil one is validated (parsed) here,
	// though AppUserInTenant already resolved it upstream.
	var assigned pgtype.UUID
	if assignedUserID != nil {
		uid, err := uuid.Parse(*assignedUserID)
		if err != nil {
			return apperr.NewInvalid("id de usuário inválido")
		}
		assigned = pgtype.UUID{Bytes: uid, Valid: true}
	}

	err = acquisitiondb.New(tx).AssignCaseResponsible(ctx, acquisitiondb.AssignCaseResponsibleParams{
		ID:             cid,
		AssignedUserID: assigned,
		TenantID:       tid,
	})
	return database.WrapInfra(err)
}

// ListProcessos reads the tenant's live processes (keyset-paginated) on the pool,
// filtered by tenant_id (isolation barrier 1) like the other screen reads. The
// caller passes a sentinel cursor for the first page.
func (r *pgRepository) ListProcessos(ctx context.Context, q ProcessosQuery) ([]ProcessoView, error) {
	tid, err := uuid.Parse(q.TenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	lastID, err := uuid.Parse(q.LastID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListProcessos(ctx, acquisitiondb.ListProcessosParams{
		TenantID:   tid,
		Limit:      int32(q.Limit),
		Search:     escapeLike(q.Search),
		Court:      q.Court,
		Degree:     q.Degree,
		Lifecycle:  q.Lifecycle,
		AssigneeID: nullUUID(q.Assignee),
		LastCnj:    q.LastCNJ,
		LastID:     lastID,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]ProcessoView, 0, len(rows))
	for _, row := range rows {
		out = append(out, ProcessoView{
			ID:               row.ID.String(),
			CaseID:           row.CaseID.String(),
			CNJNumber:        row.CnjNumber,
			Court:            row.Court,
			Degree:           row.Degree,
			Class:            deref(row.Class),
			Subject:          deref(row.Subject),
			JudgingBody:      deref(row.JudgingBody),
			FiledAt:          datePtr(row.FiledAt),
			Secrecy:          row.Secrecy,
			Lifecycle:        row.Lifecycle,
			Completeness:     row.Completeness,
			ClaimValue:       numericStr(row.ClaimValue),
			AssignedUserID:   uuidStrPtr(row.AssignedUserID),
			AssignedUserName: row.AssignedUserName,
			LastMovementText: row.LastMovementText,
			LastMovementAt:   timestampPtr(row.LastMovementAt),
			NextDeadline:     mapNextDeadline(row.NextDeadlineEndDate, row.NextDeadlineDaysLeft, row.NextDeadlineKind),
		})
	}
	return out, nil
}

// GetProcesso reads one process by id on the pool for the FE deep-link, scoped by
// tenant_id (isolation barrier 1). It mirrors the ListProcessos row→ProcessoView
// mapping (sqlc gives each query its own row type, so the projection maps in place). A
// non-uuid :id is client input → the typed KindInvalid (→ 400); a miss — or a foreign
// tenant's row — is the typed ErrProcessoNotFound (→ 404), never (nil, nil).
func (r *pgRepository) GetProcesso(ctx context.Context, tenantID, id string) (ProcessoView, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return ProcessoView{}, database.WrapInfra(err)
	}
	pid, err := uuid.Parse(id)
	if err != nil {
		return ProcessoView{}, apperr.NewInvalid("id de processo inválido")
	}
	row, err := r.q.GetProcesso(ctx, acquisitiondb.GetProcessoParams{ID: pid, TenantID: tid})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProcessoView{}, ErrProcessoNotFound
	}
	if err != nil {
		return ProcessoView{}, database.WrapInfra(err)
	}
	return ProcessoView{
		ID:               row.ID.String(),
		CaseID:           row.CaseID.String(),
		CNJNumber:        row.CnjNumber,
		Court:            row.Court,
		Degree:           row.Degree,
		Class:            deref(row.Class),
		Subject:          deref(row.Subject),
		JudgingBody:      deref(row.JudgingBody),
		FiledAt:          datePtr(row.FiledAt),
		Secrecy:          row.Secrecy,
		Lifecycle:        row.Lifecycle,
		Completeness:     row.Completeness,
		ClaimValue:       numericStr(row.ClaimValue),
		AssignedUserID:   uuidStrPtr(row.AssignedUserID),
		AssignedUserName: row.AssignedUserName,
		LastMovementText: row.LastMovementText,
		LastMovementAt:   timestampPtr(row.LastMovementAt),
		NextDeadline:     mapNextDeadline(row.NextDeadlineEndDate, row.NextDeadlineDaysLeft, row.NextDeadlineKind),
	}, nil
}

// ListIntimacoes reads the tenant's intimation inbox (keyset-paginated, newest
// availability first) on the pool, filtered by tenant_id.
func (r *pgRepository) ListIntimacoes(ctx context.Context, q IntimacoesQuery) ([]IntimacaoView, error) {
	tid, err := uuid.Parse(q.TenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	lastID, err := uuid.Parse(q.LastID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	lastMade, err := time.Parse(time.DateOnly, q.LastMadeAvailable)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListIntimacoes(ctx, acquisitiondb.ListIntimacoesParams{
		TenantID:          tid,
		Limit:             int32(q.Limit),
		Search:            escapeLike(q.Search),
		Type:              q.Type,
		UserStatus:        q.UserStatus,
		Court:             q.Court,
		Urgencia:          q.Urgencia,
		LastMadeAvailable: pgtype.Date{Time: lastMade, Valid: true},
		LastID:            lastID,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]IntimacaoView, 0, len(rows))
	for _, row := range rows {
		out = append(out, IntimacaoView{
			ID:              row.ID.String(),
			CNJNumber:       row.CnjNumber,
			Class:           deref(row.Class),
			CourtRecordID:   row.CourtRecordID.String(),
			Court:           row.Court,
			Degree:          row.Degree,
			Type:            deref(row.Type),
			Status:          row.Status,
			UserStatus:      row.UserStatus,
			Source:          row.Source,
			SourceURL:       deref(row.SourceUrl),
			MadeAvailableAt: row.MadeAvailableAt.Time,
			PublishedAt:     row.PublishedAt.Time,
			DeadlineStartAt: row.DeadlineStartAt.Time,
			ContentPreview:  contentPreview(row.Content),
			Prazo:           mapPrazo(row.PrazoDeadlineID, row.PrazoEndDate, row.PrazoDaysLeft, row.PrazoStatus, row.PrazoConfirmed),
		})
	}
	return out, nil
}

// GetIntimacao reads one intimation by id on the pool for the FE deep-link, scoped by
// tenant_id (isolation barrier 1). It mirrors the ListIntimacoes row→IntimacaoView
// mapping (sqlc gives each query its own row type, so the projection maps in place). A
// non-uuid :id is client input → the typed KindInvalid (→ 400); a miss — or a foreign
// tenant's row — is the typed ErrIntimationNotFound (→ 404), never (nil, nil).
func (r *pgRepository) GetIntimacao(ctx context.Context, tenantID, id string) (IntimacaoDetailView, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return IntimacaoDetailView{}, database.WrapInfra(err)
	}
	iid, err := uuid.Parse(id)
	if err != nil {
		return IntimacaoDetailView{}, apperr.NewInvalid("id de intimação inválido")
	}
	row, err := r.q.GetIntimacao(ctx, acquisitiondb.GetIntimacaoParams{ID: iid, TenantID: tid})
	if errors.Is(err, pgx.ErrNoRows) {
		return IntimacaoDetailView{}, ErrIntimationNotFound
	}
	if err != nil {
		return IntimacaoDetailView{}, database.WrapInfra(err)
	}
	conductorID := uuidPtrFromPgtype(row.ConductorUserID)
	reviewerID := uuidPtrFromPgtype(row.ReviewerUserID)
	history := buildIntimacaoHistory(row.MadeAvailableAt, row.PrazoConfirmedAt, row.PrazoConfirmedByName, row.PrazoStatus, row.AiAnalyzedAt)

	return IntimacaoDetailView{
		IntimacaoView: IntimacaoView{
			ID:              row.ID.String(),
			CNJNumber:       row.CnjNumber,
			Class:           deref(row.Class),
			CourtRecordID:   row.CourtRecordID.String(),
			Court:           row.Court,
			Degree:          row.Degree,
			Type:            deref(row.Type),
			Status:          row.Status,
			UserStatus:      row.UserStatus,
			Source:          row.Source,
			SourceURL:       deref(row.SourceUrl),
			MadeAvailableAt: row.MadeAvailableAt.Time,
			PublishedAt:     row.PublishedAt.Time,
			DeadlineStartAt: row.DeadlineStartAt.Time,
			// The detail carries the FULL teor below, but ContentPreview stays populated so
			// the embedded IntimacaoView is a complete list row (nothing the FE reads breaks).
			ContentPreview: contentPreview(row.Content),
			Prazo:          mapPrazo(row.PrazoDeadlineID, row.PrazoEndDate, row.PrazoDaysLeft, row.PrazoStatus, row.PrazoConfirmed),
		},
		Content:           row.Content,
		JudgingBody:       deref(row.JudgingBody),
		Recipients:        rawArrayOrEmpty(json.RawMessage(row.Recipients)),
		ConductorUserID:   conductorID,
		ConductorUserName: row.ConductorUserName,
		ReviewerUserID:    reviewerID,
		ReviewerUserName:  row.ReviewerUserName,
		History:           history,
		AISummary:         deref(row.AiSummary),
		AIProvidencias:    decodeProvidencias(row.AiProvidencias),
		AIAnalyzedAt:      timestampPtr(row.AiAnalyzedAt),
	}, nil
}

// decodeProvidencias unmarshals the intimation.ai_providencias jsonb into the wire slice.
// A NULL/empty column (pré-análise) or a malformed value yields an initialized empty slice
// (never null) so the FE always maps over an array.
func decodeProvidencias(raw []byte) []IntimacaoProvidenciaView {
	out := []IntimacaoProvidenciaView{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return []IntimacaoProvidenciaView{}
	}
	return out
}

// GetIntimacaoAnaliseContext reads one intimation's teor + its court record identification
// for the AI analysis prompt (POST /v1/intimacoes/:id/analise). A non-uuid :id is client
// input → the typed KindInvalid (→ 400); a miss — or a foreign tenant's row — is the typed
// ErrIntimationNotFound (→ 404), never (nil, nil).
func (r *pgRepository) GetIntimacaoAnaliseContext(ctx context.Context, tenantID, intimationID string) (IntimacaoAnaliseCtx, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return IntimacaoAnaliseCtx{}, database.WrapInfra(err)
	}
	iid, err := uuid.Parse(intimationID)
	if err != nil {
		return IntimacaoAnaliseCtx{}, apperr.NewInvalid("id de intimação inválido")
	}
	row, err := r.q.GetIntimacaoAnaliseContext(ctx, acquisitiondb.GetIntimacaoAnaliseContextParams{ID: iid, TenantID: tid})
	if errors.Is(err, pgx.ErrNoRows) {
		return IntimacaoAnaliseCtx{}, ErrIntimationNotFound
	}
	if err != nil {
		return IntimacaoAnaliseCtx{}, database.WrapInfra(err)
	}
	endDate := ""
	if row.DeadlineEndDate.Valid {
		endDate = row.DeadlineEndDate.Time.Format(dateLayout)
	}

	members, err := r.ListActiveMembers(ctx, tenantID)
	if err != nil {
		return IntimacaoAnaliseCtx{}, err
	}

	return IntimacaoAnaliseCtx{
		Content:         row.Content,
		Type:            row.Type,
		CNJNumber:       row.CnjNumber,
		Court:           row.Court,
		Degree:          row.Degree,
		Class:           deref(row.Class),
		Subject:         deref(row.Subject),
		DeadlineEndDate: endDate,
		Members:         members,
	}, nil
}

// ListActiveMembers reads the tenant's ACTIVE firm members (internal app_user id + name) on
// the pool — the assignable responsáveis the analyze_intimation prompt lists. A malformed
// tenant id is infra-wrapped; the empty tenant yields an empty slice (never nil).
func (r *pgRepository) ListActiveMembers(ctx context.Context, tenantID string) ([]MemberCtx, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListActiveMembers(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]MemberCtx, 0, len(rows))
	for _, row := range rows {
		out = append(out, MemberCtx{UserID: row.ID.String(), Name: deref(row.Name)})
	}
	return out, nil
}

// ListAndamentosByProcesso reads one process's docket entries (keyset-paginated,
// newest first) on the pool. docket_entry has no tenant_id, so tenant isolation
// (barrier 1) runs through the court_record join: a court_record.id belonging to
// another tenant matches nothing. The caller passes the max sentinel cursor for the
// first page.
func (r *pgRepository) ListAndamentosByProcesso(ctx context.Context, q AndamentosQuery) ([]AndamentoView, error) {
	tid, err := uuid.Parse(q.TenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	crid, err := uuid.Parse(q.CourtRecordID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	lastID, err := uuid.Parse(q.LastID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	lastOccurred, err := time.Parse(time.RFC3339Nano, q.LastOccurred)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListAndamentosByProcesso(ctx, acquisitiondb.ListAndamentosByProcessoParams{
		CourtRecordID: crid,
		TenantID:      tid,
		LastOccurred:  pgtype.Timestamptz{Time: lastOccurred, Valid: true},
		LastID:        lastID,
		PageLimit:     int32(q.Limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]AndamentoView, 0, len(rows))
	for _, row := range rows {
		out = append(out, AndamentoView{
			ID:         row.ID.String(),
			OccurredAt: row.OccurredAt.Time,
			ObservedAt: row.ObservedAt.Time,
			TPUCode:    intPtr(row.TpuCode),
			Text:       row.Text,
			Source:     row.Source,
			Fidelity:   int(row.Fidelity),
		})
	}
	return out, nil
}

// CountAndamentosByProcesso returns the "X de Y" total for the Andamentos tab: how
// many docket entries the process holds. Tenant-scoped through the same
// court_record join as the list.
func (r *pgRepository) CountAndamentosByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	crid, err := uuid.Parse(courtRecordID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	total, err := r.q.CountAndamentosByProcesso(ctx, acquisitiondb.CountAndamentosByProcessoParams{
		CourtRecordID: crid,
		TenantID:      tid,
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return total, nil
}

// ListPartesByProcesso reads the process's parties (behind the court_record) with
// their advogados aggregated per party, on the pool. Tenant isolation (barrier 1) is
// the direct tenant_id filter, and the case is resolved from the court_record inside
// the query (a foreign/unknown :id resolves to no case → no rows). The advogados arrive
// as a jsonb array (pgx scans the jsonb column into []byte for the interface{} target),
// unmarshaled here into the counsel views; an empty array (party with no advogado) maps
// to an initialized empty slice, never nil.
func (r *pgRepository) ListPartesByProcesso(ctx context.Context, tenantID, courtRecordID string) ([]PartyRow, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	crid, err := uuid.Parse(courtRecordID)
	if err != nil {
		// A non-uuid :id is client input → the typed KindInvalid (→ 400), mirroring the
		// other court_record deep-reads.
		return nil, apperr.NewInvalid("id de processo inválido")
	}
	rows, err := r.q.ListPartiesByProcesso(ctx, acquisitiondb.ListPartiesByProcessoParams{
		TenantID:      tid,
		CourtRecordID: crid,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]PartyRow, 0, len(rows))
	for _, row := range rows {
		counsels, cerr := decodeCounsels(row.Counsels)
		if cerr != nil {
			return nil, database.WrapInfra(cerr)
		}
		out = append(out, PartyRow{
			Role: row.Role,
			PartyView: PartyView{
				Name:     row.Name,
				Document: row.Document,
				Counsels: counsels,
			},
		})
	}
	return out, nil
}

// decodeCounsels unmarshals the jsonb_agg advogados column (cast to text in the query,
// so pgx scans a JSON string) into the counsel views. An empty/absent value maps to an
// initialized empty slice so the wire shape is always an array, never null.
func decodeCounsels(raw string) ([]PartyCounselView, error) {
	counsels := []PartyCounselView{}
	if raw == "" {
		return counsels, nil
	}
	if err := json.Unmarshal([]byte(raw), &counsels); err != nil {
		return nil, err
	}
	return counsels, nil
}

// ListIntimacoesByProcesso reads one process's intimations (keyset-paginated, newest
// availability first) on the pool. intimation carries its own tenant_id, so tenant
// isolation (barrier 1) is the direct filter alongside court_record_id; the join only
// supplies the record's number/court/degree. The caller passes the max sentinel cursor
// for the first page.
func (r *pgRepository) ListIntimacoesByProcesso(ctx context.Context, q IntimacoesByProcessoQuery) ([]IntimacaoView, error) {
	tid, err := uuid.Parse(q.TenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	crid, err := uuid.Parse(q.CourtRecordID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	lastID, err := uuid.Parse(q.LastID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	lastMade, err := time.Parse(time.DateOnly, q.LastMadeAvailable)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListIntimacoesByProcesso(ctx, acquisitiondb.ListIntimacoesByProcessoParams{
		CourtRecordID:     crid,
		TenantID:          tid,
		LastMadeAvailable: pgtype.Date{Time: lastMade, Valid: true},
		LastID:            lastID,
		PageLimit:         int32(q.Limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]IntimacaoView, 0, len(rows))
	for _, row := range rows {
		out = append(out, IntimacaoView{
			ID:              row.ID.String(),
			CNJNumber:       row.CnjNumber,
			Class:           deref(row.Class),
			CourtRecordID:   row.CourtRecordID.String(),
			Court:           row.Court,
			Degree:          row.Degree,
			Type:            deref(row.Type),
			Status:          row.Status,
			UserStatus:      row.UserStatus,
			Source:          row.Source,
			SourceURL:       deref(row.SourceUrl),
			MadeAvailableAt: row.MadeAvailableAt.Time,
			PublishedAt:     row.PublishedAt.Time,
			DeadlineStartAt: row.DeadlineStartAt.Time,
			ContentPreview:  contentPreview(row.Content),
		})
	}
	return out, nil
}

// CountIntimacoesByProcesso returns the "X de Y" total for the Intimações tab: how many
// intimations the process holds. Scoped by court_record_id + tenant_id (barrier 1) — no
// join, since intimation carries tenant_id.
func (r *pgRepository) CountIntimacoesByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	crid, err := uuid.Parse(courtRecordID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	total, err := r.q.CountIntimacoesByProcesso(ctx, acquisitiondb.CountIntimacoesByProcessoParams{
		CourtRecordID: crid,
		TenantID:      tid,
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return total, nil
}

// likeEscaper escapes the ILIKE metacharacters so a user-typed % or _ matches
// literally, not as a wildcard. Backslash first (it is the escape char, paired with
// ESCAPE '\' in the queries) so the replacements do not compound.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// escapeLike neutralizes ILIKE wildcards in a search term. An empty term stays empty,
// so the queries keep their empty-search short-circuit (no filter) when the term is blank.
func escapeLike(s string) string {
	return likeEscaper.Replace(s)
}

// CountProcessos returns the processes screen's "X de Y" totals: totalCount is the
// current context (filtered by the active filters when any is present), total the
// tenant's count in the screen's context. With no filter the two are equal, so a
// single COUNT (the reused global) fills both; with a filter the filtered COUNT is
// the second read. The "Y" follows the lifecycle filter (CountCourtRecordsByLifecycle
// when ?lifecycle is set, else the ACTIVE default), so "X de Y" stays meaningful —
// X is always a subset of the lifecycle context. Tenant-scoped (barrier 1).
func (r *pgRepository) CountProcessos(ctx context.Context, q ProcessosQuery) (int64, int64, error) {
	tid, err := uuid.Parse(q.TenantID)
	if err != nil {
		return 0, 0, database.WrapInfra(err)
	}

	total, err := r.countProcessosContext(ctx, tid, q.Lifecycle)
	if err != nil {
		return 0, 0, err
	}
	if !q.Filtered() {
		return total, total, nil
	}
	filtered, err := r.q.CountProcessosFiltered(ctx, acquisitiondb.CountProcessosFilteredParams{
		TenantID:   tid,
		Search:     escapeLike(q.Search),
		Court:      q.Court,
		Degree:     q.Degree,
		Lifecycle:  q.Lifecycle,
		AssigneeID: nullUUID(q.Assignee),
	})
	if err != nil {
		return 0, 0, database.WrapInfra(err)
	}
	return filtered, total, nil
}

// countProcessosContext is the "Y" of the processes counter: the records of the
// selected lifecycle (the ACTIVE default), so a ?lifecycle filter has a matching
// universe instead of a stale ACTIVE total.
func (r *pgRepository) countProcessosContext(ctx context.Context, tid uuid.UUID, lifecycle string) (int64, error) {
	if lifecycle == "" {
		return r.q.CountActiveCourtRecordsByTenant(ctx, tid)
	}
	total, err := r.q.CountCourtRecordsByLifecycle(ctx, acquisitiondb.CountCourtRecordsByLifecycleParams{
		TenantID:  tid,
		Lifecycle: lifecycle,
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return total, nil
}

// CountIntimacoes returns the intimations inbox's "X de Y" totals — the same policy as
// CountProcessos, over the intimation table (filtered by the joined court record's
// cnj_number plus the type/user_status/court filters). Tenant-scoped.
func (r *pgRepository) CountIntimacoes(ctx context.Context, q IntimacoesQuery) (int64, int64, error) {
	tid, err := uuid.Parse(q.TenantID)
	if err != nil {
		return 0, 0, database.WrapInfra(err)
	}
	total, err := r.q.CountIntimationsByTenant(ctx, tid)
	if err != nil {
		return 0, 0, database.WrapInfra(err)
	}
	if !q.Filtered() {
		return total, total, nil
	}
	filtered, err := r.q.CountIntimacoesFiltered(ctx, acquisitiondb.CountIntimacoesFilteredParams{
		TenantID:   tid,
		Search:     escapeLike(q.Search),
		Type:       q.Type,
		UserStatus: q.UserStatus,
		Court:      q.Court,
		Urgencia:   q.Urgencia,
	})
	if err != nil {
		return 0, 0, database.WrapInfra(err)
	}
	return filtered, total, nil
}

// BucketIntimacoes counts intimations per urgência section (Em atraso / Vence
// hoje / Esta semana / Mais adiante / Sem prazo) for the list envelope's `buckets`
// object. It applies the same non-urgência filters (type, user_status, court,
// search) as ListIntimacoes so the section headers agree with the filtered list —
// without an extra full-scan round-trip (one aggregate query).
func (r *pgRepository) BucketIntimacoes(ctx context.Context, q IntimacoesQuery) (IntimacaoBucketsView, error) {
	tid, err := uuid.Parse(q.TenantID)
	if err != nil {
		return IntimacaoBucketsView{}, database.WrapInfra(err)
	}
	row, err := r.q.CountIntimacoesBuckets(ctx, acquisitiondb.CountIntimacoesBucketsParams{
		TenantID:   tid,
		Search:     escapeLike(q.Search),
		Type:       q.Type,
		UserStatus: q.UserStatus,
		Court:      q.Court,
	})
	if err != nil {
		return IntimacaoBucketsView{}, database.WrapInfra(err)
	}
	return IntimacaoBucketsView{
		Atraso:      row.Atraso,
		Hoje:        row.Hoje,
		EstaSemana:  row.EstaSemana,
		MaisAdiante: row.MaisAdiante,
		SemPrazo:    row.SemPrazo,
	}, nil
}

// ListProcessoCourts reads the distinct courts of the tenant's live records (the
// processes screen's ?court options), ordered by name.
func (r *pgRepository) ListProcessoCourts(ctx context.Context, tenantID string) ([]string, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListProcessoCourts(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return rows, nil
}

// ListProcessoDegrees reads the distinct degrees of the tenant's live records (the
// processes screen's ?degree options), ordered by name.
func (r *pgRepository) ListProcessoDegrees(ctx context.Context, tenantID string) ([]string, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListProcessoDegrees(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return rows, nil
}

// ListProcessoAssignees reads the responsáveis of the tenant's live processes (the
// ?assignee options), deduped by id and ordered by name.
func (r *pgRepository) ListProcessoAssignees(ctx context.Context, tenantID string) ([]AssigneeOption, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListProcessoAssignees(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]AssigneeOption, 0, len(rows))
	for _, row := range rows {
		out = append(out, AssigneeOption{Name: row.Name, ID: row.ID.String()})
	}
	return out, nil
}

// ListIntimacaoCourts reads the distinct courts of the tenant's intimated records (the
// inbox's ?court options), ordered by name.
func (r *pgRepository) ListIntimacaoCourts(ctx context.Context, tenantID string) ([]string, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListIntimacaoCourts(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return rows, nil
}

// SummarizeProcessos reads the processes list KPI counts (bucketed by court_record
// lifecycle) on the pool, tenant-scoped. One aggregate scan; off the write path.
func (r *pgRepository) SummarizeProcessos(ctx context.Context, tenantID string) (ProcessosSummaryView, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return ProcessosSummaryView{}, database.WrapInfra(err)
	}
	row, err := r.q.SummarizeProcessos(ctx, tid)
	if err != nil {
		return ProcessosSummaryView{}, database.WrapInfra(err)
	}
	return ProcessosSummaryView{
		Total:       row.Total,
		EmAndamento: row.EmAndamento,
		Suspensos:   row.Suspensos,
		Arquivados:  row.Arquivados,
		Baixados:    row.Baixados,
	}, nil
}

// SummarizeIntimacoes reads the intimações inbox KPI counts (bucketed by triagem
// state, user_status) on the pool, tenant-scoped. One aggregate scan; off the write path.
func (r *pgRepository) SummarizeIntimacoes(ctx context.Context, tenantID string) (IntimacoesSummaryView, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return IntimacoesSummaryView{}, database.WrapInfra(err)
	}
	row, err := r.q.SummarizeIntimacoes(ctx, tid)
	if err != nil {
		return IntimacoesSummaryView{}, database.WrapInfra(err)
	}
	return IntimacoesSummaryView{
		Total:         row.Total,
		Pendentes:     row.Pendentes,
		EmAnalise:     row.EmAnalise,
		Resolvidas:    row.Resolvidas,
		Ignoradas:     row.Ignoradas,
		Criticas:      row.Criticas,
		EmAtraso:      row.EmAtraso,
		VencemHoje:    row.VencemHoje,
		NaoConfirmado: row.NaoConfirmado,
	}, nil
}

// SetIntimationUserStatus sets ONLY the intimation's triagem state (user_status) inside
// the caller's tx, tenant-scoped (barrier 1, RLS barrier 2). The DJEN cancellation
// `status` column is untouched, so a routine re-observation cannot clobber the user's
// decision. A zero-row UPDATE (unknown id or a foreign tenant's row) surfaces as
// pgx.ErrNoRows → ErrIntimationNotFound (→ 404), never a silent no-op.
func (r *pgRepository) SetIntimationUserStatus(ctx context.Context, tx database.Tx, tenantID, intimationID, userStatus string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	iid, err := uuid.Parse(intimationID)
	if err != nil {
		return apperr.NewInvalid("id de intimação inválido")
	}
	_, err = acquisitiondb.New(tx).SetIntimationUserStatus(ctx, acquisitiondb.SetIntimationUserStatusParams{
		ID:         iid,
		TenantID:   tid,
		UserStatus: userStatus,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrIntimationNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// AssignIntimacaoResponsaveis sets (or clears, with nil) the conductor_user_id and
// reviewer_user_id on one intimação inside the caller's tx, tenant-scoped (barrier 1, RLS
// barrier 2). The use case already validated membership; we parse & pass the UUIDs here.
// A zero-row UPDATE surfaces as ErrIntimationNotFound (→ 404), never a silent no-op.
func (r *pgRepository) AssignIntimacaoResponsaveis(ctx context.Context, tx database.Tx, tenantID, intimationID string, conductorUserID, reviewerUserID *string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	iid, err := uuid.Parse(intimationID)
	if err != nil {
		return apperr.NewInvalid("id de intimação inválido")
	}

	toNullableUUID := func(s *string) (pgtype.UUID, error) {
		if s == nil {
			return pgtype.UUID{}, nil
		}
		u, err := uuid.Parse(*s)
		if err != nil {
			return pgtype.UUID{}, apperr.NewInvalid("id de usuário inválido")
		}
		return pgtype.UUID{Bytes: u, Valid: true}, nil
	}

	conductor, err := toNullableUUID(conductorUserID)
	if err != nil {
		return err
	}
	reviewer, err := toNullableUUID(reviewerUserID)
	if err != nil {
		return err
	}

	_, err = acquisitiondb.New(tx).AssignIntimationResponsaveis(ctx, acquisitiondb.AssignIntimationResponsaveisParams{
		ID:              iid,
		TenantID:        tid,
		ConductorUserID: conductor,
		ReviewerUserID:  reviewer,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrIntimationNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// newCourtRecordEntity assembles the CourtRecord the use case works with from the
// resolved ids and the request's natural-key fields.
func newCourtRecordEntity(id, caseID uuid.UUID, params FindOrCreateCourtRecordParams) *CourtRecord {
	return &CourtRecord{
		ID:        id.String(),
		TenantID:  params.TenantID,
		CaseID:    caseID.String(),
		CNJNumber: params.CNJNumber,
		Degree:    params.Degree,
		Court:     params.Court,
	}
}

// nullString maps an empty string to a SQL NULL (nil *string) for the nullable
// text columns; a non-empty value is written as-is.
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nullUUID maps a string id to a nullable pgtype.UUID: empty or unparseable → SQL
// NULL (Valid:false), so an optional lineage FK (sync_run_id, backfill_job_id) is
// written NULL when absent.
func nullUUID(s string) pgtype.UUID {
	if s == "" {
		return pgtype.UUID{}
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

// derefString lifts a nullable text column (*string) to a plain string — empty on
// NULL, for the compact read-model rows.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// intPtr lifts a nullable int column (*int32) to a *int for a read model — nil on
// NULL, so an absent tpu_code serializes as JSON null rather than 0.
func intPtr(n *int32) *int {
	if n == nil {
		return nil
	}
	v := int(*n)
	return &v
}

// dateOrEmpty formats a date column to the wire layout (2006-01-02), empty on NULL.
func dateOrEmpty(d pgtype.Date) string {
	if !d.Valid {
		return ""
	}
	return d.Time.Format(dateLayout)
}

// datePtr / timestampPtr lift a nullable driver date/timestamp to a *time.Time for
// a read model — nil when the column was NULL, so it serializes as JSON null.
func datePtr(d pgtype.Date) *time.Time {
	if !d.Valid {
		return nil
	}
	t := d.Time
	return &t
}

func timestampPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// dateStrPtr formats a nullable date column to the wire layout (2006-01-02) as a
// *string — nil when NULL, so an absent window bound serializes as JSON null (the
// Captures view uses *string windows, unlike the reconciliation view's "" sentinel).
func dateStrPtr(d pgtype.Date) *string {
	if !d.Valid {
		return nil
	}
	s := d.Time.Format(dateLayout)
	return &s
}

// GetResumoContext assembles the full context for the AI process summary (GET
// /v1/processos/:id/resume): the court_record identification row plus the cached
// ai_resume/generated_at, and the three aggregated slices (last 10 andamentos, active
// intimações, open prazos) read as narrow queries. A non-uuid :id is client input →
// the typed KindInvalid (→ 400); a miss — or a foreign tenant's row — is the typed
// ErrProcessoNotFound (→ 404), never (nil, nil).
func (r *pgRepository) GetResumoContext(ctx context.Context, tenantID, courtRecordID string) (ProcessoResumoCtx, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return ProcessoResumoCtx{}, database.WrapInfra(err)
	}
	rid, err := uuid.Parse(courtRecordID)
	if err != nil {
		return ProcessoResumoCtx{}, apperr.NewInvalid("id de processo inválido")
	}

	base, err := r.q.GetResumoContext(ctx, acquisitiondb.GetResumoContextParams{ID: rid, TenantID: tid})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProcessoResumoCtx{}, ErrProcessoNotFound
	}
	if err != nil {
		return ProcessoResumoCtx{}, database.WrapInfra(err)
	}

	movRows, err := r.q.GetResumoMovements(ctx, acquisitiondb.GetResumoMovementsParams{CourtRecordID: rid, TenantID: tid})
	if err != nil {
		return ProcessoResumoCtx{}, database.WrapInfra(err)
	}
	intRows, err := r.q.GetResumoActiveIntimations(ctx, acquisitiondb.GetResumoActiveIntimationsParams{CourtRecordID: rid, TenantID: tid})
	if err != nil {
		return ProcessoResumoCtx{}, database.WrapInfra(err)
	}
	dlRows, err := r.q.GetResumoOpenDeadlines(ctx, acquisitiondb.GetResumoOpenDeadlinesParams{CourtRecordID: rid, TenantID: tid})
	if err != nil {
		return ProcessoResumoCtx{}, database.WrapInfra(err)
	}

	ctxData := ProcessoResumoCtx{
		ID:                  base.ID.String(),
		CNJNumber:           base.CnjNumber,
		Court:               base.Court,
		Degree:              base.Degree,
		Class:               deref(base.Class),
		Subject:             deref(base.Subject),
		ClaimValue:          numericStr(base.ClaimValue),
		Lifecycle:           base.Lifecycle,
		FiledAt:             datePtr(base.FiledAt),
		JudgingBody:         deref(base.JudgingBody),
		AIResume:            base.AiResume,
		AIResumeGeneratedAt: timestampPtr(base.AiResumeGeneratedAt),
		RecentMovements:     make([]RawDocketEntry, 0, len(movRows)),
		ActiveIntimations:   make([]RawIntimation, 0, len(intRows)),
		OpenDeadlines:       make([]RawDeadline, 0, len(dlRows)),
	}
	for _, m := range movRows {
		ctxData.RecentMovements = append(ctxData.RecentMovements, RawDocketEntry{
			OccurredAt: m.OccurredAt.Time,
			Text:       m.Text,
		})
	}
	for _, i := range intRows {
		days := 0
		if i.DeadlineDays != nil {
			days = int(*i.DeadlineDays)
		}
		ctxData.ActiveIntimations = append(ctxData.ActiveIntimations, RawIntimation{
			Type:         deref(i.Type),
			Teor:         i.Teor,
			DeadlineDays: days,
		})
	}
	for _, d := range dlRows {
		ctxData.OpenDeadlines = append(ctxData.OpenDeadlines, RawDeadline{
			Kind:          deref(d.Kind),
			EndDate:       d.EndDate.Time,
			DaysRemaining: int(d.DaysLeft),
			Counting:      d.Counting,
		})
	}
	return ctxData, nil
}

// numericStr lifts a nullable numeric column (claim_value, the valor da causa) to a
// *string for a read model — nil on NULL, so an absent value serializes as JSON null.
// The value is carried as a decimal string (not a float64) to preserve precision; the
// driver's Value() yields that string form.
func numericStr(n pgtype.Numeric) *string {
	if !n.Valid {
		return nil
	}
	v, err := n.Value()
	if err != nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// uuidStrPtr lifts a nullable uuid column (assigned_user_id, the responsável) to a
// *string for a read model — nil on NULL, so an unassigned process serializes the id as
// JSON null rather than the zero uuid.
func uuidStrPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}

// contentPreviewLen bounds the intimation teor preview the inbox list carries; the
// full (often long, HTML) content is a detail-screen concern.
const contentPreviewLen = 500

// contentPreview strips HTML tags, decodes entities, collapses whitespace, and
// then truncates the result to contentPreviewLen runes for the inbox list. The
// full raw HTML content is returned unchanged on the detail endpoint.
func contentPreview(content string) string {
	plain := htmlPlaintext(content)
	runes := []rune(plain)
	if len(runes) <= contentPreviewLen {
		return plain
	}
	return string(runes[:contentPreviewLen])
}

// mapPrazo converts the nullable prazo columns (LEFT JOIN deadline) from the sqlc
// row into a *IntimacaoPrazoView. Returns nil when there is no derived deadline
// (PrazoDeadlineID.Valid is false — the LEFT JOIN missed). PrazoDaysLeft and
// PrazoConfirmed are interface{} (sqlc cannot infer the CASE WHEN expression type),
// so they are decoded via type assertion from pgx's native scan types: int64 for
// the int expression and bool for the boolean expression.
func mapPrazo(
	deadlineID pgtype.UUID,
	endDate pgtype.Date,
	daysLeft interface{},
	status *string,
	confirmed interface{},
) *IntimacaoPrazoView {
	if !deadlineID.Valid {
		return nil
	}
	var days int
	switch v := daysLeft.(type) {
	case int64:
		days = int(v)
	case int32:
		days = int(v)
	}
	var conf bool
	if b, ok := confirmed.(bool); ok {
		conf = b
	}
	return &IntimacaoPrazoView{
		DeadlineID: uuid.UUID(deadlineID.Bytes).String(),
		EndDate:    endDate.Time,
		DaysLeft:   days,
		Status:     deref(status),
		Confirmed:  conf,
	}
}

// mapNextDeadline converts the nullable next_deadline columns (LEFT JOIN LATERAL
// deadline) from the sqlc row into a *NextDeadlineView. Returns nil when there is no
// OPEN/PENDING prazo for the process (LATERAL returned no row — endDate.Valid is
// false). NextDeadlineDaysLeft is interface{} (sqlc cannot infer the CASE WHEN
// expression type), decoded via type assertion from pgx's native int64/int32.
func mapNextDeadline(endDate pgtype.Date, daysLeft interface{}, kind *string) *NextDeadlineView {
	if !endDate.Valid {
		return nil
	}
	var days int
	switch v := daysLeft.(type) {
	case int64:
		days = int(v)
	case int32:
		days = int(v)
	}
	return &NextDeadlineView{
		EndDate:  endDate.Time,
		DaysLeft: days,
		Kind:     deref(kind),
	}
}

// uuidPtrFromPgtype converts a nullable sqlc UUID field to a *string (nil when absent).
// Used to map conductor_user_id / reviewer_user_id from GetIntimacaoRow.
func uuidPtrFromPgtype(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}

// buildIntimacaoHistory assembles the derived timeline for IntimacaoDetailView from the
// fields already fetched by GetIntimacao — no extra round-trip, no new table. The
// function always returns an initialized (non-nil) slice. Entries:
//  1. Capturada: made_available_at (DATE → UTC midnight).
//  2. Prazo confirmado: deadline.confirmed_at, when confirmed_by IS NOT NULL AND
//     status is OPEN (confirmed) or NO_DEADLINE; label includes the confirmer name when
//     available. Absent when there is no deadline or it is not yet confirmed.
//  3. Providências geradas: ai_analyzed_at, when the analysis has run at least once.
//
// Ordered ASC by occurred_at (the three sources don't have a fixed relative order — a
// re-analysis can happen before or after the prazo is confirmed — so the slice is sorted
// after assembly rather than relying on append order).
func buildIntimacaoHistory(
	madeAvailableAt pgtype.Date,
	confirmedAt pgtype.Timestamptz,
	confirmedByName *string,
	deadlineStatus *string,
	aiAnalyzedAt pgtype.Timestamptz,
) []IntimacaoHistoryEntry {
	entries := make([]IntimacaoHistoryEntry, 0, 3)

	// 1. Captura — made_available_at is always present (NOT NULL column).
	if madeAvailableAt.Valid {
		entries = append(entries, IntimacaoHistoryEntry{
			OccurredAt: madeAvailableAt.Time,
			Label:      "Capturada do DJEN",
		})
	}

	// 2. Prazo confirmado — only when confirmed_at is present (confirmed_by IS NOT NULL).
	if confirmedAt.Valid && deadlineStatus != nil {
		var label string
		switch *deadlineStatus {
		case "NO_DEADLINE":
			if confirmedByName != nil && *confirmedByName != "" {
				label = "Declarado sem prazo por " + *confirmedByName
			} else {
				label = "Declarado sem prazo"
			}
		default: // OPEN, MET, MISSED — all result from a human confirmation
			if confirmedByName != nil && *confirmedByName != "" {
				label = "Prazo confirmado por " + *confirmedByName
			} else {
				label = "Prazo confirmado"
			}
		}
		entries = append(entries, IntimacaoHistoryEntry{
			OccurredAt: confirmedAt.Time,
			Label:      label,
		})
	}

	// 3. Providências geradas — the analysis card was run (at least once; a re-run just
	// bumps the timestamp, so this always reflects the LAST generation, not every attempt).
	if aiAnalyzedAt.Valid {
		entries = append(entries, IntimacaoHistoryEntry{
			OccurredAt: aiAnalyzedAt.Time,
			Label:      "Providências geradas",
		})
	}

	sort.Slice(entries, func(a, b int) bool {
		return entries[a].OccurredAt.Before(entries[b].OccurredAt)
	})

	return entries
}

// nullDate maps the zero time to a SQL NULL date (Valid:false) for the nullable
// date columns (cancelled_at); a real instant is written as a valid date.
func nullDate(t time.Time) pgtype.Date {
	if t.IsZero() {
		return pgtype.Date{}
	}
	return pgtype.Date{Time: t, Valid: true}
}

// recipientsOrEmpty defaults an absent recipients list to the jsonb empty array
// the column stores, so the NOT NULL recipients column is never handed a nil.
func recipientsOrEmpty(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("[]")
	}
	return raw
}

// nullInt32 maps a zero TPU code to SQL NULL (nil *int32) for the nullable
// tpu_code column; a real code is written as-is.
func nullInt32(n int) *int32 {
	if n == 0 {
		return nil
	}
	v := int32(n)
	return &v
}

// complementsOrNil maps an absent complements blob to SQL NULL (the column is
// nullable jsonb), leaving a present blob as-is.
func complementsOrNil(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

// encodeSyncError serializes a failure reason to the sync_run.error jsonb, or nil
// (SQL NULL) when there is no error. The wrapper stays a low-cardinality shape
// ({"message": ...}) so the column is queryable.
func encodeSyncError(reason string) ([]byte, error) {
	if reason == "" {
		return nil, nil
	}
	raw, err := json.Marshal(map[string]string{"message": reason})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return raw, nil
}

// wireDateOrNull lifts a wire-format date (2006-01-02) to pgtype.Date; empty or
// malformed input becomes SQL NULL (the column is informational, never gating).
func wireDateOrNull(s string) pgtype.Date {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return pgtype.Date{}
	}
	return pgtype.Date{Time: t, Valid: true}
}
