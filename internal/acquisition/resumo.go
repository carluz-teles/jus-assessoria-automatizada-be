package acquisition

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/llm"
)

// ProcessResumoView is the wire DTO returned by GET /v1/processos/:id/resume.
// Slices are initialized so empty arrays serialize as [], never null.
type ProcessResumoView struct {
	Summary              string              `json:"summary"`
	CurrentStatus        string              `json:"current_status"`
	KeyDatesAndDeadlines []KeyDate           `json:"key_dates_and_deadlines"`
	RecentMovements      []RecentMovement    `json:"recent_movements"`
	Risks                []Risk              `json:"risks"`
	RecommendedActions   []RecommendedAction `json:"recommended_actions"`
	GeneratedAt          time.Time           `json:"generated_at"`
}

// KeyDate represents one open deadline with urgency signaling.
type KeyDate struct {
	Kind          string `json:"kind"`
	EndDate       string `json:"end_date"`
	DaysRemaining int    `json:"days_remaining"`
	Urgency       string `json:"urgency"` // OVERDUE | DUE_SOON | OK
	Source        string `json:"source"`  // deadline_id or intimation reference
}

// RecentMovement represents one significant recent docket entry.
type RecentMovement struct {
	OccurredAt string `json:"occurred_at"`
	Text       string `json:"text"`
	Source     string `json:"source"` // docket_entry_id
}

// Risk represents one red flag detected in the process.
type Risk struct {
	Description string `json:"description"`
	Source      string `json:"source"` // document_id, docket_entry_id, or description
}

// RecommendedAction represents one suggested next step.
type RecommendedAction struct {
	Action string `json:"action"`
	Source string `json:"source"` // document_id, docket_entry_id, or description
}

// resumoReader is the narrow read the ResumoUseCase needs — one court_record's full
// context by id, tenant-scoped. The ReadUseCase satisfies it.
type resumoReader interface {
	GetResumoContext(ctx context.Context, tenantID, courtRecordID string) (ProcessoResumoCtx, error)
}

// ProcessoResumoCtx is the raw context returned by the repo read — the court_record
// identification plus the cached AI resume and the three aggregated slices (last 10
// docket entries, active intimations, open deadlines). ai_resume/generated_at come
// from the base row; the slices from the :many reads.
type ProcessoResumoCtx struct {
	ID                  string
	CNJNumber           string
	Court               string
	Degree              string
	Class               string
	Subject             string
	Lifecycle           string
	FiledAt             *time.Time
	JudgingBody         string
	AIResume            []byte
	AIResumeGeneratedAt *time.Time
	RecentMovements     []RawDocketEntry
	ActiveIntimations   []RawIntimation
	OpenDeadlines       []RawDeadline
}

// RawDocketEntry is one docket entry of the summary context.
type RawDocketEntry struct {
	OccurredAt time.Time
	Text       string
}

// RawIntimation is one active intimation of the summary context. DeadlineDays is 0
// when the intimação has no derived prazo yet.
type RawIntimation struct {
	Type         string
	Teor         string
	DeadlineDays int
}

// RawDeadline is one open deadline of the summary context.
type RawDeadline struct {
	Kind          string
	EndDate       time.Time
	DaysRemaining int
	Counting      string
}

// resumoStore persists the AI-generated resume best-effort (write-once, WHERE
// ai_resume IS NULL). nil skips the write-through entirely.
type resumoStore interface {
	SaveResumo(ctx context.Context, tenantID, courtRecordID string, rawJSON []byte) error
}

// ResumoUseCase composes the process context, calls the LLM for the summary, and
// returns the structured view. It depends only on ports so tests inject fakes and the
// LLM is never hit under test.
type ResumoUseCase struct {
	reader    resumoReader
	composer  advisory.PromptComposer
	gen       llm.Generator     // optional: nil when OpenRouter is unconfigured
	store     resumoStore       // optional: nil skips the ai_resume write-through
	embedder  indexing.Embedder // optional: nil skips RAG
	indexDeps indexing.SearchDeps
	model     string // provenance: the model the generator answers with by default
}

// NewResumoUseCase wires the process summarizer. gen may be nil (no LLM configured) —
// Resume then returns a deterministic DTO instead of failing, so the FE still works.
// store may be nil (no persistence); when present, the FIRST resume for a court_record
// is written through (best-effort) so later opens serve it from the DB instead of
// asking the LLM again. embedder may be nil (no Voyage key) — RAG is then skipped, the
// context is simply built without document chunks (best-effort, never an error).
func NewResumoUseCase(
	reader resumoReader,
	composer advisory.PromptComposer,
	gen llm.Generator,
	store resumoStore,
	embedder indexing.Embedder,
	indexDeps indexing.SearchDeps,
	model string,
) *ResumoUseCase {
	return &ResumoUseCase{
		reader:    reader,
		composer:  composer,
		gen:       gen,
		store:     store,
		embedder:  embedder,
		indexDeps: indexDeps,
		model:     model,
	}
}

// processResumeSchema constrains the model's output to the structured JSON shape via
// OpenRouter's json_schema structured output (strict). additionalProperties:false +
// required at all levels make the shape exact.
var processResumeSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": { "type": "string" },
    "current_status": { "type": "string" },
    "key_dates_and_deadlines": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "kind": { "type": "string" },
          "end_date": { "type": "string" },
          "days_remaining": { "type": "integer" },
          "urgency": { "type": "string" },
          "source": { "type": "string" }
        },
        "required": ["kind", "end_date", "days_remaining", "urgency", "source"],
        "additionalProperties": false
      }
    },
    "recent_movements": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "occurred_at": { "type": "string" },
          "text": { "type": "string" },
          "source": { "type": "string" }
        },
        "required": ["occurred_at", "text", "source"],
        "additionalProperties": false
      }
    },
    "risks": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "description": { "type": "string" },
          "source": { "type": "string" }
        },
        "required": ["description", "source"],
        "additionalProperties": false
      }
    },
    "recommended_actions": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "action": { "type": "string" },
          "source": { "type": "string" }
        },
        "required": ["action", "source"],
        "additionalProperties": false
      }
    }
  },
  "required": ["summary", "current_status", "key_dates_and_deadlines", "recent_movements", "risks", "recommended_actions"],
  "additionalProperties": false
}`)

// Resume returns the AI-generated process summary for a court_record.
// A missing/foreign court_record → the read's typed ErrNotFound (→ 404).
// With no generator configured it returns a deterministic DTO (no LLM call).
// An LLM/parse fault surfaces typed (the handler maps it).
//
// Cache behavior (sync-on-first-GET, write-once): if ai_resume_generated_at is already
// set, the persisted JSON is unmarshaled and returned (no LLM call). Otherwise the LLM
// is called, the result is parsed, persisted best-effort (WHERE ai_resume IS NULL),
// and returned. A store fault must never cost the lawyer the summary — log and keep
// the fresh answer.
func (uc *ResumoUseCase) Resume(ctx context.Context, tenantID, courtRecordID string) (ProcessResumoView, error) {
	// Read the full context (includes cached ai_resume if any)
	ctxData, err := uc.reader.GetResumoContext(ctx, tenantID, courtRecordID)
	if err != nil {
		return ProcessResumoView{}, err
	}

	// If already cached, serve it
	if ctxData.AIResumeGeneratedAt != nil && len(ctxData.AIResume) > 0 {
		var cached ProcessResumoView
		if err := json.Unmarshal(ctxData.AIResume, &cached); err != nil {
			slog.WarnContext(ctx, "acquisition: failed to unmarshal cached ai_resume, regenerating",
				slog.String("court_record_id", courtRecordID), slog.Any("error", err))
		} else {
			cached.GeneratedAt = *ctxData.AIResumeGeneratedAt
			return cached, nil
		}
	}

	// No generator configured → deterministic fallback (no LLM call, no persist)
	if uc.gen == nil {
		return uc.deterministicResume(ctxData), nil
	}

	// Compose the prompt context (best-effort RAG included when the embedder is wired)
	procCtx := uc.buildProcessContext(ctx, tenantID, courtRecordID, ctxData)

	composed, err := uc.composer.ComposeProcess(advisory.AgentSummarizeProcess, procCtx)
	if err != nil {
		return ProcessResumoView{}, err
	}

	out, err := uc.gen.GenerateJSON(ctx, llm.Request{
		System:     composed.System,
		User:       composed.User,
		Schema:     processResumeSchema,
		SchemaName: "process_resume",
		MaxTokens:  2000,
		UseCase:    "acquisition.resume_process",
		TenantID:   tenantID,
	})
	if err != nil {
		return ProcessResumoView{}, err
	}

	var parsed ProcessResumoView
	if err := json.Unmarshal(out, &parsed); err != nil {
		return ProcessResumoView{}, apperr.NewInfra("acquisition: parse process resume", err)
	}
	// Ensure slices are initialized (not nil)
	if parsed.KeyDatesAndDeadlines == nil {
		parsed.KeyDatesAndDeadlines = []KeyDate{}
	}
	if parsed.RecentMovements == nil {
		parsed.RecentMovements = []RecentMovement{}
	}
	if parsed.Risks == nil {
		parsed.Risks = []Risk{}
	}
	if parsed.RecommendedActions == nil {
		parsed.RecommendedActions = []RecommendedAction{}
	}
	parsed.GeneratedAt = time.Now()

	// Persist best-effort (write-once, WHERE ai_resume IS NULL)
	if uc.store != nil {
		if err := uc.store.SaveResumo(ctx, tenantID, courtRecordID, out); err != nil {
			slog.WarnContext(ctx, "acquisition: persist ai resume failed",
				slog.String("court_record_id", courtRecordID), slog.Any("error", err))
		}
	}

	return parsed, nil
}

// deterministicResume builds a deterministic DTO without LLM (degraded mode).
// summary="", risks=[], recommended_actions=[], generated_at=now. Does NOT persist.
func (uc *ResumoUseCase) deterministicResume(ctxData ProcessoResumoCtx) ProcessResumoView {
	return ProcessResumoView{
		Summary:              "",
		CurrentStatus:        currentStatusFromContext(ctxData),
		KeyDatesAndDeadlines: keyDatesFromContext(ctxData),
		RecentMovements:      recentMovementsFromContext(ctxData),
		Risks:                []Risk{},
		RecommendedActions:   []RecommendedAction{},
		GeneratedAt:          time.Now(),
	}
}

// buildProcessContext converts the raw repo context into the advisory.ProcessContext,
// truncating the aggregated slices to the LLM-context limits (10 movements / 500 chars,
// intimations 500 chars, deadlines raw, RAG top-5 of 300 chars). RAG is best-effort:
// when the embedder is wired, the identification text is embedded and the top-5 chunks
// for the process are searched; any fault (embedding, search, unconfigured) logs and
// yields an empty chunk list — never an error (the summary stands without documents).
func (uc *ResumoUseCase) buildProcessContext(ctx context.Context, tenantID, courtRecordID string, ctxData ProcessoResumoCtx) advisory.ProcessContext {
	var recentMovements []advisory.DocketEntryCtx
	for i, m := range ctxData.RecentMovements {
		if i >= 10 {
			break
		}
		recentMovements = append(recentMovements, advisory.DocketEntryCtx{
			OccurredAt: m.OccurredAt.Format(time.RFC3339),
			Text:       truncate(m.Text, 500),
		})
	}

	var activeIntimations []advisory.IntimationCtx
	for _, i := range ctxData.ActiveIntimations {
		activeIntimations = append(activeIntimations, advisory.IntimationCtx{
			Type:         i.Type,
			Teor:         truncate(i.Teor, 500),
			DeadlineDays: i.DeadlineDays,
		})
	}

	var openDeadlines []advisory.DeadlineCtx
	for _, d := range ctxData.OpenDeadlines {
		openDeadlines = append(openDeadlines, advisory.DeadlineCtx{
			Kind:          d.Kind,
			EndDate:       d.EndDate.Format(time.DateOnly),
			DaysRemaining: d.DaysRemaining,
			Counting:      d.Counting,
		})
	}

	// Best-effort RAG: skip when no embedder or no search pool is wired. Any fault is
	// a log-and-skip, never a 5xx — the process context is enough for a useful summary.
	var docChunks []string
	if uc.embedder != nil && uc.indexDeps.Pool != nil {
		queryText := ctxData.CNJNumber + " " + ctxData.Court + " " + ctxData.Class + " " + ctxData.Subject
		if vecs, _, err := uc.embedder.Embed(ctx, []string{queryText}, indexing.InputQuery); err != nil {
			slog.WarnContext(ctx, "acquisition: resume RAG embed skipped",
				slog.String("court_record_id", courtRecordID), slog.Any("error", err))
		} else if len(vecs) > 0 {
			hits, err := indexing.SearchChunks(ctx, uc.indexDeps, tenantID, &courtRecordID, vecs[0], 5)
			if err != nil {
				slog.WarnContext(ctx, "acquisition: resume RAG search skipped",
					slog.String("court_record_id", courtRecordID), slog.Any("error", err))
			} else {
				for _, h := range hits {
					docChunks = append(docChunks, truncate(h.Text, 300))
				}
			}
		}
	}

	filedAt := ""
	if ctxData.FiledAt != nil {
		filedAt = ctxData.FiledAt.Format(time.RFC3339)
	}

	return advisory.ProcessContext{
		CNJNumber:         ctxData.CNJNumber,
		Court:             ctxData.Court,
		Degree:            ctxData.Degree,
		Class:             ctxData.Class,
		Subject:           ctxData.Subject,
		Lifecycle:         ctxData.Lifecycle,
		FiledAt:           filedAt,
		RecentMovements:   recentMovements,
		ActiveIntimations: activeIntimations,
		OpenDeadlines:     openDeadlines,
		DocumentChunks:    docChunks,
		Playbook:          "",
	}
}

// currentStatusFromContext derives the "what is happening now" line deterministically:
// the first active intimação, else the first open prazo, else the last andamento.
func currentStatusFromContext(ctxData ProcessoResumoCtx) string {
	if len(ctxData.ActiveIntimations) > 0 {
		return "Intimação ativa: " + truncate(ctxData.ActiveIntimations[0].Teor, 200)
	}
	if len(ctxData.OpenDeadlines) > 0 {
		d := ctxData.OpenDeadlines[0]
		return "Prazo aberto: " + d.Kind + " vence em " + d.EndDate.Format("2006-01-02")
	}
	if len(ctxData.RecentMovements) > 0 {
		return "Último andamento: " + truncate(ctxData.RecentMovements[0].Text, 200)
	}
	return "Sem movimentações registradas"
}

// keyDatesFromContext maps the open deadlines to the DTO with deterministic urgency
// (OVERDUE < 0 days, DUE_SOON <= 5, else OK). Always returns a non-nil slice so the
// degraded DTO never serializes null (the FE renders arrays).
func keyDatesFromContext(ctxData ProcessoResumoCtx) []KeyDate {
	dates := make([]KeyDate, 0, len(ctxData.OpenDeadlines))
	for _, d := range ctxData.OpenDeadlines {
		urgency := "OK"
		if d.DaysRemaining < 0 {
			urgency = "OVERDUE"
		} else if d.DaysRemaining <= 5 {
			urgency = "DUE_SOON"
		}
		dates = append(dates, KeyDate{
			Kind:          d.Kind,
			EndDate:       d.EndDate.Format(time.DateOnly),
			DaysRemaining: d.DaysRemaining,
			Urgency:       urgency,
			Source:        "deadline",
		})
	}
	return dates
}

// recentMovementsFromContext maps the last 3 andamentos to the DTO. Always returns a
// non-nil slice so the degraded DTO never serializes null (the FE renders arrays).
func recentMovementsFromContext(ctxData ProcessoResumoCtx) []RecentMovement {
	limit := 3
	if len(ctxData.RecentMovements) < limit {
		limit = len(ctxData.RecentMovements)
	}
	movements := make([]RecentMovement, 0, limit)
	for i, m := range ctxData.RecentMovements {
		if i >= 3 {
			break
		}
		movements = append(movements, RecentMovement{
			OccurredAt: m.OccurredAt.Format(time.RFC3339),
			Text:       truncate(m.Text, 500),
			Source:     "docket_entry",
		})
	}
	return movements
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
