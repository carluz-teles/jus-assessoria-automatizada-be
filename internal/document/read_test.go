package document

import (
	"context"
	"testing"
	"time"
)

// fakeReadRepo primes the list/count/detail reads and records the query it was asked (to prove
// the over-fetch limit).
type fakeReadRepo struct {
	list      []DocumentView
	listErr   error
	gotQ      DocumentsByProcessoQuery
	total     int64
	countErr  error
	detail    DocumentView
	detailErr error
	chunks    []string
	chunksErr error
}

func (r *fakeReadRepo) ListDocumentsByProcesso(_ context.Context, q DocumentsByProcessoQuery) ([]DocumentView, error) {
	r.gotQ = q
	return r.list, r.listErr
}

func (r *fakeReadRepo) CountDocumentsByProcesso(_ context.Context, _, _ string) (int64, error) {
	return r.total, r.countErr
}

func (r *fakeReadRepo) GetDocument(_ context.Context, _, _ string) (DocumentView, error) {
	return r.detail, r.detailErr
}

func (r *fakeReadRepo) GetDocumentChunks(_ context.Context, _, _ string) ([]string, error) {
	return r.chunks, r.chunksErr
}

// TestDocumentsByProcesso_OverFetchesForHasMore proves the read use case asks for limit+1 rows,
// trims to limit, and reports HasMore when the repo returned the extra row.
func TestDocumentsByProcesso_OverFetchesForHasMore(t *testing.T) {
	now := time.Now()
	repo := &fakeReadRepo{
		list: []DocumentView{
			{ID: "d1", CreatedAt: now}, {ID: "d2", CreatedAt: now}, {ID: "d3", CreatedAt: now},
		},
		total: 10,
	}
	uc := NewReadUseCase(repo)

	res, err := uc.DocumentsByProcesso(context.Background(), DocumentsByProcessoQuery{
		TenantID: "t", CourtRecordID: "c", Limit: 2,
	})
	if err != nil {
		t.Fatalf("DocumentsByProcesso() error = %v", err)
	}
	if repo.gotQ.Limit != 3 {
		t.Errorf("repo limit = %d, want 3 (over-fetch)", repo.gotQ.Limit)
	}
	if len(res.Items) != 2 || !res.HasMore || res.Total != 10 {
		t.Errorf("result items/hasMore/total = %d/%v/%d, want 2/true/10", len(res.Items), res.HasMore, res.Total)
	}
}

// TestDocumentsByProcesso_NoMore proves a full-but-not-over page reports HasMore=false.
func TestDocumentsByProcesso_NoMore(t *testing.T) {
	repo := &fakeReadRepo{list: []DocumentView{{ID: "d1"}}, total: 1}
	uc := NewReadUseCase(repo)

	res, err := uc.DocumentsByProcesso(context.Background(), DocumentsByProcessoQuery{Limit: 5})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if res.HasMore || len(res.Items) != 1 {
		t.Errorf("hasMore/items = %v/%d, want false/1", res.HasMore, len(res.Items))
	}
}

// TestDocument_PassThrough proves Document forwards to the repo (incl. the not-found error).
func TestDocument_PassThrough(t *testing.T) {
	repo := &fakeReadRepo{detailErr: ErrDocumentNotFound}
	uc := NewReadUseCase(repo)

	if _, err := uc.Document(context.Background(), "t", "d"); err != ErrDocumentNotFound {
		t.Fatalf("error = %v, want ErrDocumentNotFound", err)
	}
}

// TestDocumentContent_JoinsChunks proves the page texts concatenate in order joined by a blank
// line.
func TestDocumentContent_JoinsChunks(t *testing.T) {
	repo := &fakeReadRepo{chunks: []string{"page one", "page two"}}
	uc := NewReadUseCase(repo)

	got, err := uc.DocumentContent(context.Background(), "t", "d")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if want := "page one\n\npage two"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestDocumentContent_EmptyButExists proves that when no chunks exist yet but the document is
// live, the use case returns an empty string (not a 404).
func TestDocumentContent_EmptyButExists(t *testing.T) {
	repo := &fakeReadRepo{chunks: nil, detail: DocumentView{ID: "d"}}
	uc := NewReadUseCase(repo)

	got, err := uc.DocumentContent(context.Background(), "t", "d")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "" {
		t.Fatalf("content = %q, want empty", got)
	}
}

// TestDocumentContent_NotFound proves that no chunks + an unknown document propagates the typed
// not-found (→ 404).
func TestDocumentContent_NotFound(t *testing.T) {
	repo := &fakeReadRepo{chunks: nil, detailErr: ErrDocumentNotFound}
	uc := NewReadUseCase(repo)

	if _, err := uc.DocumentContent(context.Background(), "t", "d"); err != ErrDocumentNotFound {
		t.Fatalf("error = %v, want ErrDocumentNotFound", err)
	}
}
