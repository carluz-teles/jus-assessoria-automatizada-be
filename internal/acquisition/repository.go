package acquisition

import (
	"context"
	"encoding/json"
	"errors"
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
	UpsertIntimations(ctx context.Context, tx database.Tx, params []IntimationParams) (newCount int, err error)

	// DATAJUD enrichment — the placeholder+merge grade reconciliation. The
	// enrichment use case depends on the narrow enrichRepo view of these.
	UpsertGradedCourtRecord(ctx context.Context, tx database.Tx, params GradedRecordParams) (*CourtRecord, error)
	RepointIntimations(ctx context.Context, tx database.Tx, tenantID, fromRecordID, toRecordID string) (moved int, err error)
	SupersedeCourtRecord(ctx context.Context, tx database.Tx, tenantID, recordID string) error

	// Re-poll scheduler — the system-scoped due scan and claim. The scheduler use
	// case depends on the narrow schedulerRepo view of these.
	DueCourtRecordsForResync(ctx context.Context, tx database.Tx, limit int) ([]DueRecord, error)
	ClaimCourtRecordResync(ctx context.Context, tx database.Tx, recordID string, nextSyncAt time.Time) error

	// Screen reads — the keyset-paginated read models (off the write path). The read
	// use case depends on the narrow readRepo view of these.
	ListProcessos(ctx context.Context, q ProcessosQuery) ([]ProcessoView, error)
	ListIntimacoes(ctx context.Context, q IntimacoesQuery) ([]IntimacaoView, error)
	GetImportStatus(ctx context.Context, tenantID string) (ImportStatusView, error)
	GetReconciliationTotals(ctx context.Context, tenantID string) (ReconciliationTotals, error)
	ListReconciliations(ctx context.Context, tenantID string, limit int) ([]ReconciliationView, error)
	GetReconciliation(ctx context.Context, tenantID, jobID string) (ReconciliationView, error)
	ListSyncRunsByJob(ctx context.Context, tenantID, jobID string) ([]ReconciliationRunView, error)
	ListProcessosBySyncRun(ctx context.Context, tenantID, syncRunID string) ([]ProcessoLineView, error)
	ListIntimacoesBySyncRun(ctx context.Context, tenantID, syncRunID string) ([]IntimacaoLineView, error)

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
		ID:              id,
		Status:          outcome.Status,
		ItemsNew:        int32(outcome.ItemsNew),
		ItemsDeduped:    int32(outcome.ItemsDeduped),
		CourtRecordsNew: int32(outcome.CourtRecordsNew),
		IntimationsNew:  int32(outcome.IntimationsNew),
		FinishedAt:      pgtype.Timestamptz{Time: outcome.FinishedAt, Valid: true},
		Error:           errJSON,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, database.WrapInfra(err)
	}
	return true, nil
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
// the caller's tx and returns how many were ACTUALLY new. The DO UPDATE always
// returns a row (unlike the docket entries' DO NOTHING), so newness is read from
// the query's `inserted` flag (xmax = 0) rather than a pgx.ErrNoRows miss: an
// existing intimation retracted by the source is updated in place (so the deadline
// slice can revoke its prazo), counting as deduped, not new. This slice emits no
// intimation-observed event, so only the count is returned.
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

// UpsertGradedCourtRecord find-or-creates the graded court record inside the
// caller's tx (natural key tenant+cnj+degree, in the given case) and refreshes the
// DATAJUD-authoritative fields, returning the entity the enrichment re-points onto.
// It is NOT entitlement-gated: grading an already-tracked process must not consume
// a second plan slot.
func (r *pgRepository) UpsertGradedCourtRecord(ctx context.Context, tx database.Tx, params GradedRecordParams) (*CourtRecord, error) {
	tid, err := uuid.Parse(params.TenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	caseID, err := uuid.Parse(params.CaseID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	row, err := acquisitiondb.New(tx).UpsertGradedCourtRecord(ctx, acquisitiondb.UpsertGradedCourtRecordParams{
		TenantID:     tid,
		CaseID:       caseID,
		CnjNumber:    params.CNJNumber,
		Degree:       params.Degree,
		Court:        params.Court,
		Class:        nullString(params.Class),
		Subject:      nullString(params.Subject),
		JudgingBody:  nullString(params.JudgingBody),
		FiledAt:      nullDate(params.FiledAt),
		Secrecy:      params.Secrecy,
		Completeness: params.Completeness,
		NextSyncAt:   pgtype.Timestamptz{Time: params.NextSyncAt, Valid: !params.NextSyncAt.IsZero()},
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return &CourtRecord{
		ID:        row.ID.String(),
		TenantID:  params.TenantID,
		CaseID:    row.CaseID.String(),
		CNJNumber: params.CNJNumber,
		Degree:    params.Degree,
		Court:     params.Court,
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
		TenantID: tid,
		Limit:    int32(q.Limit),
		LastCnj:  q.LastCNJ,
		LastID:   lastID,
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
			LastMovementText: row.LastMovementText,
			LastMovementAt:   timestampPtr(row.LastMovementAt),
		})
	}
	return out, nil
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
			Court:           row.Court,
			Degree:          row.Degree,
			Type:            deref(row.Type),
			Status:          row.Status,
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

// contentPreviewLen bounds the intimation teor preview the inbox list carries; the
// full (often long, HTML) content is a detail-screen concern.
const contentPreviewLen = 500

// contentPreview truncates the teor to contentPreviewLen runes for the inbox list.
func contentPreview(content string) string {
	runes := []rune(content)
	if len(runes) <= contentPreviewLen {
		return content
	}
	return string(runes[:contentPreviewLen])
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
