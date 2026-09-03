package draft

// chat.go implements the synchronous grounded-chat use case (Peticionamento Fatia 3b).
// The advogado asks a question about a draft/peça; the assistant answers in-process
// (synchronous), anchored to citations from the case corpus (RAG).
// Thread is persisted multi-turn: each AnswerQuestion call appends 2 rows to
// chat_message (role='user' + role='assistant') in ONE tx (UnitOfWork).
// GetThread returns the last 50 messages ordered chronologically.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/llm"
)

// chatHistoryLimit is the maximum number of history turns passed to the prompt composer.
// We pass all 50 loaded messages — the limit on retrieval is 50 (GetChatThread).
const chatHistoryLimit = 50

// chatModel is the single source of truth for the model used in the chat pipeline.
// Callers may override via ChatUseCaseParams.Model.
const chatModel = "claude-opus-4-8"

// AnswerQuestionCommand is the input for POST /v1/pecas/:id/chat.
type AnswerQuestionCommand struct {
	TenantID string
	DraftID  string
	Question string
}

// chatRepo is the narrow read/write port the ChatUseCase needs. Satisfied by the
// full Repository; kept as a separate interface so tests can inject minimal fakes.
type chatRepo interface {
	GetDraftByID(ctx context.Context, tx database.Tx, tenantID, draftID string) (*Draft, error)
	GetIntimationForDraft(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*IntimationContext, error)
	InsertChatMessage(ctx context.Context, tx database.Tx, m *ChatMessage) (*ChatMessage, error)
	GetChatThread(ctx context.Context, tx database.Tx, draftID string) ([]ChatMessage, error)
}

// chatOutput is the LLM's structured response for one chat turn.
type chatOutput struct {
	Answer    string       `json:"answer"`
	Citations []rawChatCit `json:"citations"`
}

// rawChatCit is the LLM's citation before validation.
type rawChatCit struct {
	DocumentID string `json:"document_id"`
	Page       int    `json:"page"`
	Quote      string `json:"quote"`
}

// chatSchema is the JSON schema constraining the LLM's chat output (strict mode).
// All fields are in required; citations may be an empty array.
var chatSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "answer": { "type": "string" },
    "citations": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "document_id": { "type": "string" },
          "page":        { "type": ["integer", "null"] },
          "quote":       { "type": "string" }
        },
        "required": ["document_id", "page", "quote"],
        "additionalProperties": false
      }
    }
  },
  "required": ["answer", "citations"],
  "additionalProperties": false
}`)

// ChatUseCase is the synchronous chat use case. It is composed independently
// from the async GenerateUseCase (different dep set; no outbox needed).
type ChatUseCase struct {
	uow      database.UnitOfWork
	repo     chatRepo
	gen      llm.Generator // nil → typed error "IA não configurada"
	emb      embedder      // nil → degraded (no RAG grounding)
	search   indexing.SearchDeps
	ragCache *RAGCache // nil → sem cache
	composer advisory.PromptComposer
	model    string
}

// ChatUseCaseParams groups the construction parameters. All pointer fields may be nil.
type ChatUseCaseParams struct {
	UoW      database.UnitOfWork
	Repo     chatRepo
	Gen      llm.Generator
	Emb      embedder
	Search   indexing.SearchDeps
	RAGCache *RAGCache
	Composer advisory.PromptComposer
	Model    string // OpenRouter model slug; empty → chatModel fallback
}

// NewChatUseCase wires the chat use case.
func NewChatUseCase(p ChatUseCaseParams) *ChatUseCase {
	model := p.Model
	if model == "" {
		model = chatModel
	}
	return &ChatUseCase{
		uow:      p.UoW,
		repo:     p.Repo,
		gen:      p.Gen,
		emb:      p.Emb,
		search:   p.Search,
		ragCache: p.RAGCache,
		composer: p.Composer,
		model:    model,
	}
}

// AnswerQuestion implements POST /v1/pecas/:id/chat.
// Structured in 3 phases to avoid holding a pgx connection open during the LLM call
// (which can take 2–30 s and would exhaust the pool under concurrency):
//
//	Phase 1 (READ, short tx): load draft (tenant guard) + load history + resolve courtRecordID.
//	Phase 2 (LLM, NO tx):    embed → SearchChunks (scoped to intimation) → ComposeChat → GenerateJSON → validate citations.
//	Phase 3 (WRITE, short tx): insert role='user' + role='assistant' atomically.
//
// The FK chat_message.draft_id ON DELETE CASCADE ensures a clean insert failure
// if the draft disappears between Phase 1 and Phase 3 — no dangling rows.
//
// Degradation:
//   - gen nil → returns ErrIANotConfigured (typed invalid, → 422 at edge).
//   - emb nil OR no chunks → answers grounded=false, citations=[].
func (uc *ChatUseCase) AnswerQuestion(ctx context.Context, cmd AnswerQuestionCommand) (*ChatMessage, error) {
	if uc.gen == nil {
		return nil, ErrIANotConfigured
	}

	// ── Phase 1: READ (short tx) ──────────────────────────────────────────────
	var d *Draft
	var history []ChatMessage
	var crid *string // court_record_id resolved from the intimation (nil → whole-tenant)
	if err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		loaded, err := uc.repo.GetDraftByID(ctx, tx, cmd.TenantID, cmd.DraftID)
		if err != nil {
			return err
		}
		d = loaded

		// Resolve the court_record_id for RAG scoping. Non-fatal: if the intimation
		// load fails, grounding degrades to tenant-wide (same policy as generate.go).
		if d.IntimationID != "" {
			intimation, e := uc.repo.GetIntimationForDraft(ctx, tx, cmd.TenantID, d.IntimationID)
			if e != nil {
				slog.WarnContext(ctx, "chat: intimation load failed — falling back to tenant-wide RAG",
					slog.String("draft_id", cmd.DraftID),
					slog.Any("error", e),
				)
			} else if intimation.CourtRecordID != "" {
				crid = &intimation.CourtRecordID
			}
		}

		msgs, err := uc.repo.GetChatThread(ctx, tx, cmd.DraftID)
		if err != nil {
			return fmt.Errorf("chat: load thread: %w", err)
		}
		history = msgs
		return nil
	}); err != nil {
		return nil, err
	}

	// ── Phase 2: LLM (NO tx — connection released) ───────────────────────────

	// 2a. RAG: embed question → search chunks (scoped to intimation's court_record when available).
	chunks, chunkHits, _ := runRAG(ctx, uc.emb, uc.search, uc.ragCache, cmd.TenantID, crid, cmd.Question, 8)

	// 2b. Compose prompt.
	chatCtx := buildChatContext(d, history, chunks, chunkHits, cmd.Question)
	composed, err := uc.composer.ComposeChat(advisory.AgentChatGrounding, chatCtx)
	if err != nil {
		return nil, fmt.Errorf("chat: compose prompt: %w", err)
	}

	// 2c. Call LLM.
	rawBytes, err := uc.gen.GenerateJSON(ctx, llm.Request{
		System:     composed.System,
		User:       composed.User,
		Schema:     chatSchema,
		SchemaName: "chat_answer",
		Model:      uc.model,
		MaxTokens:  2048,
		UseCase:    "draft.chat",
		TenantID:   cmd.TenantID,
	})
	if err != nil {
		return nil, fmt.Errorf("chat: llm call: %w", err)
	}

	var out chatOutput
	if err := json.Unmarshal(rawBytes, &out); err != nil {
		return nil, fmt.Errorf("chat: parse llm output: %w", err)
	}

	// 2d. Validate citations — keep only those whose document_id appears in the
	// retrieved chunks (same principle as generate.go's substring validation).
	validatedCitations := validateChatCitations(out.Citations, chunkHits)
	grounded := len(chunkHits) > 0 && len(validatedCitations) > 0

	assistantCitations := make([]Citation, 0, len(validatedCitations))
	for _, c := range validatedCitations {
		assistantCitations = append(assistantCitations, Citation{
			DocumentID: c.DocumentID,
			Page:       c.Page,
			Quote:      c.Quote,
		})
	}

	// ── Phase 3: WRITE (short tx — only the 2 inserts) ───────────────────────
	var assistantMsg *ChatMessage
	if err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		userMsg := &ChatMessage{
			DraftID:   cmd.DraftID,
			Role:      ChatRoleUser,
			Content:   cmd.Question,
			Citations: []Citation{},
			Grounded:  false,
		}
		if _, err := uc.repo.InsertChatMessage(ctx, tx, userMsg); err != nil {
			return fmt.Errorf("chat: insert user message: %w", err)
		}

		aMsg := &ChatMessage{
			DraftID:      cmd.DraftID,
			Role:         ChatRoleAssistant,
			Content:      out.Answer,
			Citations:    assistantCitations,
			Grounded:     grounded,
			ModelVersion: uc.model,
		}
		persisted, err := uc.repo.InsertChatMessage(ctx, tx, aMsg)
		if err != nil {
			return fmt.Errorf("chat: insert assistant message: %w", err)
		}
		assistantMsg = persisted
		return nil
	}); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "chat answer persisted",
		slog.String("draft_id", cmd.DraftID),
		slog.Bool("grounded", assistantMsg.Grounded),
		slog.Int("citations", len(assistantMsg.Citations)),
	)
	return assistantMsg, nil
}

// GetThread implements GET /v1/pecas/:id/chat.
// Loads the draft (tenant guard) then returns the last 50 messages chronologically.
// An empty thread returns an empty slice, never nil.
func (uc *ChatUseCase) GetThread(ctx context.Context, tenantID, draftID string) ([]ChatMessage, *Draft, error) {
	var messages []ChatMessage
	var draft *Draft

	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		d, err := uc.repo.GetDraftByID(ctx, tx, tenantID, draftID)
		if err != nil {
			return err
		}
		draft = d

		msgs, err := uc.repo.GetChatThread(ctx, tx, draftID)
		if err != nil {
			return fmt.Errorf("chat: get thread: %w", err)
		}
		messages = msgs
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if messages == nil {
		messages = []ChatMessage{}
	}
	return messages, draft, nil
}

// buildChatContext converts the domain objects to an advisory.ChatContext for prompt composition.
// hits carries the same RAG results as chunks but with the document_id/page the prompt must
// expose so the model can cite by document_id (chunks alone is bare text — see chat_grounding/v2).
func buildChatContext(d *Draft, history []ChatMessage, chunks []string, hits []indexing.ChunkHit, question string) advisory.ChatContext {
	turns := make([]advisory.ChatTurn, 0, len(history))
	for _, m := range history {
		turns = append(turns, advisory.ChatTurn{Role: m.Role, Content: m.Content})
	}
	refs := make([]advisory.ChatChunkRef, 0, len(hits))
	for _, h := range hits {
		if h.DocumentID == "" {
			continue
		}
		refs = append(refs, advisory.ChatChunkRef{
			DocumentID: h.DocumentID,
			Page:       h.Page,
			Text:       h.Text,
		})
	}
	return advisory.ChatContext{
		DraftContent: d.Content,
		Chunks:       chunks,
		ChunkRefs:    refs,
		History:      turns,
		Question:     question,
	}
}

// validateChatCitations filters LLM citations to those whose document_id appears
// in the actual retrieved chunk hits. A citation claiming a document_id that was
// not in the top-8 results is discarded (same validation philosophy as generate.go's
// substring check — we only allow grounding in what we actually retrieved).
func validateChatCitations(raw []rawChatCit, hits []indexing.ChunkHit) []rawChatCit {
	if len(hits) == 0 {
		return []rawChatCit{}
	}

	// Build a set of document IDs from the actual hits.
	validDocs := make(map[string]bool, len(hits))
	for _, h := range hits {
		if h.DocumentID != "" {
			validDocs[h.DocumentID] = true
		}
	}

	out := make([]rawChatCit, 0, len(raw))
	for _, c := range raw {
		if c.DocumentID == "" {
			continue
		}
		if !validDocs[c.DocumentID] {
			// LLM hallucinated a document_id not in the retrieved set — discard.
			continue
		}
		// Basic sanity: quote must be non-empty.
		if strings.TrimSpace(c.Quote) == "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// ErrIANotConfigured is returned when AnswerQuestion is called without a configured
// LLM generator. Typed invalid (→ 422 at the edge).
var ErrIANotConfigured = apperr.NewInvalid("IA não configurada: OPENROUTER_API_KEY não definida")
