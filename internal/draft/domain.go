package draft

import (
	"context"
	"errors"
	"time"

	"github.com/jusassessoria/platform/lib/database"
)

// UseCase owns the domain logic for the peticionamento Fatia 1 write path.
// It opens the UoW transaction; the repo participates. Reads (GetDraftDetail) run
// in a lightweight, pool-backed read tx — no outbox, no UoW overhead.
//
// Event note: draft.created is intentionally NOT emitted in this fatia.
// The queueFor function in lib/events/relay.go routes "draft.*" → "default", and
// no worker drains the "default" queue — events would accumulate silently.
// TODO(F2): emit draft.created once a listener is registered on the correct queue.
type UseCase struct {
	repo database.UnitOfWork
	rw   Repository
	now  func() time.Time
}

// NewUseCase wires the use case to its dependencies. uow owns the transaction
// boundary for all three endpoints (GetDetail runs in a read-scoped Do, same RLS
// barrier as the writes). now defaults to time.Now and is overridable in tests.
func NewUseCase(uow database.UnitOfWork, repo Repository, opts ...Option) *UseCase {
	uc := &UseCase{repo: uow, rw: repo, now: time.Now}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// Option configures a UseCase at construction.
type Option func(*UseCase)

// WithClock overrides the reference clock. Production leaves the default (time.Now);
// tests pin it for deterministic assertions.
func WithClock(now func() time.Time) Option {
	return func(uc *UseCase) { uc.now = now }
}

// CreateCommand is the input the handler builds from the request + the verified
// principal. TenantID comes from the principal, never the body.
type CreateCommand struct {
	TenantID     string
	Source       string
	IntimationID string
	CaseID       string
	PieceType    string
	Title        string
}

// CreateResult carries the response body for a POST /v1/pecas: the created or found
// draft, plus a flag indicating whether this was a first creation (201) or idempotent
// (200).
type CreateResult struct {
	Draft     *Draft
	IsNewDraft bool
}

// Create implements POST /v1/pecas. It runs in a single tenant-scoped transaction:
//  1. When source=intimation, reads the intimation context (case_id, court_record_id,
//     type) to infer piece_type and populate CaseID.
//  2. Inserts the draft. On a 23505 unique violation (same tenant + intimation_id),
//     fetches the existing row and returns IsNewDraft=false (200 idempotent).
//
// tenant_id always comes from the verified principal — never the body.
func (uc *UseCase) Create(ctx context.Context, cmd CreateCommand) (CreateResult, error) {
	var result CreateResult

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		d := &Draft{
			TenantID:  cmd.TenantID,
			PieceType: cmd.PieceType,
			Title:     cmd.Title,
		}

		switch cmd.Source {
		case SourceIntimation:
			intimation, err := uc.rw.GetIntimationForDraft(ctx, tx, cmd.TenantID, cmd.IntimationID)
			if err != nil {
				return err
			}
			d.IntimationID = intimation.IntimationID
			d.CaseID = intimation.CaseID
			// Infer piece_type from intimation type when not supplied by the client.
			if d.PieceType == "" {
				d.PieceType = inferPieceType(intimation.Type)
			}

		case SourceProcesso:
			d.CaseID = cmd.CaseID
			if d.PieceType == "" {
				d.PieceType = PieceTypeOther
			}

		default: // SourceBlank
			if d.PieceType == "" {
				d.PieceType = PieceTypeOther
			}
		}

		created, err := uc.rw.InsertDraft(ctx, tx, d)
		if err != nil {
			if errors.Is(err, ErrDraftAlreadyExists) {
				// Idempotent: fetch and return the existing row (→ 200 at the edge).
				existing, fetchErr := uc.rw.GetDraftByIntimationID(ctx, tx, cmd.TenantID, cmd.IntimationID)
				if fetchErr != nil {
					return fetchErr
				}
				result = CreateResult{Draft: existing, IsNewDraft: false}
				return nil
			}
			return err
		}

		// TODO(F2): emit draft.created event here once a consumer queue is registered.
		// The relay routes "draft.*" → "default" (no worker drains it), so emitting
		// in Fatia 1 would cause silent queue buildup. Wire the event after the F2
		// listener is in place and lib/events/relay.go's queueFor routes "draft" to
		// the correct queue.

		result = CreateResult{Draft: created, IsNewDraft: true}
		return nil
	})
	if err != nil {
		return CreateResult{}, err
	}
	return result, nil
}

// PatchCommand is the input for PATCH /v1/pecas/:id.
type PatchCommand struct {
	TenantID string
	DraftID  string
	Content  string
	Title    *string
}

// Patch implements PATCH /v1/pecas/:id (autosave). Runs in a single tenant-scoped tx:
// updates content + optionally title + bumps updated_at. A missing or foreign-tenant
// draft is ErrDraftNotFound (→ 404). content empty is valid (editor cleared it).
// No event is emitted — autosave is not a domain event.
func (uc *UseCase) Patch(ctx context.Context, cmd PatchCommand) (*PatchResult, error) {
	var result *PatchResult

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		r, err := uc.rw.UpdateDraftContent(ctx, tx, cmd.DraftID, cmd.TenantID, cmd.Content, cmd.Title)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetDetail implements GET /v1/pecas/:id. Runs in a read-only tenant-scoped tx
// (the pool-backed Do — same RLS barrier as the writes). A missing or foreign-tenant
// draft is ErrDraftNotFound (→ 404).
func (uc *UseCase) GetDetail(ctx context.Context, tenantID, draftID string) (*DraftDetailView, error) {
	var view *DraftDetailView

	err := uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		v, err := uc.rw.GetDraftDetail(ctx, tx, tenantID, draftID)
		if err != nil {
			return err
		}
		view = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}
