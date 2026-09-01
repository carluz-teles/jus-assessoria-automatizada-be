package thesis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// fakeUOW is a no-op unit of work: it runs fn with a nil tx (the mocked repo never
// touches it) and records the RLS scope the use case asked for.
type fakeUOW struct {
	scope string
	err   error
}

func (u *fakeUOW) Do(ctx context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.scope = tenantID
	if u.err != nil {
		return u.err
	}
	return fn(nil)
}

// recordingOutbox captures what a use case publishes.
type recordingOutbox struct {
	published []events.Event
	err       error
}

func (r *recordingOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	if r.err != nil {
		return r.err
	}
	r.published = append(r.published, ev)
	return nil
}

// mockRepo is a hand-rolled Repository backed by in-memory maps, enough to drive
// the create/approve/discard/coverage flows without a real database.
type mockRepo struct {
	theses           map[string]*Thesis
	anchorsByThesis  map[string][]ThesisAnchor
	segmentsByDraft  map[string][]DraftSegment
	anchorsBySegment map[string][]SegmentAnchor

	insertedCoverage *ThesisCoverage
	getThesisErr     error
}

func newMockRepo() *mockRepo {
	return &mockRepo{
		theses:           map[string]*Thesis{},
		anchorsByThesis:  map[string][]ThesisAnchor{},
		segmentsByDraft:  map[string][]DraftSegment{},
		anchorsBySegment: map[string][]SegmentAnchor{},
	}
}

func (m *mockRepo) InsertThesis(ctx context.Context, tx database.Tx, t *Thesis) (*Thesis, error) {
	cp := *t
	m.theses[t.ID] = &cp
	return &cp, nil
}

func (m *mockRepo) GetThesisByID(ctx context.Context, tx database.Tx, tenantID, thesisID string) (*Thesis, error) {
	if m.getThesisErr != nil {
		return nil, m.getThesisErr
	}
	t, ok := m.theses[thesisID]
	if !ok {
		return nil, ErrThesisNotFound
	}
	cp := *t
	return &cp, nil
}

func (m *mockRepo) ListThesesByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]Thesis, error) {
	var out []Thesis
	for _, t := range m.theses {
		if t.DraftID == draftID {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (m *mockRepo) UpdateThesisEstado(ctx context.Context, tx database.Tx, tenantID, thesisID, estado string) (*Thesis, error) {
	t, ok := m.theses[thesisID]
	if !ok {
		return nil, ErrThesisNotFound
	}
	t.Estado = estado
	cp := *t
	return &cp, nil
}

func (m *mockRepo) InsertThesisAnchor(ctx context.Context, tx database.Tx, a *ThesisAnchor) (*ThesisAnchor, error) {
	cp := *a
	m.anchorsByThesis[a.ThesisID] = append(m.anchorsByThesis[a.ThesisID], cp)
	return &cp, nil
}

func (m *mockRepo) ListAnchorsByThesis(ctx context.Context, tx database.Tx, thesisID string) ([]ThesisAnchor, error) {
	return m.anchorsByThesis[thesisID], nil
}

func (m *mockRepo) InsertDraftSegment(ctx context.Context, tx database.Tx, s *DraftSegment) (*DraftSegment, error) {
	cp := *s
	m.segmentsByDraft[s.DraftID] = append(m.segmentsByDraft[s.DraftID], cp)
	return &cp, nil
}

func (m *mockRepo) ListSegmentsByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]DraftSegment, error) {
	return m.segmentsByDraft[draftID], nil
}

func (m *mockRepo) InsertSegmentAnchor(ctx context.Context, tx database.Tx, sa *SegmentAnchor) (*SegmentAnchor, error) {
	cp := *sa
	m.anchorsBySegment[sa.DraftSegmentID] = append(m.anchorsBySegment[sa.DraftSegmentID], cp)
	return &cp, nil
}

func (m *mockRepo) ListAnchorsBySegment(ctx context.Context, tx database.Tx, segmentID string) ([]SegmentAnchor, error) {
	return m.anchorsBySegment[segmentID], nil
}

func (m *mockRepo) InsertThesisCoverage(ctx context.Context, tx database.Tx, c *ThesisCoverage) (*ThesisCoverage, error) {
	cp := *c
	m.insertedCoverage = &cp
	return &cp, nil
}

func (m *mockRepo) ListCoverageByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]ThesisCoverage, error) {
	if m.insertedCoverage == nil {
		return nil, nil
	}
	return []ThesisCoverage{*m.insertedCoverage}, nil
}

func (m *mockRepo) GetCoverageSummary(ctx context.Context, tx database.Tx, tenantID, draftID string) (*CoverageSummary, error) {
	return &CoverageSummary{}, nil
}

func newUseCase(repo *mockRepo, outbox OutboxPublisher) *UseCase {
	return NewUseCase(&fakeUOW{}, repo, outbox, WithClock(func() time.Time { return time.Unix(0, 0) }))
}

func TestUseCase_CreateThesis(t *testing.T) {
	t.Parallel()

	repo := newMockRepo()
	outbox := &recordingOutbox{}
	uc := newUseCase(repo, outbox)

	th, err := uc.CreateThesis(context.Background(), CreateThesisCommand{
		TenantID: "tenant-1", DraftID: "draft-1", Enunciado: "réu não impugnou o fato X",
		Forca: ForcaFavoravel,
		Anchors: []CreateAnchorRequest{
			{Tipo: AnchorTipoFato, AlvoDocumento: "doc-1", Motivo: "consta nos autos"},
		},
	})
	if err != nil {
		t.Fatalf("CreateThesis() error = %v", err)
	}
	if th.Estado != EstadoProposta {
		t.Errorf("Estado = %q, want proposta (a tese nasce proposta)", th.Estado)
	}
	if len(repo.anchorsByThesis[th.ID]) != 1 {
		t.Errorf("anchors persisted = %d, want 1", len(repo.anchorsByThesis[th.ID]))
	}
	if len(outbox.published) != 1 || outbox.published[0].Type() != TypeThesisCreated {
		t.Errorf("published = %+v, want one thesis.created event", outbox.published)
	}
}

func TestUseCase_ApproveDiscardThesis(t *testing.T) {
	t.Parallel()

	t.Run("approve flips estado and publishes thesis.approved", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.theses["t1"] = &Thesis{ID: "t1", TenantID: "tenant-1", DraftID: "draft-1", Estado: EstadoProposta}
		outbox := &recordingOutbox{}
		uc := newUseCase(repo, outbox)

		th, err := uc.ApproveThesis(context.Background(), "tenant-1", "t1")
		if err != nil {
			t.Fatalf("ApproveThesis() error = %v", err)
		}
		if th.Estado != EstadoAprovada {
			t.Errorf("Estado = %q, want aprovada", th.Estado)
		}
		if len(outbox.published) != 1 || outbox.published[0].Type() != TypeThesisApproved {
			t.Errorf("published = %+v, want one thesis.approved event", outbox.published)
		}
	})

	t.Run("discard flips estado and publishes thesis.discarded", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.theses["t1"] = &Thesis{ID: "t1", TenantID: "tenant-1", DraftID: "draft-1", Estado: EstadoProposta}
		outbox := &recordingOutbox{}
		uc := newUseCase(repo, outbox)

		th, err := uc.DiscardThesis(context.Background(), "tenant-1", "t1")
		if err != nil {
			t.Fatalf("DiscardThesis() error = %v", err)
		}
		if th.Estado != EstadoDescartada {
			t.Errorf("Estado = %q, want descartada", th.Estado)
		}
		if len(outbox.published) != 1 || outbox.published[0].Type() != TypeThesisDiscarded {
			t.Errorf("published = %+v, want one thesis.discarded event", outbox.published)
		}
	})

	t.Run("unknown thesis propagates typed not-found", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		uc := newUseCase(repo, &recordingOutbox{})

		_, err := uc.ApproveThesis(context.Background(), "tenant-1", "unknown")
		if !errors.Is(err, ErrThesisNotFound) {
			t.Errorf("ApproveThesis() error = %v, want ErrThesisNotFound", err)
		}
	})
}

// TestUseCase_CheckCoverage drives the three-way rule from docs/erd-costura-
// providencia-tarefa-peca.md §4.2: ausente (no segment at all), divergente (a
// segment exists but drops an anchor the thesis declared), coberta (every anchor
// survives in some segment).
func TestUseCase_CheckCoverage(t *testing.T) {
	t.Parallel()

	t.Run("ausente: thesis with no draft_segment", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.theses["t1"] = &Thesis{ID: "t1", TenantID: "tenant-1", DraftID: "draft-1", Estado: EstadoAprovada}
		uc := newUseCase(repo, &recordingOutbox{})

		cov, err := uc.CheckCoverage(context.Background(), "tenant-1", "t1")
		if err != nil {
			t.Fatalf("CheckCoverage() error = %v", err)
		}
		if cov.Resultado != CoverageAusente {
			t.Errorf("Resultado = %q, want ausente", cov.Resultado)
		}
	})

	t.Run("divergente: segment exists but drops an anchor", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.theses["t1"] = &Thesis{ID: "t1", TenantID: "tenant-1", DraftID: "draft-1", Estado: EstadoAprovada}
		repo.anchorsByThesis["t1"] = []ThesisAnchor{{ID: "a1", ThesisID: "t1"}, {ID: "a2", ThesisID: "t1"}}
		repo.segmentsByDraft["draft-1"] = []DraftSegment{{ID: "seg1", DraftID: "draft-1", ThesisID: "t1"}}
		// Only a1 is preserved — a2 is dropped.
		repo.anchorsBySegment["seg1"] = []SegmentAnchor{{ID: "sa1", DraftSegmentID: "seg1", ThesisAnchorID: "a1"}}
		uc := newUseCase(repo, &recordingOutbox{})

		cov, err := uc.CheckCoverage(context.Background(), "tenant-1", "t1")
		if err != nil {
			t.Fatalf("CheckCoverage() error = %v", err)
		}
		if cov.Resultado != CoverageDivergente {
			t.Errorf("Resultado = %q, want divergente", cov.Resultado)
		}
	})

	t.Run("coberta: every anchor preserved", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.theses["t1"] = &Thesis{ID: "t1", TenantID: "tenant-1", DraftID: "draft-1", Estado: EstadoAprovada}
		repo.anchorsByThesis["t1"] = []ThesisAnchor{{ID: "a1", ThesisID: "t1"}}
		repo.segmentsByDraft["draft-1"] = []DraftSegment{{ID: "seg1", DraftID: "draft-1", ThesisID: "t1"}}
		repo.anchorsBySegment["seg1"] = []SegmentAnchor{{ID: "sa1", DraftSegmentID: "seg1", ThesisAnchorID: "a1"}}
		outbox := &recordingOutbox{}
		uc := newUseCase(repo, outbox)

		cov, err := uc.CheckCoverage(context.Background(), "tenant-1", "t1")
		if err != nil {
			t.Fatalf("CheckCoverage() error = %v", err)
		}
		if cov.Resultado != CoverageCoberta {
			t.Errorf("Resultado = %q, want coberta", cov.Resultado)
		}
		if len(outbox.published) != 1 || outbox.published[0].Type() != TypeThesisCoverageChecked {
			t.Errorf("published = %+v, want one thesis.coverage_checked event", outbox.published)
		}
	})

	t.Run("segments of OTHER theses on the same draft are ignored", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.theses["t1"] = &Thesis{ID: "t1", TenantID: "tenant-1", DraftID: "draft-1", Estado: EstadoAprovada}
		repo.theses["t2"] = &Thesis{ID: "t2", TenantID: "tenant-1", DraftID: "draft-1", Estado: EstadoAprovada}
		repo.segmentsByDraft["draft-1"] = []DraftSegment{{ID: "seg-other", DraftID: "draft-1", ThesisID: "t2"}}
		uc := newUseCase(repo, &recordingOutbox{})

		cov, err := uc.CheckCoverage(context.Background(), "tenant-1", "t1")
		if err != nil {
			t.Fatalf("CheckCoverage() error = %v", err)
		}
		if cov.Resultado != CoverageAusente {
			t.Errorf("Resultado = %q, want ausente (t1 has no segment of its own)", cov.Resultado)
		}
	})
}
