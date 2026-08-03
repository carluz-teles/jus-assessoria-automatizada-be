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

// readRepo is the narrow read port the ReadUseCase drives — the keyset list reads,
// off the write path.
type readRepo interface {
	ListProcessos(ctx context.Context, q ProcessosQuery) ([]ProcessoView, error)
	ListIntimacoes(ctx context.Context, q IntimacoesQuery) ([]IntimacaoView, error)
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
