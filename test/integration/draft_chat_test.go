//go:build integration

// draft_chat integration tests — prove the Fatia 3b grounded chat round-trip
// (AnswerQuestion → persists 2 rows → GetThread → reloads → tenant isolation)
// against a REAL Postgres with migrations applied through 0045_chat_message.
//
// What unit tests with mocked repos CANNOT prove:
//   - AnswerQuestion writes BOTH the user turn AND the assistant turn in the SAME
//     committed transaction (UnitOfWork atomicity).
//   - GetThread orders rows chronologically (oldest first) and returns at most 50.
//   - Tenant isolation: a foreign tenant cannot read another tenant's thread via
//     GetThread (barrier 1: GetDraftByID tenant guard before GetChatThread).
//   - The chat_message table rows survive across independent tx boundaries (persist
//     through the Postgres ACID flush, not just an in-memory fake).
//
// The real LLM API is NEVER called. A fake llm.Generator returns canned JSON.
// Embedder is nil (degraded path — no grounding) to keep tests infrastructure-free.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/draft"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/llm"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

// newChatUC wires the chat use case against the real pool with the given fake
// generator. Embedder and search pool are nil → degraded (no RAG).
func newChatUC(pool *pgxpool.Pool, gen llm.Generator) *draft.ChatUseCase {
	repo := draft.NewRepository()
	return draft.NewChatUseCase(draft.ChatUseCaseParams{
		UoW:      database.NewUnitOfWork(pool),
		Repo:     repo,
		Gen:      gen,
		Emb:      nil, // nil → degraded (no grounding in integration tests)
		Search:   indexing.SearchDeps{Pool: nil},
		Composer: advisory.NewTemplateComposer(),
		Model:    "claude-opus-4-8",
	})
}

// newDraftUC is a convenience alias for integration tests (defined in
// draft_generate_test.go with the same pool).
// Redefining here would clash; use the one from draft_generate_test.go.

// canned chat JSON returns a valid chat answer with no citations (ungrounded).
const cannedChatAnswerJSON = `{
  "answer": "Com base nos autos disponíveis, o prazo é de 15 dias úteis.",
  "citations": []
}`

// countChatMessages counts chat_message rows for a given draft_id.
func countChatMessages(t *testing.T, pool *pgxpool.Pool, draftID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM chat_message WHERE draft_id = $1`, draftID).Scan(&n); err != nil {
		t.Fatalf("countChatMessages(%s): %v", draftID, err)
	}
	return n
}

// readChatThreadRaw reads the chat_message rows for a draft_id ordered by
// created_at ASC (bypasses the use case — used for direct DB assertions).
func readChatThreadRaw(t *testing.T, pool *pgxpool.Pool, draftID string) []struct {
	Role     string
	Content  string
	Grounded bool
} {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT role, content, grounded FROM chat_message
		 WHERE draft_id = $1 ORDER BY created_at ASC`, draftID)
	if err != nil {
		t.Fatalf("readChatThreadRaw(%s): %v", draftID, err)
	}
	defer rows.Close()

	var out []struct {
		Role     string
		Content  string
		Grounded bool
	}
	for rows.Next() {
		var r struct {
			Role     string
			Content  string
			Grounded bool
		}
		if err := rows.Scan(&r.Role, &r.Content, &r.Grounded); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// ── AC1: AnswerQuestion persists 2 rows in one tx ─────────────────────────────

// TestDraftChat_AnswerQuestion_PersistedTwoRows proves that AnswerQuestion inserts
// exactly 2 rows: role='user' (the question) and role='assistant' (the answer).
// Both rows must be committed in the same transaction (visible after the call).
func TestDraftChat_AnswerQuestion_PersistedTwoRows(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-chat-persist", 0)
	draftID := seedDraft(t, pool, tenantID)

	gen := &integrationFakeGen{out: []byte(cannedChatAnswerJSON)}
	chatUC := newChatUC(pool, gen)

	msg, err := chatUC.AnswerQuestion(ctx, draft.AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  draftID,
		Question: "Qual o prazo para contestar?",
	})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}

	// Returned message must be the assistant turn.
	if msg.Role != draft.ChatRoleAssistant {
		t.Errorf("returned role = %q, want assistant", msg.Role)
	}
	if msg.ID == "" {
		t.Error("returned message ID is empty")
	}

	// DB must have exactly 2 rows (user + assistant).
	if n := countChatMessages(t, pool, draftID); n != 2 {
		t.Fatalf("chat_message count = %d, want 2 (user + assistant)", n)
	}

	rows := readChatThreadRaw(t, pool, draftID)
	if len(rows) != 2 {
		t.Fatalf("raw rows = %d, want 2", len(rows))
	}
	if rows[0].Role != draft.ChatRoleUser {
		t.Errorf("rows[0].Role = %q, want user", rows[0].Role)
	}
	if rows[0].Content != "Qual o prazo para contestar?" {
		t.Errorf("rows[0].Content = %q, want the question", rows[0].Content)
	}
	if rows[1].Role != draft.ChatRoleAssistant {
		t.Errorf("rows[1].Role = %q, want assistant", rows[1].Role)
	}
}

// ── AC2: GetThread reloads thread chronologically ─────────────────────────────

// TestDraftChat_GetThread_OrderedChronologically proves that GetThread returns the
// messages for a draft in ascending created_at order (oldest first) and that
// subsequent calls to AnswerQuestion correctly accumulate the thread.
func TestDraftChat_GetThread_OrderedChronologically(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-chat-order", 0)
	draftID := seedDraft(t, pool, tenantID)

	gen := &integrationFakeGen{out: []byte(cannedChatAnswerJSON)}
	chatUC := newChatUC(pool, gen)

	// First turn.
	if _, err := chatUC.AnswerQuestion(ctx, draft.AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  draftID,
		Question: "Primeira pergunta.",
	}); err != nil {
		t.Fatalf("first AnswerQuestion: %v", err)
	}

	// Second turn.
	if _, err := chatUC.AnswerQuestion(ctx, draft.AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  draftID,
		Question: "Segunda pergunta.",
	}); err != nil {
		t.Fatalf("second AnswerQuestion: %v", err)
	}

	// 4 rows total (2 questions + 2 answers).
	if n := countChatMessages(t, pool, draftID); n != 4 {
		t.Fatalf("chat_message count = %d, want 4 (2 turns × 2 rows)", n)
	}

	msgs, d, err := chatUC.GetThread(ctx, tenantID, draftID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if d == nil {
		t.Error("draft returned by GetThread is nil")
	}
	if len(msgs) != 4 {
		t.Fatalf("GetThread returned %d messages, want 4", len(msgs))
	}

	// Chronological order: user, assistant, user, assistant.
	wantRoles := []string{"user", "assistant", "user", "assistant"}
	for i, wantRole := range wantRoles {
		if msgs[i].Role != wantRole {
			t.Errorf("msgs[%d].Role = %q, want %q", i, msgs[i].Role, wantRole)
		}
	}

	// First user message should contain the first question's content.
	if msgs[0].Content != "Primeira pergunta." {
		t.Errorf("msgs[0].Content = %q, want first question", msgs[0].Content)
	}
}

// ── AC3: Thread limit — at most 50 messages returned ─────────────────────────

// TestDraftChat_GetThread_Limit50 proves that GetThread returns at most 50 messages
// even when more exist in the DB. The limit is applied as a subquery so the returned
// rows are always the most-recent 50, ordered chronologically.
func TestDraftChat_GetThread_Limit50(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-chat-limit", 0)
	draftID := seedDraft(t, pool, tenantID)

	// Seed 30 turns (60 rows = 30 user + 30 assistant) to exceed the limit.
	gen := &integrationFakeGen{out: []byte(cannedChatAnswerJSON)}
	chatUC := newChatUC(pool, gen)

	for i := range 30 {
		if _, err := chatUC.AnswerQuestion(ctx, draft.AnswerQuestionCommand{
			TenantID: tenantID,
			DraftID:  draftID,
			Question: "Pergunta número " + string(rune('A'+i)) + "?",
		}); err != nil {
			t.Fatalf("AnswerQuestion turn %d: %v", i, err)
		}
	}

	// 60 rows exist in DB.
	if n := countChatMessages(t, pool, draftID); n != 60 {
		t.Fatalf("chat_message count = %d, want 60 (30 turns)", n)
	}

	msgs, _, err := chatUC.GetThread(ctx, tenantID, draftID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}

	// GetThread returns at most 50.
	if len(msgs) > 50 {
		t.Errorf("GetThread returned %d messages, want at most 50", len(msgs))
	}
	// Must be ordered chronologically.
	for i := 1; i < len(msgs); i++ {
		if msgs[i].CreatedAt.Before(msgs[i-1].CreatedAt) {
			t.Errorf("msgs[%d].CreatedAt < msgs[%d].CreatedAt — not chronological", i, i-1)
		}
	}
}

// ── AC4: Tenant isolation ─────────────────────────────────────────────────────

// TestDraftChat_TenantIsolation proves that GetThread returns an error (not data)
// when a foreign tenant tries to access another tenant's chat thread.
// The chat_message table has no RLS; isolation is enforced at barrier-1 via
// GetDraftByID's tenant scope.
func TestDraftChat_TenantIsolation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	// Owner tenant: create a draft and post a message.
	ownerTenantID := uuid.NewString()
	seedTenant(t, pool, ownerTenantID, "org-chat-owner-iso", 0)
	ownerDraftID := seedDraft(t, pool, ownerTenantID)

	gen := &integrationFakeGen{out: []byte(cannedChatAnswerJSON)}
	chatUC := newChatUC(pool, gen)

	if _, err := chatUC.AnswerQuestion(ctx, draft.AnswerQuestionCommand{
		TenantID: ownerTenantID,
		DraftID:  ownerDraftID,
		Question: "Mensagem secreta do dono.",
	}); err != nil {
		t.Fatalf("owner AnswerQuestion: %v", err)
	}

	// Confirm the owner's rows exist.
	if n := countChatMessages(t, pool, ownerDraftID); n != 2 {
		t.Fatalf("owner chat_message count = %d, want 2", n)
	}

	// Foreign tenant tries to read the owner's thread.
	foreignTenantID := uuid.NewString()
	seedTenant(t, pool, foreignTenantID, "org-chat-foreign-iso", 0)

	_, _, err := chatUC.GetThread(ctx, foreignTenantID, ownerDraftID)
	if err == nil {
		t.Fatal("foreign GetThread returned nil error — chat data may have leaked")
	}

	// Foreign tenant also cannot POST a question to the owner's draft.
	_, err = chatUC.AnswerQuestion(ctx, draft.AnswerQuestionCommand{
		TenantID: foreignTenantID,
		DraftID:  ownerDraftID,
		Question: "Tentativa de acesso indevido.",
	})
	if err == nil {
		t.Fatal("foreign AnswerQuestion returned nil error — chat data may have been written to wrong tenant")
	}

	// Owner's row count must be unchanged (no extra rows from the foreign attempt).
	if n := countChatMessages(t, pool, ownerDraftID); n != 2 {
		t.Fatalf("owner chat_message count = %d, want 2 (no rows from foreign attempt)", n)
	}
}

// ── AC5b: RAG scoping — SearchChunks with court_record_id isolation ──────────

// seedChunkForDocument inserts a chunk row with a fixed embedding vector (all-zeros
// 1024-dim) linked to the given documentID. Schema: 0001_init (id, document_id, page,
// text, embedding) + 0034_chunk_embedding_deltas (embedding_model, dim, chunk_hash).
// The exact vector value is irrelevant for scoping tests; cosine distance still orders
// by similarity and any non-null vector produces hits.
func seedChunkForDocument(t *testing.T, pool *pgxpool.Pool, documentID string, page int, text string) {
	t.Helper()
	// Use a CTE to capture $3 once so sha256 sees bytea while the INSERT sees text.
	if _, err := pool.Exec(context.Background(),
		`WITH v AS (SELECT $3::text AS t)
		 INSERT INTO chunk (document_id, page, text, chunk_hash, embedding, embedding_model, dim)
		 SELECT $1, $2, v.t,
		        encode(sha256(v.t::bytea), 'hex'),
		        (array_fill(0, ARRAY[1024])::vector(1024)),
		        'voyage-4-lite', 1024
		 FROM v`,
		documentID, page, text); err != nil {
		t.Fatalf("seedChunkForDocument(%s, %d): %v", documentID, page, err)
	}
}

// TestSearchChunks_ScopedByCourtRecordID proves that SearchChunks with a non-nil
// courtRecordID returns ONLY chunks whose document has that court_record_id.
// Two documents are seeded: one with court_record_id=A and one with court_record_id=B.
// A query scoped to A must return only the chunk from document A.
//
// This is the load-bearing integration assertion for the intimation-scoped grounding
// feature: we prove that the SQL isolation (document.court_record_id = $3) actually
// works against a real Postgres, not just in the pgxmock unit tests.
func TestSearchChunks_ScopedByCourtRecordID(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-rag-scope", 0)

	// Seed two court_record rows (auto-generated IDs) — represent two different processos.
	cridA, _ := seedCourtRecordCNJ(t, pool, tenantID, "0000001-01.2024.8.26.0001")
	cridB, _ := seedCourtRecordCNJ(t, pool, tenantID, "0000002-02.2024.8.26.0001")

	// One document per court_record.
	docA := seedUploadedDocument(t, pool, tenantID, cridA)
	docB := seedUploadedDocument(t, pool, tenantID, cridB)

	// One chunk per document.
	seedChunkForDocument(t, pool, docA, 1, "texto do processo A — prazo de 15 dias")
	seedChunkForDocument(t, pool, docB, 1, "texto do processo B — recurso cabível")

	// All-zeros query vector (1024-dim) — used for the SearchChunks call. The actual
	// score is irrelevant; we just need hits to be returned (any similarity > -1).
	queryVec := make([]float32, 1024)

	// Scope to court_record A → must return ONLY the chunk from docA.
	hitsA, err := indexing.SearchChunks(ctx,
		indexing.SearchDeps{Pool: pool},
		tenantID,
		&cridA,
		queryVec,
		10,
	)
	if err != nil {
		t.Fatalf("SearchChunks(cridA): %v", err)
	}
	if len(hitsA) != 1 {
		t.Fatalf("hits with cridA = %d, want 1 (only docA's chunk)", len(hitsA))
	}
	if hitsA[0].DocumentID != docA {
		t.Errorf("hit document_id = %q, want docA %q", hitsA[0].DocumentID, docA)
	}
	if hitsA[0].Text != "texto do processo A — prazo de 15 dias" {
		t.Errorf("hit text = %q, want docA text", hitsA[0].Text)
	}

	// Scope to court_record B → must return ONLY the chunk from docB.
	hitsB, err := indexing.SearchChunks(ctx,
		indexing.SearchDeps{Pool: pool},
		tenantID,
		&cridB,
		queryVec,
		10,
	)
	if err != nil {
		t.Fatalf("SearchChunks(cridB): %v", err)
	}
	if len(hitsB) != 1 {
		t.Fatalf("hits with cridB = %d, want 1 (only docB's chunk)", len(hitsB))
	}
	if hitsB[0].DocumentID != docB {
		t.Errorf("hit document_id = %q, want docB %q", hitsB[0].DocumentID, docB)
	}

	// Whole-tenant (nil crid) → both chunks returned.
	hitsAll, err := indexing.SearchChunks(ctx,
		indexing.SearchDeps{Pool: pool},
		tenantID,
		nil,
		queryVec,
		10,
	)
	if err != nil {
		t.Fatalf("SearchChunks(nil): %v", err)
	}
	if len(hitsAll) != 2 {
		t.Errorf("hits whole-tenant = %d, want 2 (docA + docB)", len(hitsAll))
	}
}

// ── AC5: Ungrounded — grounded=false when no embedder ─────────────────────────

// TestDraftChat_Ungrounded_GroundedFalse proves that when the embedder is nil,
// the assistant message is persisted with grounded=false and citations=[].
func TestDraftChat_Ungrounded_GroundedFalse(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-chat-ungrounded", 0)
	draftID := seedDraft(t, pool, tenantID)

	gen := &integrationFakeGen{out: []byte(cannedChatAnswerJSON)}
	chatUC := newChatUC(pool, gen) // nil embedder → degraded

	msg, err := chatUC.AnswerQuestion(ctx, draft.AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  draftID,
		Question: "Existe contestação?",
	})
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}

	if msg.Grounded {
		t.Error("grounded = true, want false (no embedder)")
	}
	if len(msg.Citations) != 0 {
		t.Errorf("citations = %d, want 0 (ungrounded)", len(msg.Citations))
	}

	// Verify persisted in DB.
	rows := readChatThreadRaw(t, pool, draftID)
	if len(rows) != 2 {
		t.Fatalf("DB rows = %d, want 2", len(rows))
	}
	// Assistant row must have grounded=false.
	if rows[1].Grounded {
		t.Error("DB assistant row grounded = true, want false")
	}
}
