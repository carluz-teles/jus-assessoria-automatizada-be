package extraction

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// --- fakes ------------------------------------------------------------------

// fakeUOW runs fn with a nil tx (the fakes never touch it) and records each Do's RLS scope.
// err, when set, fails the FIRST Do only, so a test can fail the finalize/EXTRACTING tx
// independently of the failure tx (which the fail() path opens fresh).
type fakeUOW struct {
	scopes  []string
	errs    []error // per-call errors, popped in order; missing entries are nil
	calls   int
	doCount int
}

func (u *fakeUOW) Do(_ context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.scopes = append(u.scopes, tenantID)
	u.doCount++
	var forced error
	if u.calls < len(u.errs) {
		forced = u.errs[u.calls]
	}
	u.calls++
	if forced != nil {
		return forced
	}
	return fn(nil)
}

func (u *fakeUOW) DoSystem(_ context.Context, fn func(tx database.Tx) error) error {
	return fn(nil)
}

// fakeDedup reports first-seen by default; set seen=true to model an at-least-once replay.
type fakeDedup struct {
	seen   bool
	err    error
	marked []string
}

func (d *fakeDedup) SeenOrMark(_ context.Context, _ database.Tx, _, eventID string) (bool, error) {
	d.marked = append(d.marked, eventID)
	return d.seen, d.err
}

// fakeOutbox captures every published event so a test can assert the contract.
type fakeOutbox struct {
	published []events.Event
	err       error
}

func (o *fakeOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	o.published = append(o.published, ev)
	return o.err
}

// fakeStore records puts and returns canned bytes for Get.
type fakeStore struct {
	getBytes []byte
	getErr   error
	putErr   error
	puts     map[string][]byte
}

func (s *fakeStore) Get(context.Context, string) ([]byte, error) {
	return s.getBytes, s.getErr
}

func (s *fakeStore) Put(_ context.Context, key string, body []byte, _ string) error {
	if s.putErr != nil {
		return s.putErr
	}
	if s.puts == nil {
		s.puts = map[string][]byte{}
	}
	s.puts[key] = body
	return nil
}

// fakeRepo records which transition ran and can force an error on any of them.
type fakeRepo struct {
	extractingCalls int
	extractedParams *MarkExtractedParams
	failedJSON      string
	failedCalls     int
	extractingErr   error
	extractedErr    error
	failedErr       error
}

func (r *fakeRepo) MarkExtracting(context.Context, database.Tx, string, string) error {
	r.extractingCalls++
	return r.extractingErr
}

func (r *fakeRepo) MarkExtracted(_ context.Context, _ database.Tx, p MarkExtractedParams) error {
	r.extractedParams = &p
	return r.extractedErr
}

func (r *fakeRepo) MarkFailed(_ context.Context, _ database.Tx, _, _, errorJSON string) error {
	r.failedCalls++
	r.failedJSON = errorJSON
	return r.failedErr
}

// fakeExtractor returns canned per-page text / hasTextLayer / version, or an error.
type fakeExtractor struct {
	pages        []PageText
	hasTextLayer bool
	version      string
	err          error
	calls        int
}

func (e *fakeExtractor) Extract(context.Context, []byte) ([]PageText, bool, string, error) {
	e.calls++
	return e.pages, e.hasTextLayer, e.version, e.err
}

func uploadedFixture() DocumentUploaded {
	return DocumentUploaded{
		Base:          events.Base{EventID: "evt-1", Aggregate: "11111111-1111-1111-1111-111111111111"},
		DocumentID:    "11111111-1111-1111-1111-111111111111",
		TenantID:      "tenant-1",
		CourtRecordID: "cr-1",
		StorageKey:    "tenant-1/documents/abc",
	}
}

// --- tests ------------------------------------------------------------------

// TestOnDocumentUploaded_TextLayerHappyPath: a PDF with a text layer extracts, persists the
// text JSON to <storage_key>.text.json, finalizes EXTRACTED with the metadata, and emits
// document.extracted (has_text_layer=true, the text_key + pages carried).
func TestOnDocumentUploaded_TextLayerHappyPath(t *testing.T) {
	uow := &fakeUOW{}
	store := &fakeStore{getBytes: []byte("%PDF-fake")}
	repo := &fakeRepo{}
	outbox := &fakeOutbox{}
	extractor := &fakeExtractor{
		pages:        []PageText{{Page: 1, Text: "hello"}, {Page: 2, Text: "world"}},
		hasTextLayer: true,
		version:      "pdftext-v1",
	}
	uc := NewUseCase(uow, store, repo, extractor, &fakeDedup{}, outbox)

	ev := uploadedFixture()
	if err := uc.OnDocumentUploaded(context.Background(), ev); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}

	if repo.extractingCalls != 1 {
		t.Errorf("MarkExtracting calls = %d, want 1", repo.extractingCalls)
	}
	wantKey := ev.StorageKey + textKeySuffix
	body, ok := store.puts[wantKey]
	if !ok {
		t.Fatalf("text json not written to %q; puts = %v", wantKey, store.puts)
	}
	var td textDocument
	if err := json.Unmarshal(body, &td); err != nil {
		t.Fatalf("text json unmarshal: %v", err)
	}
	if td.ExtractorVersion != "pdftext-v1" || len(td.Pages) != 2 || td.Pages[0].Text != "hello" {
		t.Errorf("text json = %+v, want v1 + 2 pages", td)
	}
	if repo.extractedParams == nil {
		t.Fatal("MarkExtracted not called")
	}
	if repo.extractedParams.Pages != 2 || !repo.extractedParams.HasTextLayer || repo.extractedParams.ExtractorVersion != "pdftext-v1" {
		t.Errorf("MarkExtracted params = %+v", *repo.extractedParams)
	}
	if repo.failedCalls != 0 {
		t.Errorf("MarkFailed called on happy path (calls = %d)", repo.failedCalls)
	}

	if len(outbox.published) != 1 {
		t.Fatalf("published = %d events, want 1", len(outbox.published))
	}
	ext, ok := outbox.published[0].(DocumentExtracted)
	if !ok {
		t.Fatalf("published event type = %T, want DocumentExtracted", outbox.published[0])
	}
	if ext.Type() != "document.extracted" || ext.TextKey != wantKey || ext.Pages != 2 ||
		!ext.HasTextLayer || ext.ExtractorVersion != "pdftext-v1" || ext.DocumentID != ev.DocumentID ||
		ext.TenantID != ev.TenantID || ext.CourtRecordID != ev.CourtRecordID {
		t.Errorf("document.extracted = %+v", ext)
	}
	if ext.AggregateType() != "document" || ext.AggregateID() != ev.Aggregate {
		t.Errorf("aggregate = %s/%s, want document/%s", ext.AggregateType(), ext.AggregateID(), ev.Aggregate)
	}
}

// TestOnDocumentUploaded_OCRFallback: the dispatcher's outcome is hasTextLayer=false (a scan
// OCR'd), so the row + event carry has_text_layer=false and the OCR version. Uses a fake
// extractor standing in for the composite — the dispatcher's own routing is covered in
// dispatcher_test.
func TestOnDocumentUploaded_OCRFallback(t *testing.T) {
	uow := &fakeUOW{}
	store := &fakeStore{getBytes: []byte("%PDF-scan")}
	repo := &fakeRepo{}
	outbox := &fakeOutbox{}
	extractor := &fakeExtractor{
		pages:        []PageText{{Page: 1, Text: "ocr text"}},
		hasTextLayer: false,
		version:      "claude-ocr-opus-4-8",
	}
	uc := NewUseCase(uow, store, repo, extractor, &fakeDedup{}, outbox)

	if err := uc.OnDocumentUploaded(context.Background(), uploadedFixture()); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}

	if repo.extractedParams == nil || repo.extractedParams.HasTextLayer {
		t.Errorf("MarkExtracted has_text_layer = true, want false (OCR)")
	}
	if repo.extractedParams.ExtractorVersion != "claude-ocr-opus-4-8" {
		t.Errorf("version = %q, want claude-ocr-opus-4-8", repo.extractedParams.ExtractorVersion)
	}
	ext := outbox.published[0].(DocumentExtracted)
	if ext.HasTextLayer || ext.ExtractorVersion != "claude-ocr-opus-4-8" {
		t.Errorf("document.extracted = %+v, want OCR/no-text-layer", ext)
	}
}

// TestOnDocumentUploaded_Failure: an extractor error routes to the FAILED path — MarkFailed
// records {stage,message}, document.failed is emitted, and the ORIGINAL error is returned so
// asynq retries. Covers extract, storage-get and storage-put faults via a table.
func TestOnDocumentUploaded_Failure(t *testing.T) {
	cause := apperr.NewInfra("boom", errors.New("root"))

	tests := []struct {
		name  string
		store *fakeStore
		extr  *fakeExtractor
	}{
		{
			name:  "extractor error",
			store: &fakeStore{getBytes: []byte("pdf")},
			extr:  &fakeExtractor{err: cause},
		},
		{
			name:  "storage get error",
			store: &fakeStore{getErr: cause},
			extr:  &fakeExtractor{pages: []PageText{{Page: 1}}, hasTextLayer: true, version: "pdftext-v1"},
		},
		{
			name:  "storage put error",
			store: &fakeStore{getBytes: []byte("pdf"), putErr: cause},
			extr:  &fakeExtractor{pages: []PageText{{Page: 1}}, hasTextLayer: true, version: "pdftext-v1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := &fakeUOW{}
			repo := &fakeRepo{}
			outbox := &fakeOutbox{}
			uc := NewUseCase(uow, tt.store, repo, tt.extr, &fakeDedup{}, outbox)

			err := uc.OnDocumentUploaded(context.Background(), uploadedFixture())

			if !errors.Is(err, cause) {
				t.Fatalf("returned err = %v, want the original cause in chain", err)
			}
			if repo.failedCalls != 1 {
				t.Errorf("MarkFailed calls = %d, want 1", repo.failedCalls)
			}
			var body struct{ Stage, Message string }
			if e := json.Unmarshal([]byte(repo.failedJSON), &body); e != nil {
				t.Fatalf("failed json unmarshal: %v (raw=%q)", e, repo.failedJSON)
			}
			if body.Stage != "extraction" {
				t.Errorf("failed stage = %q, want extraction", body.Stage)
			}
			if len(outbox.published) != 1 {
				t.Fatalf("published = %d, want 1 (document.failed)", len(outbox.published))
			}
			fe, ok := outbox.published[0].(DocumentFailed)
			if !ok || fe.Type() != "document.failed" || fe.Stage != "extraction" || fe.Error == "" {
				t.Errorf("document.failed = %+v", outbox.published[0])
			}
			if repo.extractedParams != nil {
				t.Errorf("MarkExtracted ran despite failure: %+v", *repo.extractedParams)
			}
		})
	}
}

// TestOnDocumentUploaded_Idempotency: a duplicate delivery (dedup reports seen) short-circuits
// BEFORE any effect — no extraction, no storage write, no transition, no event, and it acks
// (nil).
func TestOnDocumentUploaded_Idempotency(t *testing.T) {
	uow := &fakeUOW{}
	store := &fakeStore{getBytes: []byte("pdf")}
	repo := &fakeRepo{}
	outbox := &fakeOutbox{}
	extractor := &fakeExtractor{pages: []PageText{{Page: 1}}, hasTextLayer: true, version: "pdftext-v1"}
	uc := NewUseCase(uow, store, repo, extractor, &fakeDedup{seen: true}, outbox)

	if err := uc.OnDocumentUploaded(context.Background(), uploadedFixture()); err != nil {
		t.Fatalf("err = %v, want nil (replay acks)", err)
	}

	if repo.extractingCalls != 0 {
		t.Errorf("MarkExtracting ran on replay (calls = %d)", repo.extractingCalls)
	}
	if extractor.calls != 0 {
		t.Errorf("extractor ran on replay (calls = %d)", extractor.calls)
	}
	if len(store.puts) != 0 {
		t.Errorf("storage written on replay: %v", store.puts)
	}
	if repo.extractedParams != nil || repo.failedCalls != 0 {
		t.Errorf("row transitioned on replay")
	}
	if len(outbox.published) != 0 {
		t.Errorf("event published on replay: %v", outbox.published)
	}
	if uow.doCount != 1 {
		t.Errorf("uow.Do count = %d, want 1 (only the dedup tx)", uow.doCount)
	}
}

// TestOnDocumentUploaded_TenantScopedTx: every uow.Do on the happy path is scoped to the
// event's tenant (RLS barrier) — never an empty or foreign scope.
func TestOnDocumentUploaded_TenantScopedTx(t *testing.T) {
	uow := &fakeUOW{}
	uc := NewUseCase(uow, &fakeStore{getBytes: []byte("pdf")}, &fakeRepo{},
		&fakeExtractor{pages: []PageText{{Page: 1}}, hasTextLayer: true, version: "pdftext-v1"},
		&fakeDedup{}, &fakeOutbox{})

	if err := uc.OnDocumentUploaded(context.Background(), uploadedFixture()); err != nil {
		t.Fatalf("err = %v", err)
	}
	for i, s := range uow.scopes {
		if s != "tenant-1" {
			t.Errorf("Do #%d scope = %q, want tenant-1", i, s)
		}
	}
}
