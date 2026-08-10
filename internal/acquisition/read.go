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
	ID            string     `json:"id"`
	Source        string     `json:"source"`
	WindowFrom    string     `json:"window_from"`
	WindowTo      string     `json:"window_to"`
	Status        string     `json:"status"` // RUNNING | OK | FAILED
	ProcessosNew  int        `json:"processos_new"`
	IntimacoesNew int        `json:"intimacoes_new"`
	Error         *string    `json:"error"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
}

// ReconciliationTotals is the tenant's acquired-so-far tally shown in the
// reconciliations summary (and during an import, its progress line).
type ReconciliationTotals struct {
	CourtRecords int `json:"court_records"`
	Intimations  int `json:"intimations"`
}

// ReconciliationView is one import (backfill_job) on the reconciliations
// screen — the "reconciliação": the processes/intimations its windows discovered
// (summed), the overall date window (janela de prazo geral), the slice tallies and
// the lifecycle. The user drills into it for the per-window detail.
type ReconciliationView struct {
	ID          string     `json:"id"`
	Source      string     `json:"source"`
	Status      string     `json:"status"` // RUNNING | COMPLETED | PARTIAL
	WindowFrom  string     `json:"window_from"`
	WindowTo    string     `json:"window_to"`
	Processos   int        `json:"processos"`
	Intimacoes  int        `json:"intimacoes"`
	TotalSlices int        `json:"total_slices"`
	SlicesOK    int        `json:"slices_ok"`
	SlicesError int        `json:"slices_error"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

// ReconciliationsView is the reconciliations screen read: the import banner state,
// the tenant's acquired totals, and one card per import (reconciliation), newest first.
type ReconciliationsView struct {
	Import          ImportStatusView     `json:"import"`
	Totals          ReconciliationTotals `json:"totals"`
	Reconciliations []ReconciliationView `json:"reconciliations"`
}

// ReconciliationDetailView is the deep view of one import: its reconciliation header plus
// every window (sync_run), chronological — the detail screen's table.
type ReconciliationDetailView struct {
	Reconciliation ReconciliationView      `json:"reconciliation"`
	Windows        []ReconciliationRunView `json:"windows"`
}

// ProcessoLineView / IntimacaoLineView are the compact rows a window's collapse
// lists — the processes/intimations that window first discovered (via sync_run_id).
type ProcessoLineView struct {
	ID        string `json:"id"`
	CNJNumber string `json:"cnj_number"`
	Court     string `json:"court"`
	Degree    string `json:"degree"`
	Class     string `json:"class"`
}

type IntimacaoLineView struct {
	ID              string    `json:"id"`
	CNJNumber       string    `json:"cnj_number"`
	Court           string    `json:"court"`
	Degree          string    `json:"degree"`
	Type            string    `json:"type"`
	Status          string    `json:"status"`
	MadeAvailableAt time.Time `json:"made_available_at"`
}

// SyncRunItemsView is a window's collapse payload: the processes and intimations it
// brought, loaded on expand.
type SyncRunItemsView struct {
	Processos  []ProcessoLineView  `json:"processos"`
	Intimacoes []IntimacaoLineView `json:"intimacoes"`
}

// reconciliationRunsLimit caps the reconciliation list: one screenful of imports, newest
// first (an eventual "ver mais" pages beyond it).
const reconciliationRunsLimit = 60

// readRepo is the narrow read port the ReadUseCase drives — the keyset list reads
// and the import-status/reconciliations reads, off the write path.
type readRepo interface {
	ListProcessos(ctx context.Context, q ProcessosQuery) ([]ProcessoView, error)
	ListIntimacoes(ctx context.Context, q IntimacoesQuery) ([]IntimacaoView, error)
	GetImportStatus(ctx context.Context, tenantID string) (ImportStatusView, error)
	GetReconciliationTotals(ctx context.Context, tenantID string) (ReconciliationTotals, error)
	ListReconciliations(ctx context.Context, tenantID string, limit int) ([]ReconciliationView, error)
	GetReconciliation(ctx context.Context, tenantID, jobID string) (ReconciliationView, error)
	ListSyncRunsByJob(ctx context.Context, tenantID, jobID string) ([]ReconciliationRunView, error)
	ListProcessosBySyncRun(ctx context.Context, tenantID, syncRunID string) ([]ProcessoLineView, error)
	ListIntimacoesBySyncRun(ctx context.Context, tenantID, syncRunID string) ([]IntimacaoLineView, error)
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

// Reconciliations returns the reconciliations screen read: import banner state +
// acquired totals + one reconciliation per import. Three pool reads, no tx — a read
// model, not an aggregate; a small skew between them under concurrent syncs is fine.
func (uc *ReadUseCase) Reconciliations(ctx context.Context, tenantID string) (ReconciliationsView, error) {
	imp, err := uc.repo.GetImportStatus(ctx, tenantID)
	if err != nil {
		return ReconciliationsView{}, err
	}
	totals, err := uc.repo.GetReconciliationTotals(ctx, tenantID)
	if err != nil {
		return ReconciliationsView{}, err
	}
	recons, err := uc.repo.ListReconciliations(ctx, tenantID, reconciliationRunsLimit)
	if err != nil {
		return ReconciliationsView{}, err
	}
	return ReconciliationsView{Import: imp, Totals: totals, Reconciliations: recons}, nil
}

// ReconciliationDetail returns one import's reconciliação header plus every window
// (sync_run) it fanned out, chronological — the detail screen. A miss on the
// reconciliation surfaces as the repo's typed not-found.
func (uc *ReadUseCase) ReconciliationDetail(ctx context.Context, tenantID, jobID string) (ReconciliationDetailView, error) {
	reconciliation, err := uc.repo.GetReconciliation(ctx, tenantID, jobID)
	if err != nil {
		return ReconciliationDetailView{}, err
	}
	windows, err := uc.repo.ListSyncRunsByJob(ctx, tenantID, jobID)
	if err != nil {
		return ReconciliationDetailView{}, err
	}
	return ReconciliationDetailView{Reconciliation: reconciliation, Windows: windows}, nil
}

// SyncRunItems returns a window's collapse payload: the processes and intimations it
// first discovered. Two pool reads; empty slices when the window brought nothing.
func (uc *ReadUseCase) SyncRunItems(ctx context.Context, tenantID, syncRunID string) (SyncRunItemsView, error) {
	processos, err := uc.repo.ListProcessosBySyncRun(ctx, tenantID, syncRunID)
	if err != nil {
		return SyncRunItemsView{}, err
	}
	intimacoes, err := uc.repo.ListIntimacoesBySyncRun(ctx, tenantID, syncRunID)
	if err != nil {
		return SyncRunItemsView{}, err
	}
	return SyncRunItemsView{Processos: processos, Intimacoes: intimacoes}, nil
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
