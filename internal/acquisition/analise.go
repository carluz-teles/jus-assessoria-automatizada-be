package acquisition

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/lib/llm"
)

// analise.go is the AI analysis of ONE intimation (POST /v1/intimacoes/:id/analise — the
// "Analisar esta intimação" card). It mirrors resumo.go's shape (read context → compose
// prompt → LLM → persist best-effort → return the view) but differs in two ways: it is a
// POST (write on the write path is the point, not a side-effect of a GET) and it is
// RE-EXECUTABLE — "Gerar novamente" re-runs and OVERWRITES (no write-once guard). Degraded
// mode (gen==nil or an LLM/parse fault) persists+returns summary="" + providencias=[] with
// a fresh analyzed_at, so the FE distinguishes "not analysed" (analyzed_at nil) from
// "analysed but IA unavailable" (analyzed_at set, summary empty).

// IntimacaoAnaliseView is the wire DTO returned by POST /v1/intimacoes/:id/analise.
// Providencias is always initialized so it serializes as [], never null.
type IntimacaoAnaliseView struct {
	Summary string `json:"summary"`
	// Ato é o ato principal classificado pela IA (ex.: "Contestação") — vira o TÍTULO
	// do detalhe e o pill "Ato". "" no modo degradado / análise antiga.
	Ato          string                     `json:"ato"`
	Providencias []IntimacaoProvidenciaView `json:"providencias"`
	AnalyzedAt   time.Time                  `json:"analyzed_at"`
}

// IntimacaoAnaliseCtx is the raw context the repo returns for the analysis prompt: the
// intimation's teor + type, its court record's identification, the derived prazo end_date
// (when a deadline exists — the horizon the IA must keep every due_date at or before), and
// the firm's active members (so the IA can pick a real responsável by id). Type is a pointer
// (the column is nullable); DeadlineEndDate is "" when the intimation has no prazo yet.
type IntimacaoAnaliseCtx struct {
	Content         string
	Type            *string
	CNJNumber       string
	Court           string
	Degree          string
	Class           string
	Subject         string
	DeadlineEndDate string      // "2006-01-02" or "" when no deadline
	Members         []MemberCtx // active app_users of the tenant (id + name)
}

// MemberCtx is one firm member the IA may assign a providência to: the internal app_user id
// (never org_id) plus the display name. The prompt lists these so the model returns a real
// suggested_assignee_user_id, which the approval flow forwards to POST /v1/tasks unchanged.
type MemberCtx struct {
	UserID string
	Name   string
}

// analiseReader is the narrow read the AnaliseUseCase needs — one intimation's analysis
// context by id, tenant-scoped. The ReadUseCase satisfies it.
type analiseReader interface {
	GetIntimacaoAnaliseContext(ctx context.Context, tenantID, intimationID string) (IntimacaoAnaliseCtx, error)
}

// analiseStore persists the AI analysis (OVERWRITE — re-executable). nil skips the write.
// logActivity tells the store whether this write should also append a process activity log
// row (true on a real/successful analysis, false on the degraded write — a degraded analysis
// is not something to surface on the process timeline).
type analiseStore interface {
	SaveAnalise(ctx context.Context, tenantID, intimationID, summary, ato string, providencias []byte, logActivity bool) error
}

// AnaliseUseCase composes the intimation context, calls the LLM for the analysis, persists
// it, and returns the view. It depends only on ports so tests inject fakes and the LLM is
// never hit under test — the same design as ResumoUseCase.
type AnaliseUseCase struct {
	reader   analiseReader
	composer advisory.PromptComposer
	gen      llm.Generator // optional: nil when the LLM is unconfigured (degraded mode)
	store    analiseStore  // optional: nil skips persistence
	model    string        // OpenRouter slug; "" cai no default do generator
}

// NewAnaliseUseCase wires the intimation analyzer. gen may be nil (no LLM configured) —
// Analisar then persists+returns the degraded DTO (summary="", providencias=[]) instead of
// failing, so the FE still transitions to the pós-análise state. store may be nil (no
// persistence), in which case the analysis is computed but not written through.
// model = "" mantém o modelo default do generator (cfg.OpenRouterModel legado).
func NewAnaliseUseCase(reader analiseReader, composer advisory.PromptComposer, gen llm.Generator, store analiseStore, model string) *AnaliseUseCase {
	return &AnaliseUseCase{reader: reader, composer: composer, gen: gen, store: store, model: model}
}

// intimationAnalysisSchema constrains the model's output to {summary, providencias:[{title,
// description, suggested_assignee_user_id, due_date}]} via OpenRouter's json_schema structured
// output (strict). suggested_assignee_user_id / due_date are nullable strings — the model may
// emit null when it can't pick a member or a date; the use case then leaves them nil. status is
// NOT in the schema — every generated providência is server-stamped SUGGESTED (§1).
var intimationAnalysisSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": { "type": "string" },
    "ato": { "type": "string" },
    "providencias": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "title": { "type": "string" },
          "description": { "type": "string" },
          "kind": { "type": "string", "enum": ["PECA", "CIENCIA"] },
          "suggested_assignee_user_id": { "type": ["string", "null"] },
          "due_date": { "type": ["string", "null"] }
        },
        "required": ["title", "description", "kind", "suggested_assignee_user_id", "due_date"],
        "additionalProperties": false
      }
    }
  },
  "required": ["summary", "ato", "providencias"],
  "additionalProperties": false
}`)

// maxProvidencias caps the derived list so a runaway model output can't bloat the row/UI.
const maxProvidencias = 12

// Analisar generates (and persists) the AI analysis of one intimation.
// A missing/foreign intimation → the read's typed ErrIntimationNotFound (→ 404).
// With no generator configured it persists+returns the degraded DTO (no LLM call).
// An LLM/parse fault ALSO degrades (log-not-fail): the lawyer still gets a pós-análise
// state, just empty — a failed analysis must not 5xx the button. The analysis is
// re-executable: every call OVERWRITES the persisted columns.
func (uc *AnaliseUseCase) Analisar(ctx context.Context, tenantID, intimationID string) (IntimacaoAnaliseView, error) {
	ctxData, err := uc.reader.GetIntimacaoAnaliseContext(ctx, tenantID, intimationID)
	if err != nil {
		return IntimacaoAnaliseView{}, err
	}

	// No generator configured → degraded (persist empty, return empty).
	if uc.gen == nil {
		return uc.persistDegraded(ctx, tenantID, intimationID), nil
	}

	view, ok := uc.generate(ctx, tenantID, intimationID, ctxData)
	if !ok {
		// Any LLM/compose/parse fault degrades — never a 5xx on the analyze button.
		return uc.persistDegraded(ctx, tenantID, intimationID), nil
	}

	// Persist best-effort (OVERWRITE). A store fault must not cost the answer — log and keep it.
	rawProv, err := json.Marshal(view.Providencias)
	if err != nil {
		rawProv = []byte("[]")
	}
	uc.persist(ctx, tenantID, intimationID, view.Summary, view.Ato, rawProv, true)
	return view, nil
}

// generate composes the prompt, calls the LLM, parses+sanitizes the output. ok=false on any
// fault (the caller degrades). Never returns an error — faults are logged here.
func (uc *AnaliseUseCase) generate(ctx context.Context, tenantID, intimationID string, ctxData IntimacaoAnaliseCtx) (IntimacaoAnaliseView, bool) {
	members := make([]advisory.MemberCtx, 0, len(ctxData.Members))
	for _, m := range ctxData.Members {
		members = append(members, advisory.MemberCtx{UserID: m.UserID, Name: m.Name})
	}
	composed, err := uc.composer.Compose(advisory.AgentAnalyzeIntimation, advisory.CaseContext{
		Court:          ctxData.Court,
		Degree:         ctxData.Degree,
		Class:          ctxData.Class,
		Subject:        ctxData.Subject,
		IntimationType: deref(ctxData.Type),
		IntimationText: ctxData.Content,
		DeadlineDate:   ctxData.DeadlineEndDate,
		Members:        members,
	})
	if err != nil {
		slog.WarnContext(ctx, "acquisition: compose intimation analysis failed",
			slog.String("intimation_id", intimationID), slog.Any("error", err))
		return IntimacaoAnaliseView{}, false
	}

	out, err := uc.gen.GenerateJSON(ctx, llm.Request{
		System:     composed.System,
		User:       composed.User,
		Schema:     intimationAnalysisSchema,
		SchemaName: "intimation_analysis",
		Model:      uc.model, // "" = cai no default do generator
		MaxTokens:  1500,
		UseCase:    "acquisition.analyze_intimation",
		TenantID:   tenantID,
	})
	if err != nil {
		slog.WarnContext(ctx, "acquisition: intimation analysis generation failed",
			slog.String("intimation_id", intimationID), slog.Any("error", err))
		return IntimacaoAnaliseView{}, false
	}

	var parsed struct {
		Summary      string `json:"summary"`
		Ato          string `json:"ato"`
		Providencias []struct {
			Title                   string  `json:"title"`
			Description             string  `json:"description"`
			Kind                    string  `json:"kind"`
			SuggestedAssigneeUserID *string `json:"suggested_assignee_user_id"`
			DueDate                 *string `json:"due_date"`
		} `json:"providencias"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		slog.WarnContext(ctx, "acquisition: parse intimation analysis failed",
			slog.String("intimation_id", intimationID), slog.Any("error", err))
		return IntimacaoAnaliseView{}, false
	}

	prov := make([]IntimacaoProvidenciaView, 0, len(parsed.Providencias))
	for i, p := range parsed.Providencias {
		if i >= maxProvidencias {
			break
		}
		assigneeID, assigneeName := resolveAssignee(p.SuggestedAssigneeUserID, ctxData.Members)
		prov = append(prov, IntimacaoProvidenciaView{
			Title:                   p.Title,
			Description:             p.Description,
			Kind:                    normalizeProvidenciaKind(p.Kind),
			SuggestedAssigneeUserID: assigneeID,
			SuggestedAssigneeName:   assigneeName,
			DueDate:                 clampDueDate(p.DueDate, ctxData.DeadlineEndDate),
			Status:                  ProvidenciaStatusSuggested,
		})
	}
	return IntimacaoAnaliseView{
		Summary:      parsed.Summary,
		Ato:          strings.TrimSpace(parsed.Ato),
		Providencias: prov,
		AnalyzedAt:   time.Now(),
	}, true
}

// normalizeProvidenciaKind mantém só os valores conhecidos ("PECA"|"CIENCIA") — o
// schema já restringe o enum, mas isto blinda contra caixa/valor inesperado (degrada
// para "" = sem chip de tipo, nunca lixo na UI).
func normalizeProvidenciaKind(kind string) string {
	switch strings.ToUpper(strings.TrimSpace(kind)) {
	case ProvidenciaKindPeca:
		return ProvidenciaKindPeca
	case ProvidenciaKindCiencia:
		return ProvidenciaKindCiencia
	default:
		return ""
	}
}

// resolveAssignee accepts the model's suggested id ONLY when it matches a real firm member
// (a hallucinated / omitted id degrades to nil,nil → the FE shows "—"). Returns the id plus
// the resolved display name so the row is self-describing (the FE needn't re-join members).
func resolveAssignee(suggested *string, members []MemberCtx) (*string, *string) {
	if suggested == nil {
		return nil, nil
	}
	id := *suggested
	for _, m := range members {
		if m.UserID == id {
			name := m.Name
			return &id, &name
		}
	}
	return nil, nil
}

// clampDueDate keeps the model's due_date only when it parses as YYYY-MM-DD and is ≤ the prazo
// end_date (when a prazo exists). A malformed date, or one after the deadline, degrades to nil
// (the FE shows "—") — never silently pushes a task past its legal horizon.
func clampDueDate(due *string, endDate string) *string {
	if due == nil {
		return nil
	}
	d, err := time.Parse("2006-01-02", strings.TrimSpace(*due))
	if err != nil {
		return nil
	}
	if endDate != "" {
		end, err := time.Parse("2006-01-02", endDate)
		if err == nil && d.After(end) {
			return nil
		}
	}
	out := d.Format("2006-01-02")
	return &out
}

// persistDegraded writes+returns the empty analysis (summary="", providencias=[]) with a
// fresh analyzed_at, so the FE moves to pós-análise and shows the "IA indisponível" state.
func (uc *AnaliseUseCase) persistDegraded(ctx context.Context, tenantID, intimationID string) IntimacaoAnaliseView {
	uc.persist(ctx, tenantID, intimationID, "", "", []byte("[]"), false)
	return IntimacaoAnaliseView{Summary: "", Providencias: []IntimacaoProvidenciaView{}, AnalyzedAt: time.Now()}
}

// persistWrite write-throughs best-effort; a store fault is logged, never returned (the lawyer
// keeps the answer even if the row didn't update). logActivity=true only on a real analysis —
// the degraded write (no LLM / LLM fault) never logs a process activity row.
func (uc *AnaliseUseCase) persist(ctx context.Context, tenantID, intimationID, summary, ato string, providencias []byte, logActivity bool) {
	if uc.store == nil {
		return
	}
	if err := uc.store.SaveAnalise(ctx, tenantID, intimationID, summary, ato, providencias, logActivity); err != nil {
		slog.WarnContext(ctx, "acquisition: persist intimation analysis failed",
			slog.String("intimation_id", intimationID), slog.Any("error", err))
	}
}
