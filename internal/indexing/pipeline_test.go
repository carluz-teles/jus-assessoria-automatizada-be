package indexing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// pipeline_test.go covers the use case end-to-end with fakes (no storage, no Voyage, no DB):
// happy path (chunks inserted + CHUNKED→READY + document.ready with chunk_count), dedup replay,
// empty extraction (READY with 0 chunks, no INSERT), embed failure (FAILED + document.failed),
// idempotent ON CONFLICT, and the terminal-vs-retryable error classification.

const (
	testTenant = "11111111-1111-1111-1111-111111111111"
	testDoc    = "22222222-2222-2222-2222-222222222222"
)

// --- fakes ------------------------------------------------------------------

// fakeReader returns canned bytes for the extracted-text object (or an error).
type fakeReader struct {
	bytes []byte
	err   error
}

func (r *fakeReader) ReadObject(_ context.Context, _ string) ([]byte, error) {
	return r.bytes, r.err
}

// fakeEmbedder returns one fixed-width vector per input (or an error). It records the batch it
// was asked to embed so a test can assert the chunk texts reached it.
type fakeEmbedder struct {
	model     string
	err       error
	shortBy   int // return len(texts)-shortBy vectors to model a contract violation
	gotBatch  [][]string
	gotInput  []InputType
	dimension int
}

func (e *fakeEmbedder) Embed(_ context.Context, texts []string, inputType InputType) ([][]float32, string, error) {
	e.gotBatch = append(e.gotBatch, append([]string(nil), texts...))
	e.gotInput = append(e.gotInput, inputType)
	if e.err != nil {
		return nil, "", e.err
	}
	n := len(texts) - e.shortBy
	dim := e.dimension
	if dim == 0 {
		dim = 4
	}
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = make([]float32, dim)
	}
	model := e.model
	if model == "" {
		model = "voyage-3.5-lite"
	}
	return vecs, model, nil
}

// fakeUOW runs fn with a nil tx (the fake repo/dedup/outbox never touch it) and records the RLS
// scope each Do asked for. Mirrors internal/deadline's fakeUOW.
type fakeUOW struct {
	scopes []string
	err    error
}

func (u *fakeUOW) Do(_ context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.scopes = append(u.scopes, tenantID)
	if u.err != nil {
		return u.err
	}
	return fn(nil)
}

// fakeDedup reports every event first-seen by default; set seen=true to model a replay.
type fakeDedup struct {
	seen   bool
	err    error
	marked []string
}

func (d *fakeDedup) SeenOrMark(_ context.Context, _ database.Tx, _ string, eventID string) (bool, error) {
	d.marked = append(d.marked, eventID)
	return d.seen, d.err
}

// fakeRepo records the writes: the chunk rows inserted, the status transitions, the failure.
type fakeRepo struct {
	insertErr error
	statusErr error
	inserted  [][]ChunkRow
	conflicts int // rows to subtract from the reported insert count (ON CONFLICT skips)
	statuses  []string
	failed    []failRecord
}

type failRecord struct {
	stage, message string
}

func (r *fakeRepo) InsertChunks(_ context.Context, _ database.Tx, rows []ChunkRow) (int, error) {
	if r.insertErr != nil {
		return 0, r.insertErr
	}
	r.inserted = append(r.inserted, rows)
	return len(rows) - r.conflicts, nil
}

func (r *fakeRepo) SetStatus(_ context.Context, _ database.Tx, _, _, status string) error {
	if r.statusErr != nil {
		return r.statusErr
	}
	r.statuses = append(r.statuses, status)
	return nil
}

func (r *fakeRepo) SetFailed(_ context.Context, _ database.Tx, _, _, stage, message string) error {
	r.failed = append(r.failed, failRecord{stage: stage, message: message})
	return nil
}

// fakeOutbox captures every published event so a test can assert the document.ready/failed
// contract.
type fakeOutbox struct {
	published []events.Event
	err       error
}

func (o *fakeOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	o.published = append(o.published, ev)
	return o.err
}

// --- fixtures ---------------------------------------------------------------

func extractedFixture() DocumentExtracted {
	return DocumentExtracted{
		Base:             events.Base{EventID: "evt-1", Aggregate: testDoc},
		DocumentID:       testDoc,
		TenantID:         testTenant,
		TextKey:          testTenant + "/text/abc.json",
		Pages:            2,
		HasTextLayer:     true,
		ExtractorVersion: "v1",
	}
}

func textJSON(t *testing.T, pages []ExtractedPage) []byte {
	t.Helper()
	b, err := json.Marshal(ExtractedText{ExtractorVersion: "v1", Pages: pages})
	if err != nil {
		t.Fatalf("marshal text json: %v", err)
	}
	return b
}

func longText(n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte('a' + (i % 26))
	}
	return string(buf)
}

// newUC assembles a UseCase over the given fakes with dim=4.
func newUC(reader objectReader, embed Embedder, uow unitOfWork, dedup deduper, repo repository, ob outbox) *UseCase {
	uc, err := NewUseCase(reader, embed, uow, dedup, repo, ob, 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		panic("indexing test: NewUseCase: " + err.Error())
	}
	return uc
}

// --- tests ------------------------------------------------------------------

func TestOnDocumentExtracted_HappyPath(t *testing.T) {
	t.Parallel()

	// Two pages, each big enough to produce >1 chunk.
	body := textJSON(t, []ExtractedPage{
		{Page: 1, Text: longText(2000)},
		{Page: 2, Text: longText(1200)},
	})
	reader := &fakeReader{bytes: body}
	embed := &fakeEmbedder{model: "voyage-3.5-lite"}
	uow := &fakeUOW{}
	dedup := &fakeDedup{}
	repo := &fakeRepo{}
	ob := &fakeOutbox{}
	uc := newUC(reader, embed, uow, dedup, repo, ob)

	if err := uc.OnDocumentExtracted(context.Background(), extractedFixture()); err != nil {
		t.Fatalf("OnDocumentExtracted: %v", err)
	}

	// Chunks were inserted (one InsertChunks call with all rows).
	if len(repo.inserted) != 1 {
		t.Fatalf("InsertChunks called %d times, want 1", len(repo.inserted))
	}
	nChunks := len(repo.inserted[0])
	if nChunks < 3 {
		t.Fatalf("expected multiple chunks across 2 pages, got %d", nChunks)
	}
	// Every row carries the model, dim and a hash.
	for i, row := range repo.inserted[0] {
		if row.EmbeddingModel != "voyage-3.5-lite" {
			t.Errorf("row %d model = %q", i, row.EmbeddingModel)
		}
		if row.Dim != 4 {
			t.Errorf("row %d dim = %d, want 4", i, row.Dim)
		}
		if row.Hash == "" || len(row.Embedding) != 4 {
			t.Errorf("row %d bad hash/embedding", i)
		}
	}

	// CHUNKED then READY, in order.
	if len(repo.statuses) != 2 || repo.statuses[0] != statusChunked || repo.statuses[1] != statusReady {
		t.Fatalf("statuses = %v, want [CHUNKED READY]", repo.statuses)
	}

	// document.ready published with the chunk count + model.
	if len(ob.published) != 1 {
		t.Fatalf("published %d events, want 1", len(ob.published))
	}
	ready, ok := ob.published[0].(DocumentReady)
	if !ok {
		t.Fatalf("published %T, want DocumentReady", ob.published[0])
	}
	if ready.ChunkCount != nChunks {
		t.Errorf("chunk_count = %d, want %d", ready.ChunkCount, nChunks)
	}
	if ready.EmbeddingModel != "voyage-3.5-lite" || ready.DocumentID != testDoc || ready.TenantID != testTenant {
		t.Errorf("ready event fields wrong: %+v", ready)
	}
	if ready.Type() != TypeDocumentReady {
		t.Errorf("type = %q, want %q", ready.Type(), TypeDocumentReady)
	}

	// The tx was tenant-scoped.
	if len(uow.scopes) != 1 || uow.scopes[0] != testTenant {
		t.Errorf("scopes = %v, want [%s]", uow.scopes, testTenant)
	}

	// The indexing side embeds as documents (never as a query) — the corpus half of the
	// asymmetric retrieval. A regression here silently degrades RAG recall.
	for i, it := range embed.gotInput {
		if it != InputDocument {
			t.Errorf("embed call %d input type = %q, want %q", i, it, InputDocument)
		}
	}
}

func TestOnDocumentExtracted_Dedup(t *testing.T) {
	t.Parallel()

	reader := &fakeReader{bytes: textJSON(t, []ExtractedPage{{Page: 1, Text: longText(2000)}})}
	repo := &fakeRepo{}
	ob := &fakeOutbox{}
	uc := newUC(reader, &fakeEmbedder{}, &fakeUOW{}, &fakeDedup{seen: true}, repo, ob)

	if err := uc.OnDocumentExtracted(context.Background(), extractedFixture()); err != nil {
		t.Fatalf("OnDocumentExtracted: %v", err)
	}
	// A replay writes nothing and publishes nothing.
	if len(repo.inserted) != 0 || len(repo.statuses) != 0 {
		t.Errorf("replay wrote chunks/statuses: inserted=%v statuses=%v", repo.inserted, repo.statuses)
	}
	if len(ob.published) != 0 {
		t.Errorf("replay published %d events, want 0", len(ob.published))
	}
}

func TestOnDocumentExtracted_EmptyExtraction(t *testing.T) {
	t.Parallel()

	// Pages with no text → no chunks → READY with 0 chunks, no INSERT, no CHUNKED.
	reader := &fakeReader{bytes: textJSON(t, []ExtractedPage{{Page: 1, Text: ""}})}
	repo := &fakeRepo{}
	ob := &fakeOutbox{}
	uc := newUC(reader, &fakeEmbedder{}, &fakeUOW{}, &fakeDedup{}, repo, ob)

	if err := uc.OnDocumentExtracted(context.Background(), extractedFixture()); err != nil {
		t.Fatalf("OnDocumentExtracted: %v", err)
	}
	if len(repo.inserted) != 0 {
		t.Errorf("empty extraction inserted chunks: %v", repo.inserted)
	}
	// Only READY (no CHUNKED, since no chunks were persisted).
	if len(repo.statuses) != 1 || repo.statuses[0] != statusReady {
		t.Errorf("statuses = %v, want [READY]", repo.statuses)
	}
	ready, ok := ob.published[0].(DocumentReady)
	if !ok || ready.ChunkCount != 0 {
		t.Errorf("want DocumentReady chunk_count=0, got %+v", ob.published)
	}
}

func TestOnDocumentExtracted_EmbedFailure(t *testing.T) {
	t.Parallel()

	reader := &fakeReader{bytes: textJSON(t, []ExtractedPage{{Page: 1, Text: longText(2000)}})}
	embedErr := apperr.NewInfra("voyage down", errors.New("boom"))
	repo := &fakeRepo{}
	ob := &fakeOutbox{}
	uc := newUC(reader, &fakeEmbedder{err: embedErr}, &fakeUOW{}, &fakeDedup{}, repo, ob)

	err := uc.OnDocumentExtracted(context.Background(), extractedFixture())
	if err == nil {
		t.Fatal("expected error on embed failure")
	}
	// No chunks inserted, no READY.
	if len(repo.inserted) != 0 {
		t.Errorf("inserted chunks despite embed failure: %v", repo.inserted)
	}
	// FAILED recorded with stage=indexing.
	if len(repo.failed) != 1 || repo.failed[0].stage != stageIndexing {
		t.Fatalf("SetFailed = %v, want one indexing failure", repo.failed)
	}
	// document.failed published.
	if len(ob.published) != 1 {
		t.Fatalf("published %d events, want 1 (document.failed)", len(ob.published))
	}
	failed, ok := ob.published[0].(DocumentFailed)
	if !ok {
		t.Fatalf("published %T, want DocumentFailed", ob.published[0])
	}
	if failed.Stage != stageIndexing || failed.DocumentID != testDoc || failed.Error == "" {
		t.Errorf("failed event fields wrong: %+v", failed)
	}
	if failed.Type() != TypeDocumentFailed {
		t.Errorf("type = %q, want %q", failed.Type(), TypeDocumentFailed)
	}
	// The embed error is infra → NOT terminal → retryable.
	if isTerminal(err) {
		t.Errorf("infra embed error classified terminal")
	}
}

func TestOnDocumentExtracted_MalformedTextIsTerminal(t *testing.T) {
	t.Parallel()

	reader := &fakeReader{bytes: []byte("not json {{{")}
	repo := &fakeRepo{}
	ob := &fakeOutbox{}
	uc := newUC(reader, &fakeEmbedder{}, &fakeUOW{}, &fakeDedup{}, repo, ob)

	err := uc.OnDocumentExtracted(context.Background(), extractedFixture())
	if err == nil {
		t.Fatal("expected error on malformed text json")
	}
	if !isTerminal(err) {
		t.Errorf("malformed text json should be terminal (KindInvalid)")
	}
	// It still records FAILED + document.failed.
	if len(repo.failed) != 1 {
		t.Errorf("SetFailed = %v, want one failure", repo.failed)
	}
}

func TestOnDocumentExtracted_ConflictIdempotent(t *testing.T) {
	t.Parallel()

	// The repo reports all rows as conflicts (already indexed), but the saga still completes:
	// CHUNKED→READY and document.ready fire with the FULL chunk count (a reprocess reports its
	// intended index size, not the ON-CONFLICT-adjusted 0).
	reader := &fakeReader{bytes: textJSON(t, []ExtractedPage{{Page: 1, Text: longText(2000)}})}
	repo := &fakeRepo{conflicts: 1 << 30} // more than any row count → insert count clamps low
	ob := &fakeOutbox{}
	uc := newUC(reader, &fakeEmbedder{}, &fakeUOW{}, &fakeDedup{}, repo, ob)

	if err := uc.OnDocumentExtracted(context.Background(), extractedFixture()); err != nil {
		t.Fatalf("OnDocumentExtracted: %v", err)
	}
	if len(repo.statuses) != 2 || repo.statuses[1] != statusReady {
		t.Errorf("statuses = %v, want [CHUNKED READY]", repo.statuses)
	}
	ready := ob.published[0].(DocumentReady)
	if ready.ChunkCount != len(repo.inserted[0]) {
		t.Errorf("chunk_count = %d, want full count %d", ready.ChunkCount, len(repo.inserted[0]))
	}
}

func TestOnDocumentExtracted_StorageReadRetryable(t *testing.T) {
	t.Parallel()

	reader := &fakeReader{err: apperr.NewInfra("storage down", errors.New("boom"))}
	repo := &fakeRepo{}
	ob := &fakeOutbox{}
	uc := newUC(reader, &fakeEmbedder{}, &fakeUOW{}, &fakeDedup{}, repo, ob)

	err := uc.OnDocumentExtracted(context.Background(), extractedFixture())
	if err == nil {
		t.Fatal("expected error on storage read failure")
	}
	if isTerminal(err) {
		t.Errorf("infra storage error classified terminal")
	}
	// FAILED + document.failed still recorded.
	if len(repo.failed) != 1 || len(ob.published) != 1 {
		t.Errorf("want one FAILED + one document.failed, got failed=%v published=%d", repo.failed, len(ob.published))
	}
}

func TestOnDocumentExtracted_EmbedCountMismatchTerminal(t *testing.T) {
	t.Parallel()

	reader := &fakeReader{bytes: textJSON(t, []ExtractedPage{{Page: 1, Text: longText(2000)}})}
	repo := &fakeRepo{}
	ob := &fakeOutbox{}
	// Embedder returns one fewer vector than inputs → contract violation → terminal.
	uc := newUC(reader, &fakeEmbedder{shortBy: 1}, &fakeUOW{}, &fakeDedup{}, repo, ob)

	err := uc.OnDocumentExtracted(context.Background(), extractedFixture())
	if err == nil {
		t.Fatal("expected error on vector count mismatch")
	}
	if !isTerminal(err) {
		t.Errorf("embedder contract violation should be terminal (KindInvalid)")
	}
}
