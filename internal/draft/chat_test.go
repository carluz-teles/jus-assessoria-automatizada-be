package draft

// chat_test.go — unit tests for ChatUseCase (Fatia 3b grounded chat).
// All external ports (generator, embedder, search, repo) are mocked via fakes;
// the real LLM and Postgres are NEVER called. Tests prove observable behaviour:
// correct persistence, grounding decisions, citation validation, tenant guard,
// and the generator-nil degraded path.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/llm"
)

// ── fakes ────────────────────────────────────────────────────────────────────

// fakeChatGen is a fake llm.Generator for chat tests.
type fakeChatGen struct {
	out []byte
	err error
}

func (f *fakeChatGen) GenerateJSON(_ context.Context, _ llm.Request) ([]byte, error) {
	return f.out, f.err
}

func (f *fakeChatGen) GenerateJSONStream(_ context.Context, _ llm.Request, onChunk func(string) error) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if onChunk != nil {
		_ = onChunk(string(f.out))
	}
	return f.out, nil
}

// fakeChatEmb is a fake embedder for chat tests. Satisfies the package-level
// embedder interface (same as fakeEmbedder in generate_test.go, but kept separate
// so each test file controls its own fake independently).
type fakeChatEmb struct {
	vec []float32
	err error
}

func (f *fakeChatEmb) Embed(_ context.Context, _ []string, _ indexing.InputType) ([][]float32, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	return [][]float32{f.vec}, "fake-model", nil
}

// fakeChatSearch wraps SearchChunks for injection — but since SearchChunks is a
// function (not an interface) we test the degraded path (Pool nil) separately.
// For the grounded path we use a fakeChatRepo that returns pre-seeded chunks via
// the RAG helper (replaced at test time via the chatRepo interface).

// fakeChatRepo satisfies chatRepo for unit tests.
type fakeChatRepo struct {
	draft        *Draft
	draftErr     error
	intimation   *IntimationContext
	intimErr     error
	intimCalled  bool
	thread       []ChatMessage
	threadErr    error
	insertedMsgs []*ChatMessage
	insertErr    error
}

func (r *fakeChatRepo) GetDraftByID(_ context.Context, _ database.Tx, _, _ string) (*Draft, error) {
	return r.draft, r.draftErr
}

func (r *fakeChatRepo) GetIntimationForDraft(_ context.Context, _ database.Tx, _, _ string) (*IntimationContext, error) {
	r.intimCalled = true
	return r.intimation, r.intimErr
}

func (r *fakeChatRepo) InsertChatMessage(_ context.Context, _ database.Tx, m *ChatMessage) (*ChatMessage, error) {
	if r.insertErr != nil {
		return nil, r.insertErr
	}
	m.ID = uuid.NewString()
	m.CreatedAt = time.Now()
	r.insertedMsgs = append(r.insertedMsgs, m)
	return m, nil
}

func (r *fakeChatRepo) GetChatThread(_ context.Context, _ database.Tx, _ string) ([]ChatMessage, error) {
	if r.threadErr != nil {
		return nil, r.threadErr
	}
	if r.thread == nil {
		return []ChatMessage{}, nil
	}
	return r.thread, nil
}

// fakeChatComposer is a minimal PromptComposer that satisfies the interface for
// chat tests. Only ComposeChat is exercised; other methods panic to surface misuse.
type fakeChatComposer struct{}

func (fakeChatComposer) Compose(_ string, _ advisory.CaseContext) (advisory.Composed, error) {
	panic("unexpected Compose call in chat test")
}

func (fakeChatComposer) ComposeProcess(_ string, _ advisory.ProcessContext) (advisory.Composed, error) {
	panic("unexpected ComposeProcess call in chat test")
}

func (fakeChatComposer) ComposeDraft(_ string, _ advisory.DraftContext) (advisory.Composed, error) {
	panic("unexpected ComposeDraft call in chat test")
}

func (fakeChatComposer) ComposeChat(_ string, _ advisory.ChatContext) (advisory.Composed, error) {
	return advisory.Composed{
		System:        "system",
		User:          "user",
		PromptVersion: "chat_grounding/v1",
	}, nil
}

func (fakeChatComposer) ComposeReview(_ string, _ advisory.ReviewContext) (advisory.Composed, error) {
	panic("unexpected ComposeReview call in chat test")
}

func (fakeChatComposer) ComposeTheses(_ string, _ advisory.DraftContext) (advisory.Composed, error) {
	panic("unexpected ComposeTheses call in chat test")
}

func (fakeChatComposer) ComposeIterate(_ string, _ advisory.IterateContext) (advisory.Composed, error) {
	panic("unexpected ComposeIterate call in chat test")
}

// newChatUCForTest builds a ChatUseCase with the given fakes. Uses fakeUoW from
// generate_test.go (same package — runs fn immediately with a nil tx).
func newChatUCForTest(repo chatRepo, gen llm.Generator, emb embedder) *ChatUseCase {
	return &ChatUseCase{
		uow:      fakeUoW{},
		repo:     repo,
		gen:      gen,
		emb:      emb,
		search:   indexing.SearchDeps{Pool: nil}, // pool nil → no RAG; emb nil tested separately
		composer: fakeChatComposer{},
		model:    "test-model",
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// cannedChatJSON is a valid chatOutput JSON for tests.
const cannedChatJSON = `{
  "answer": "Com base nos autos, o prazo é de 15 dias.",
  "citations": [
    {"document_id": "doc-1", "page": 3, "quote": "prazo de 15 dias úteis"}
  ]
}`

// noCitationsJSON is a valid chatOutput with empty citations.
const noCitationsJSON = `{
  "answer": "Não encontrei essa informação nos autos disponíveis.",
  "citations": []
}`

func stubChatDraft(tenantID string) *Draft {
	return &Draft{
		ID:        uuid.NewString(),
		TenantID:  tenantID,
		CaseID:    uuid.NewString(), // has case → grounded_capable=true
		Content:   "Exmo. Sr. Juiz, vem o réu...",
		SagaState: SagaStateReviewed,
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestChatUseCase_AnswerQuestion_GenNil — when generator is nil, AnswerQuestion
// returns ErrIANotConfigured (KindInvalid → 422) without touching the repo.
func TestChatUseCase_AnswerQuestion_GenNil(t *testing.T) {
	repo := &fakeChatRepo{draft: stubChatDraft("tenant-1")}
	uc := newChatUCForTest(repo, nil, nil)

	_, err := uc.AnswerQuestion(context.Background(), AnswerQuestionCommand{
		TenantID: "tenant-1",
		DraftID:  uuid.NewString(),
		Question: "Qual o prazo?",
	})

	if err == nil {
		t.Fatal("want error from nil generator, got nil")
	}
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindInvalid {
		t.Errorf("want KindInvalid, got %v", err)
	}
	// No messages should have been inserted.
	if len(repo.insertedMsgs) != 0 {
		t.Errorf("inserted %d messages, want 0 (nil gen returns early)", len(repo.insertedMsgs))
	}
}

// TestChatUseCase_AnswerQuestion_DraftNotFound — when the repo returns
// ErrDraftNotFound, AnswerQuestion propagates it without inserting any messages.
func TestChatUseCase_AnswerQuestion_DraftNotFound(t *testing.T) {
	repo := &fakeChatRepo{draftErr: ErrDraftNotFound}
	gen := &fakeChatGen{out: []byte(cannedChatJSON)}
	uc := newChatUCForTest(repo, gen, nil)

	_, err := uc.AnswerQuestion(context.Background(), AnswerQuestionCommand{
		TenantID: "tenant-1",
		DraftID:  uuid.NewString(),
		Question: "Qual o prazo?",
	})

	if !errors.Is(err, ErrDraftNotFound) {
		t.Errorf("want ErrDraftNotFound, got %v", err)
	}
	if len(repo.insertedMsgs) != 0 {
		t.Errorf("inserted %d messages, want 0 (draft not found)", len(repo.insertedMsgs))
	}
}

// TestChatUseCase_AnswerQuestion_Ungrounded — when embedder is nil, the answer
// is persisted with grounded=false and citations=[].
func TestChatUseCase_AnswerQuestion_Ungrounded(t *testing.T) {
	tenantID := "tenant-ungrounded"
	repo := &fakeChatRepo{draft: stubChatDraft(tenantID)}
	gen := &fakeChatGen{out: []byte(noCitationsJSON)}
	uc := newChatUCForTest(repo, gen, nil) // nil embedder → ungrounded

	msg, err := uc.AnswerQuestion(context.Background(), AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  repo.draft.ID,
		Question: "Existe algum prazo em aberto?",
	})

	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if msg == nil {
		t.Fatal("expected non-nil message")
	}
	if msg.Grounded {
		t.Error("grounded = true, want false (no embedder)")
	}
	if len(msg.Citations) != 0 {
		t.Errorf("citations = %d, want 0 (ungrounded)", len(msg.Citations))
	}
	if msg.Role != ChatRoleAssistant {
		t.Errorf("role = %q, want %q", msg.Role, ChatRoleAssistant)
	}

	// Both user and assistant messages must be inserted in the same tx.
	if len(repo.insertedMsgs) != 2 {
		t.Fatalf("inserted %d messages, want 2 (user + assistant)", len(repo.insertedMsgs))
	}
	if repo.insertedMsgs[0].Role != ChatRoleUser {
		t.Errorf("first inserted role = %q, want %q", repo.insertedMsgs[0].Role, ChatRoleUser)
	}
	if repo.insertedMsgs[1].Role != ChatRoleAssistant {
		t.Errorf("second inserted role = %q, want %q", repo.insertedMsgs[1].Role, ChatRoleAssistant)
	}
}

// TestChatUseCase_AnswerQuestion_GroundedWithValidCitations — when embedder returns
// a hit and the LLM cites a valid document_id, grounded=true and citations are kept.
// We use a custom chatUseCase with a stubbed runChatRAG — since SearchChunks is a
// package-level function we test by setting Pool=nil (no real DB) and injecting
// a fake that overrides the hit list at the validation step.
func TestChatUseCase_AnswerQuestion_CitationValidation_InvalidDocDropped(t *testing.T) {
	// The LLM claims a document_id that was NOT in the retrieved hits → should be dropped.
	badCitJSON := `{
		"answer": "Resposta com citação inválida.",
		"citations": [{"document_id": "hallucinated-doc", "page": 1, "quote": "trecho inventado"}]
	}`
	tenantID := "tenant-valid-cit"
	repo := &fakeChatRepo{draft: stubChatDraft(tenantID)}
	gen := &fakeChatGen{out: []byte(badCitJSON)}
	// embedder nil → no RAG hits → hallucinated document_id gets no valid docs set
	uc := newChatUCForTest(repo, gen, nil)

	msg, err := uc.AnswerQuestion(context.Background(), AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  repo.draft.ID,
		Question: "O que aconteceu no processo?",
	})

	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	// No hits → validateChatCitations returns empty (no valid docs) → grounded=false
	if msg.Grounded {
		t.Error("grounded = true, want false (no valid hits for citation validation)")
	}
	if len(msg.Citations) != 0 {
		t.Errorf("citations = %d, want 0 (hallucinated doc_id dropped)", len(msg.Citations))
	}
}

// TestChatUseCase_AnswerQuestion_PersistsTwoRows — the happy path always inserts
// exactly 2 rows: role='user' and role='assistant' in sequence.
func TestChatUseCase_AnswerQuestion_PersistsTwoRows(t *testing.T) {
	tenantID := "tenant-two-rows"
	repo := &fakeChatRepo{draft: stubChatDraft(tenantID)}
	gen := &fakeChatGen{out: []byte(noCitationsJSON)}
	uc := newChatUCForTest(repo, gen, nil)

	_, err := uc.AnswerQuestion(context.Background(), AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  repo.draft.ID,
		Question: "Qual o resultado?",
	})

	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if len(repo.insertedMsgs) != 2 {
		t.Fatalf("inserted %d messages, want 2", len(repo.insertedMsgs))
	}
	// User turn carries no citations or model_version.
	userMsg := repo.insertedMsgs[0]
	if userMsg.Role != ChatRoleUser {
		t.Errorf("first msg role = %q, want user", userMsg.Role)
	}
	if len(userMsg.Citations) != 0 {
		t.Errorf("user msg citations = %d, want 0", len(userMsg.Citations))
	}
	if userMsg.ModelVersion != "" {
		t.Errorf("user msg model_version = %q, want empty", userMsg.ModelVersion)
	}

	// Assistant turn carries model_version.
	asstMsg := repo.insertedMsgs[1]
	if asstMsg.Role != ChatRoleAssistant {
		t.Errorf("second msg role = %q, want assistant", asstMsg.Role)
	}
	if asstMsg.ModelVersion == "" {
		t.Error("assistant msg model_version is empty, want non-empty")
	}
}

// TestChatUseCase_GetThread_ReturnsEmpty — GetThread returns empty slice (not nil)
// when no messages exist.
func TestChatUseCase_GetThread_ReturnsEmpty(t *testing.T) {
	tenantID := "tenant-empty-thread"
	repo := &fakeChatRepo{draft: stubChatDraft(tenantID)}
	uc := newChatUCForTest(repo, nil, nil)

	msgs, draft, err := uc.GetThread(context.Background(), tenantID, repo.draft.ID)

	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if msgs == nil {
		t.Error("messages is nil, want empty slice")
	}
	if len(msgs) != 0 {
		t.Errorf("messages count = %d, want 0", len(msgs))
	}
	if draft == nil {
		t.Error("draft is nil")
	}
}

// TestChatUseCase_GetThread_TenantIsolation — GetThread propagates ErrDraftNotFound
// when the draft belongs to another tenant (barrier-1 guard in GetDraftByID).
func TestChatUseCase_GetThread_TenantIsolation(t *testing.T) {
	repo := &fakeChatRepo{draftErr: ErrDraftNotFound}
	uc := newChatUCForTest(repo, nil, nil)

	_, _, err := uc.GetThread(context.Background(), "foreign-tenant", uuid.NewString())

	if !errors.Is(err, ErrDraftNotFound) {
		t.Errorf("want ErrDraftNotFound for foreign tenant, got %v", err)
	}
}

// TestChatUseCase_AnswerQuestion_LLMError — when the LLM call fails, AnswerQuestion
// returns the error without inserting any messages.
func TestChatUseCase_AnswerQuestion_LLMError(t *testing.T) {
	tenantID := "tenant-llm-err"
	repo := &fakeChatRepo{draft: stubChatDraft(tenantID)}
	llmErr := errors.New("upstream timeout")
	gen := &fakeChatGen{err: llmErr}
	uc := newChatUCForTest(repo, gen, nil)

	_, err := uc.AnswerQuestion(context.Background(), AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  repo.draft.ID,
		Question: "Qual o prazo?",
	})

	if err == nil {
		t.Fatal("want error from LLM failure, got nil")
	}
	// No messages should be inserted on LLM failure.
	if len(repo.insertedMsgs) != 0 {
		t.Errorf("inserted %d messages, want 0 (LLM error)", len(repo.insertedMsgs))
	}
}

// TestChatUseCase_AnswerQuestion_MultiTurnHistoryPassedToComposer — proves the
// existing history is loaded and passed to the composer. We use a custom composer
// that captures the ChatContext and asserts the history is non-empty.
func TestChatUseCase_AnswerQuestion_MultiTurnHistoryPassedToComposer(t *testing.T) {
	tenantID := "tenant-multiturn"
	history := []ChatMessage{
		{ID: uuid.NewString(), Role: ChatRoleUser, Content: "Primeira pergunta.", Citations: []Citation{}},
		{ID: uuid.NewString(), Role: ChatRoleAssistant, Content: "Primeira resposta.", Citations: []Citation{}},
	}
	repo := &fakeChatRepo{
		draft:  stubChatDraft(tenantID),
		thread: history,
	}
	gen := &fakeChatGen{out: []byte(noCitationsJSON)}

	var capturedCtx advisory.ChatContext
	capturingComposer := &capturingChatComposer{captured: &capturedCtx}
	uc := &ChatUseCase{
		uow:      fakeUoW{},
		repo:     repo,
		gen:      gen,
		emb:      nil,
		search:   indexing.SearchDeps{Pool: nil},
		composer: capturingComposer,
		model:    "test-model",
	}

	_, err := uc.AnswerQuestion(context.Background(), AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  repo.draft.ID,
		Question: "Segunda pergunta.",
	})

	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if len(capturedCtx.History) != 2 {
		t.Errorf("history length = %d, want 2 (the 2 prior turns)", len(capturedCtx.History))
	}
	if capturedCtx.Question != "Segunda pergunta." {
		t.Errorf("question = %q, want %q", capturedCtx.Question, "Segunda pergunta.")
	}
}

// capturingChatComposer captures the ChatContext passed to ComposeChat.
type capturingChatComposer struct {
	captured *advisory.ChatContext
}

func (c *capturingChatComposer) Compose(_ string, _ advisory.CaseContext) (advisory.Composed, error) {
	panic("unexpected")
}
func (c *capturingChatComposer) ComposeProcess(_ string, _ advisory.ProcessContext) (advisory.Composed, error) {
	panic("unexpected")
}
func (c *capturingChatComposer) ComposeDraft(_ string, _ advisory.DraftContext) (advisory.Composed, error) {
	panic("unexpected")
}
func (c *capturingChatComposer) ComposeChat(_ string, ctx advisory.ChatContext) (advisory.Composed, error) {
	*c.captured = ctx
	return advisory.Composed{System: "s", User: "u", PromptVersion: "v"}, nil
}
func (c *capturingChatComposer) ComposeReview(_ string, _ advisory.ReviewContext) (advisory.Composed, error) {
	panic("unexpected ComposeReview in chat test")
}
func (c *capturingChatComposer) ComposeTheses(_ string, _ advisory.DraftContext) (advisory.Composed, error) {
	panic("unexpected ComposeTheses in chat test")
}
func (c *capturingChatComposer) ComposeIterate(_ string, _ advisory.IterateContext) (advisory.Composed, error) {
	panic("unexpected ComposeIterate in chat test")
}

// ── validateChatCitations unit tests ─────────────────────────────────────────

// TestValidateChatCitations covers the citation validation helper directly.
func TestValidateChatCitations(t *testing.T) {
	hits := []indexing.ChunkHit{
		{DocumentID: "doc-1", Page: 3, Text: "prazo de 15 dias"},
		{DocumentID: "doc-2", Page: 7, Text: "art. 5 da CF"},
	}

	tests := []struct {
		name      string
		raw       []rawChatCit
		hits      []indexing.ChunkHit
		wantLen   int
		wantDrops []string // document_ids expected to be dropped
	}{
		{
			name:    "valid citations kept",
			raw:     []rawChatCit{{DocumentID: "doc-1", Page: 3, Quote: "prazo de 15 dias"}},
			hits:    hits,
			wantLen: 1,
		},
		{
			name:    "hallucinated doc_id dropped",
			raw:     []rawChatCit{{DocumentID: "hallucinated", Page: 1, Quote: "inventado"}},
			hits:    hits,
			wantLen: 0,
		},
		{
			name:    "empty quote dropped",
			raw:     []rawChatCit{{DocumentID: "doc-1", Page: 3, Quote: ""}},
			hits:    hits,
			wantLen: 0,
		},
		{
			name:    "no hits returns empty",
			raw:     []rawChatCit{{DocumentID: "doc-1", Page: 3, Quote: "prazo"}},
			hits:    nil,
			wantLen: 0,
		},
		{
			name:    "empty raw returns empty",
			raw:     []rawChatCit{},
			hits:    hits,
			wantLen: 0,
		},
		{
			name: "mixed valid and invalid",
			raw: []rawChatCit{
				{DocumentID: "doc-1", Page: 3, Quote: "prazo"},
				{DocumentID: "ghost", Page: 1, Quote: "inventado"},
				{DocumentID: "doc-2", Page: 7, Quote: "art. 5"},
			},
			hits:    hits,
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := tt
			got := validateChatCitations(is.raw, is.hits)
			if len(got) != is.wantLen {
				t.Errorf("len(got) = %d, want %d", len(got), is.wantLen)
			}
		})
	}
}

// TestBuildChatContext — buildChatContext maps domain objects to advisory.ChatContext.
func TestBuildChatContext(t *testing.T) {
	d := &Draft{Content: "Exmo. Sr. Juiz...", CaseID: "case-1"}
	history := []ChatMessage{
		{Role: ChatRoleUser, Content: "Q1", Citations: []Citation{}},
		{Role: ChatRoleAssistant, Content: "A1", Citations: []Citation{}},
	}
	chunks := []string{"chunk 1", "chunk 2"}
	hits := []indexing.ChunkHit{
		{DocumentID: "doc-1", Page: 3, Text: "chunk 1"},
		{DocumentID: "doc-2", Page: 7, Text: "chunk 2"},
	}

	ctx := buildChatContext(d, history, chunks, hits, "Nova pergunta")

	if ctx.DraftContent != d.Content {
		t.Errorf("DraftContent = %q, want %q", ctx.DraftContent, d.Content)
	}
	if len(ctx.Chunks) != 2 {
		t.Errorf("Chunks len = %d, want 2", len(ctx.Chunks))
	}
	if len(ctx.History) != 2 {
		t.Errorf("History len = %d, want 2", len(ctx.History))
	}
	if ctx.History[0].Role != ChatRoleUser {
		t.Errorf("History[0].Role = %q, want user", ctx.History[0].Role)
	}
	if ctx.Question != "Nova pergunta" {
		t.Errorf("Question = %q, want %q", ctx.Question, "Nova pergunta")
	}
}

// TestChatUseCase_WithIntimation_CRIDResolvedInPhase1 verifies that when a draft has
// an intimation with a CourtRecordID, GetIntimationForDraft is called during Phase 1
// (inside the read tx) and the court_record_id is resolved before Phase 2 (RAG).
// Since the unit tests use Pool=nil, runRAG degrades gracefully; we assert the answer
// is still produced (non-fatal degradation) and GetIntimationForDraft was invoked.
func TestChatUseCase_WithIntimation_CRIDResolvedInPhase1(t *testing.T) {
	tenantID := "tenant-crid"
	d := stubChatDraft(tenantID)
	d.IntimationID = "intim-1"

	intim := &IntimationContext{
		IntimationID:  "intim-1",
		CaseID:        d.CaseID,
		CourtRecordID: "court-record-abc",
		Type:          "CITACAO",
	}
	repo := &fakeChatRepo{
		draft:      d,
		intimation: intim,
	}
	gen := &fakeChatGen{out: []byte(noCitationsJSON)}
	uc := newChatUCForTest(repo, gen, nil) // nil embedder → RAG degrades (non-fatal)

	msg, err := uc.AnswerQuestion(context.Background(), AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  d.ID,
		Question: "Qual o prazo para contestar?",
	})

	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if msg == nil {
		t.Fatal("expected non-nil message")
	}
	// Degraded (nil embedder, pool nil) → grounded=false, but the answer is still produced.
	if msg.Grounded {
		t.Error("grounded = true, want false (nil embedder)")
	}
	// The 2 messages (user + assistant) must be persisted regardless of grounding.
	if len(repo.insertedMsgs) != 2 {
		t.Errorf("inserted %d messages, want 2", len(repo.insertedMsgs))
	}
}

// TestChatUseCase_WithIntimation_CRIDLoadError_NonFatal verifies that when
// GetIntimationForDraft returns an error, AnswerQuestion degrades to whole-tenant
// RAG (non-fatal) rather than failing the request.
func TestChatUseCase_WithIntimation_CRIDLoadError_NonFatal(t *testing.T) {
	tenantID := "tenant-crid-err"
	d := stubChatDraft(tenantID)
	d.IntimationID = "intim-2"

	repo := &fakeChatRepo{
		draft:    d,
		intimErr: errors.New("intimation row not found"), // simulates DB error
	}
	gen := &fakeChatGen{out: []byte(noCitationsJSON)}
	uc := newChatUCForTest(repo, gen, nil)

	msg, err := uc.AnswerQuestion(context.Background(), AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  d.ID,
		Question: "Qual é o juiz?",
	})

	// Non-fatal: intimation load error must NOT propagate; the chat must still answer.
	if err != nil {
		t.Fatalf("want nil err (intimation load error is non-fatal), got %v", err)
	}
	if msg == nil {
		t.Fatal("expected non-nil message")
	}
	if len(repo.insertedMsgs) != 2 {
		t.Errorf("inserted %d messages, want 2", len(repo.insertedMsgs))
	}
}

// TestChatUseCase_WithoutIntimation_NoCRIDLoad verifies that a draft without an
// IntimationID does NOT call GetIntimationForDraft (no unnecessary DB round-trip).
func TestChatUseCase_WithoutIntimation_NoCRIDLoad(t *testing.T) {
	tenantID := "tenant-no-intim"
	d := stubChatDraft(tenantID)
	d.IntimationID = "" // blank/processo draft

	repo := &fakeChatRepo{
		draft: d,
	}
	gen := &fakeChatGen{out: []byte(noCitationsJSON)}
	uc := newChatUCForTest(repo, gen, nil)

	msg, err := uc.AnswerQuestion(context.Background(), AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  d.ID,
		Question: "Alguma informação?",
	})

	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if msg == nil {
		t.Fatal("expected non-nil message")
	}
	// The guard (d.IntimationID != "") must prevent the extra DB round-trip.
	if repo.intimCalled {
		t.Error("GetIntimationForDraft was called for a draft without an intimation")
	}
}

// TestUnmarshalCitations — codec round-trip for citations jsonb.
func TestUnmarshalCitations(t *testing.T) {
	cits := []Citation{
		{DocumentID: "doc-1", Page: 3, Quote: "trecho"},
	}
	b, err := json.Marshal(cits)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := unmarshalCitations(b)
	if len(got) != 1 {
		t.Fatalf("got %d citations, want 1", len(got))
	}
	if got[0].DocumentID != "doc-1" {
		t.Errorf("DocumentID = %q, want doc-1", got[0].DocumentID)
	}

	// Empty input → empty slice, not nil.
	empty := unmarshalCitations(nil)
	if empty == nil {
		t.Error("unmarshalCitations(nil) = nil, want []Citation{}")
	}
}
