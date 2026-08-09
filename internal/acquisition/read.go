package acquisition

import (
	"context"
	"time"
)

// read.go is the slice's read side: the screen reads that bypass the write
// aggregate (a DTO per query, not the entity). The use case here is a thin
// pagination policy over the repo's keyset reads — it over-fetches one row to tell
// the handler whether a next page exists, without a COUNT.

// ProcessoView is one row of the consolidated processes screen: the court record
// plus its most recent andamento. Nullable columns are pointers so an absent value
// serializes as JSON null, not a zero.
type ProcessoView struct {
	ID               string     `json:"id"`
	CaseID           string     `json:"case_id"`
	CNJNumber        string     `json:"cnj_number"`
	Court            string     `json:"court"`
	Degree           string     `json:"degree"`
	Class            string     `json:"class"`
	Subject          string     `json:"subject"`
	JudgingBody      string     `json:"judging_body"`
	FiledAt          *time.Time `json:"filed_at"`
	Secrecy          string     `json:"secrecy"`
	Lifecycle        string     `json:"lifecycle"`
	Completeness     float32    `json:"completeness"`
	LastMovementText string     `json:"last_movement_text"`
	LastMovementAt   *time.Time `json:"last_movement_at"`
}

// IntimacaoView is one row of the intimações inbox: the intimation plus its court
// record's number/court/degree. ContentPreview is a truncation of the (often long,
// HTML) teor — the inbox shows a summary, the detail screen the full content.
type IntimacaoView struct {
	ID              string    `json:"id"`
	CNJNumber       string    `json:"cnj_number"`
	Court           string    `json:"court"`
	Degree          string    `json:"degree"`
	Type            string    `json:"type"`
	Status          string    `json:"status"`
	Source          string    `json:"source"`
	SourceURL       string    `json:"source_url"`
	MadeAvailableAt time.Time `json:"made_available_at"`
	PublishedAt     time.Time `json:"published_at"`
	DeadlineStartAt time.Time `json:"deadline_start_at"`
	ContentPreview  string    `json:"content_preview"`
}

// ProcessosQuery / IntimacoesQuery carry the keyset cursor (the last row's sort key
// and id) and the page size. The handler fills the sentinel for a first page; the
// repo turns them into the query's keyset predicate.
type ProcessosQuery struct {
	TenantID string
	LastCNJ  string
	LastID   string
	Limit    int
}

type IntimacoesQuery struct {
	TenantID          string
	LastMadeAvailable string
	LastID            string
	Limit             int
}

// ImportStatusView is the onboarding backfill state for the FE banner ("importando
// seus processos…"): whether an import is running for the tenant, plus the slice
// tallies for a progress hint. Status NONE (no job ever) keeps the banner hidden.
type ImportStatusView struct {
	Importing   bool   `json:"importing"` // status == RUNNING
	Status      string `json:"status"`    // RUNNING | COMPLETED | PARTIAL | NONE
	TotalSlices int    `json:"total_slices"`
	SlicesOK    int    `json:"slices_ok"`
	SlicesError int    `json:"slices_error"`
}

// importStatusNone is the read-side sentinel for a tenant with no backfill_job — the
// FE banner treats it as "not importing" (never shown). It is NOT a DB status value.
const importStatusNone = "NONE"

// ReconciliationRunView is one row of the reconciliations screen: a sync_run with
// its integration's source and the failure reason (error jsonb message) lifted to
// a nullable string. Window bounds use the wire date format (2006-01-02).
type ReconciliationRunView struct {
	ID           string     `json:"id"`
	Source       string     `json:"source"`
	WindowFrom   string     `json:"window_from"`
	WindowTo     string     `json:"window_to"`
	Status       string     `json:"status"` // RUNNING | OK | FAILED
	ItemsNew     int        `json:"items_new"`
	ItemsDeduped int        `json:"items_deduped"`
	Error        *string    `json:"error"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}

// ReconciliationTotals is the tenant's acquired-so-far tally shown in the
// reconciliations summary (and during an import, its progress line).
type ReconciliationTotals struct {
	CourtRecords int `json:"court_records"`
	Intimations  int `json:"intimations"`
}

// ReconciliationsView is the whole reconciliations read: the import (backfill)
// state, the acquired totals and the recent executions, newest first.
type ReconciliationsView struct {
	Import ImportStatusView        `json:"import"`
	Totals ReconciliationTotals    `json:"totals"`
	Runs   []ReconciliationRunView `json:"runs"`
}

// reconciliationRunsLimit caps the executions list: the 53 weekly backfill slices
// plus headroom for the continuous captures — one screenful of audit, not the
// whole history (an eventual "ver mais" pages beyond it).
const reconciliationRunsLimit = 60

// readRepo is the narrow read port the ReadUseCase drives — the keyset list reads
// and the import-status/reconciliations reads, off the write path.
type readRepo interface {
	ListProcessos(ctx context.Context, q ProcessosQuery) ([]ProcessoView, error)
	ListIntimacoes(ctx context.Context, q IntimacoesQuery) ([]IntimacaoView, error)
	GetImportStatus(ctx context.Context, tenantID string) (ImportStatusView, error)
	ListRecentSyncRuns(ctx context.Context, tenantID string, limit int) ([]ReconciliationRunView, error)
	GetReconciliationTotals(ctx context.Context, tenantID string) (ReconciliationTotals, error)
}

// ReadUseCase serves the screen reads. It is a pagination policy over readRepo: it
// over-fetches one row per page so the handler learns whether more remain without a
// separate COUNT.
type ReadUseCase struct {
	repo readRepo
}

// NewReadUseCase wires the read use case (share the slice's repo with the writer).
func NewReadUseCase(repo readRepo) *ReadUseCase {
	return &ReadUseCase{repo: repo}
}

// Processos returns up to q.Limit processes and whether a further page exists.
func (uc *ReadUseCase) Processos(ctx context.Context, q ProcessosQuery) (items []ProcessoView, hasMore bool, err error) {
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := uc.repo.ListProcessos(ctx, q)
	if err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// ImportStatus returns the tenant's latest backfill state — the FE banner reads it
// on load (so it survives a page refresh) and hides on the import_finished push.
func (uc *ReadUseCase) ImportStatus(ctx context.Context, tenantID string) (ImportStatusView, error) {
	return uc.repo.GetImportStatus(ctx, tenantID)
}

// Reconciliations returns the reconciliations screen read: import state + acquired
// totals + the recent executions. Three pool reads, no tx — a read model, not an
// aggregate; a small skew between them under concurrent syncs is acceptable.
func (uc *ReadUseCase) Reconciliations(ctx context.Context, tenantID string) (ReconciliationsView, error) {
	imp, err := uc.repo.GetImportStatus(ctx, tenantID)
	if err != nil {
		return ReconciliationsView{}, err
	}
	totals, err := uc.repo.GetReconciliationTotals(ctx, tenantID)
	if err != nil {
		return ReconciliationsView{}, err
	}
	runs, err := uc.repo.ListRecentSyncRuns(ctx, tenantID, reconciliationRunsLimit)
	if err != nil {
		return ReconciliationsView{}, err
	}
	return ReconciliationsView{Import: imp, Totals: totals, Runs: runs}, nil
}

// Intimacoes returns up to q.Limit intimations (newest availability first) and
// whether a further page exists.
func (uc *ReadUseCase) Intimacoes(ctx context.Context, q IntimacoesQuery) (items []IntimacaoView, hasMore bool, err error) {
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := uc.repo.ListIntimacoes(ctx, q)
	if err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}
