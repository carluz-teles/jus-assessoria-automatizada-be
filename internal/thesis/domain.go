package thesis

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// UnitOfWork is the subset of database.UnitOfWork this slice needs; the real
// implementation (lib/database.UnitOfWork) satisfies it structurally.
type UnitOfWork interface {
	Do(ctx context.Context, tenantID string, fn func(tx database.Tx) error) error
}

// OutboxPublisher is the transactional-outbox producer port (see lib/events.Outbox).
type OutboxPublisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

type UseCase struct {
	uow    UnitOfWork
	repo   Repository
	now    func() time.Time
	outbox OutboxPublisher
}

type Option func(*UseCase)

func WithClock(now func() time.Time) Option {
	return func(uc *UseCase) { uc.now = now }
}

func NewUseCase(uow UnitOfWork, repo Repository, outbox OutboxPublisher, opts ...Option) *UseCase {
	uc := &UseCase{uow: uow, repo: repo, outbox: outbox, now: time.Now}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

type CreateThesisCommand struct {
	TenantID        string
	DraftID         string
	PieceProfileKey string
	NotificationID  string
	Enunciado       string
	Forca           string
	Anchors         []CreateAnchorRequest
}

func (uc *UseCase) CreateThesis(ctx context.Context, cmd CreateThesisCommand) (*Thesis, error) {
	var thesis *Thesis
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		thesis = &Thesis{
			ID:              uuid.Must(uuid.NewV7()).String(),
			TenantID:        cmd.TenantID,
			DraftID:         cmd.DraftID,
			PieceProfileKey: cmd.PieceProfileKey,
			NotificationID:  cmd.NotificationID,
			Enunciado:       cmd.Enunciado,
			Forca:           cmd.Forca,
			Estado:          EstadoProposta,
			CreatedAt:       uc.now(),
		}
		var err error
		thesis, err = uc.repo.InsertThesis(ctx, tx, thesis)
		if err != nil {
			return err
		}
		for i := range cmd.Anchors {
			anchor := &ThesisAnchor{
				ID:            uuid.Must(uuid.NewV7()).String(),
				ThesisID:      thesis.ID,
				Tipo:          cmd.Anchors[i].Tipo,
				AlvoDocumento: cmd.Anchors[i].AlvoDocumento,
				AlvoFonte:     cmd.Anchors[i].AlvoFonte,
				Motivo:        cmd.Anchors[i].Motivo,
				Status:        AnchorStatusAConfirmar,
			}
			if _, err := uc.repo.InsertThesisAnchor(ctx, tx, anchor); err != nil {
				return err
			}
		}
		return uc.outbox.Publish(ctx, tx, newThesisCreated(thesis))
	})
	if err != nil {
		return nil, err
	}
	return thesis, nil
}

func (uc *UseCase) ListThesesByDraft(ctx context.Context, tenantID, draftID string) ([]Thesis, error) {
	var theses []Thesis
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		theses, err = uc.repo.ListThesesByDraft(ctx, tx, tenantID, draftID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if theses == nil {
		theses = []Thesis{}
	}
	return theses, nil
}

func (uc *UseCase) GetThesisByID(ctx context.Context, tenantID, thesisID string) (*Thesis, error) {
	var t *Thesis
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		t, err = uc.repo.GetThesisByID(ctx, tx, tenantID, thesisID)
		if err != nil {
			return err
		}
		t.Anchors, err = uc.repo.ListAnchorsByThesis(ctx, tx, t.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (uc *UseCase) ApproveThesis(ctx context.Context, tenantID, thesisID string) (*Thesis, error) {
	var thesis *Thesis
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		thesis, err = uc.repo.UpdateThesisEstado(ctx, tx, tenantID, thesisID, EstadoAprovada)
		if err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newThesisApproved(thesis))
	})
	if err != nil {
		return nil, err
	}
	return thesis, nil
}

func (uc *UseCase) DiscardThesis(ctx context.Context, tenantID, thesisID string) (*Thesis, error) {
	var thesis *Thesis
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		thesis, err = uc.repo.UpdateThesisEstado(ctx, tx, tenantID, thesisID, EstadoDescartada)
		if err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newThesisDiscarded(thesis))
	})
	if err != nil {
		return nil, err
	}
	return thesis, nil
}

type CreateSegmentCommand struct {
	TenantID         string
	DraftID          string
	ThesisID         string
	ProfileSectionID string
	Conteudo         string
}

func (uc *UseCase) CreateSegment(ctx context.Context, cmd CreateSegmentCommand) (*DraftSegment, error) {
	var segment *DraftSegment
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		segment = &DraftSegment{
			ID:               uuid.Must(uuid.NewV7()).String(),
			TenantID:         cmd.TenantID,
			DraftID:          cmd.DraftID,
			ThesisID:         cmd.ThesisID,
			ProfileSectionID: cmd.ProfileSectionID,
			Conteudo:         cmd.Conteudo,
			CreatedAt:        uc.now(),
		}
		var err error
		segment, err = uc.repo.InsertDraftSegment(ctx, tx, segment)
		return err
	})
	if err != nil {
		return nil, err
	}
	return segment, nil
}

func (uc *UseCase) ListSegmentsByDraft(ctx context.Context, tenantID, draftID string) ([]DraftSegment, error) {
	var segments []DraftSegment
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		segments, err = uc.repo.ListSegmentsByDraft(ctx, tx, tenantID, draftID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if segments == nil {
		segments = []DraftSegment{}
	}
	return segments, nil
}

// CheckCoverage recomputes the coverage verdict for a thesis (docs/erd-costura-
// providencia-tarefa-peca.md §4.2): coberta when at least one draft_segment exists
// and every one of the thesis's anchors is preserved by a segment_anchor; divergente
// when segments exist but an anchor was dropped; ausente when the thesis has no
// segment at all. draftID is read off the thesis itself (thesis.draft_id) — the
// caller only needs the thesis id.
func (uc *UseCase) CheckCoverage(ctx context.Context, tenantID, thesisID string) (*ThesisCoverage, error) {
	var coverage *ThesisCoverage
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		t, err := uc.repo.GetThesisByID(ctx, tx, tenantID, thesisID)
		if err != nil {
			return err
		}

		anchors, err := uc.repo.ListAnchorsByThesis(ctx, tx, t.ID)
		if err != nil {
			return err
		}

		segments, err := uc.repo.ListSegmentsByDraft(ctx, tx, tenantID, t.DraftID)
		if err != nil {
			return err
		}
		var thesisSegments []DraftSegment
		for _, s := range segments {
			if s.ThesisID == t.ID {
				thesisSegments = append(thesisSegments, s)
			}
		}

		resultado, detalhe, err := computeCoverage(ctx, uc.repo, tx, anchors, thesisSegments)
		if err != nil {
			return err
		}

		c := &ThesisCoverage{
			ID:        uuid.Must(uuid.NewV7()).String(),
			ThesisID:  t.ID,
			Resultado: resultado,
			Detalhe:   detalhe,
			CreatedAt: uc.now(),
		}
		coverage, err = uc.repo.InsertThesisCoverage(ctx, tx, c)
		if err != nil {
			return err
		}

		return uc.outbox.Publish(ctx, tx, newThesisCoverageChecked(tenantID, t.DraftID, coverage))
	})
	if err != nil {
		return nil, err
	}
	return coverage, nil
}

// computeCoverage applies the three-way rule from docs/erd-costura-providencia-
// tarefa-peca.md §4.2: ausente when no segment carries the thesis, divergente when a
// segment exists but does not preserve every anchor the thesis declared, coberta
// otherwise.
func computeCoverage(ctx context.Context, repo Repository, tx database.Tx, anchors []ThesisAnchor, segments []DraftSegment) (string, string, error) {
	if len(segments) == 0 {
		return CoverageAusente, "tese aprovada sem segmento na peça", nil
	}

	preserved := make(map[string]bool, len(anchors))
	for _, s := range segments {
		segAnchors, err := repo.ListAnchorsBySegment(ctx, tx, s.ID)
		if err != nil {
			return "", "", err
		}
		for _, sa := range segAnchors {
			preserved[sa.ThesisAnchorID] = true
		}
	}

	for _, a := range anchors {
		if !preserved[a.ID] {
			return CoverageDivergente, "âncora da tese não preservada em nenhum segmento", nil
		}
	}

	return CoverageCoberta, "", nil
}

func (uc *UseCase) ListCoverageByDraft(ctx context.Context, tenantID, draftID string) ([]ThesisCoverage, error) {
	var coverage []ThesisCoverage
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		coverage, err = uc.repo.ListCoverageByDraft(ctx, tx, tenantID, draftID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if coverage == nil {
		coverage = []ThesisCoverage{}
	}
	return coverage, nil
}

func (uc *UseCase) GetCoverageSummary(ctx context.Context, tenantID, draftID string) (*CoverageSummary, error) {
	var summary *CoverageSummary
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		summary, err = uc.repo.GetCoverageSummary(ctx, tx, tenantID, draftID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return summary, nil
}
