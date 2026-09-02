package acquisition

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
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
//
// docs/erd-costura-providencia-tarefa-peca.md: this use case NO LONGER persists the
// providências as jsonb on the intimation row — it publishes acquisition.intimation.
// analyzed (in the SAME tx as the ai_summary write) carrying the providência candidates,
// and the actionitem slice's listener materializes real action_item rows from it. This
// slice never imports actionitem's entity/repo (slices talk by event, decisão P1); the
// authoritative tipo/piece_profile_key sanitization also lives there (single source of
// truth) — this use case only normalizes (trim/lowercase) what it forwards.

// IntimacaoAnaliseView is the wire DTO returned by POST /v1/intimacoes/:id/analise.
// Providencias is always initialized so it serializes as [], never null.
type IntimacaoAnaliseView struct {
	Summary string `json:"summary"`
	// Ato é o ato principal classificado pela IA (ex.: "Contestação") — vira o TÍTULO
	// do detalhe e o pill "Ato". "" no modo degradado / análise antiga.
	Ato          string                   `json:"ato"`
	Providencias []AnaliseProvidenciaView `json:"providencias"`
	AnalyzedAt   time.Time                `json:"analyzed_at"`
}

// AnaliseProvidenciaView is one providência CANDIDATE the IA identified — the immediate
// response of "Analisar", BEFORE materialization. It is intentionally richer than the
// persisted action_item row (Title/Description/SuggestedAssignee/DueDate exist only here,
// for the analysis card's display — action_item carries no such columns, docs §2) and
// intentionally thinner on identity (no id/status/task_id: those only exist once the
// actionitem listener has materialized the candidate, which happens asynchronously after
// this response is already on the wire).
type AnaliseProvidenciaView struct {
	Title                   string  `json:"title"`
	Description             string  `json:"description"`
	SuggestedAssigneeUserID *string `json:"suggested_assignee_user_id"`
	SuggestedAssigneeName   *string `json:"suggested_assignee_name"`
	DueDate                 *string `json:"due_date"` // "2006-01-02" or null
	// Tipo/GeraPeca/PieceProfileKey/Declarado/Confianca are the classification the
	// actionitem slice's listener turns into an action_item row (docs §3's motor de
	// precedência): Declarado marks a teor that stated the tipo explicitly (→
	// tipo_origem=declarado, confiável); otherwise the IA inferred it (→ tipo_origem=ia,
	// a_confirmar) and Confianca carries its confidence.
	Tipo            string   `json:"tipo"`
	GeraPeca        bool     `json:"gera_peca"`
	PieceProfileKey *string  `json:"piece_profile_key"`
	Declarado       bool     `json:"declarado"`
	Confianca       *float64 `json:"confianca"`
}

// IntimacaoAnaliseCtx is the raw context the repo returns for the analysis prompt: the
// intimation's teor + type, its court record's identification, the derived prazo end_date
// (when a deadline exists — the horizon the IA must keep every due_date at or before), and
// the firm's active members (so the IA can pick a real responsável by id). Type is a pointer
// (the column is nullable); DeadlineEndDate is "" when the intimation has no prazo yet.
// CourtRecordID/DeadlineID ("" when no prazo yet) are carried through unchanged onto the
// acquisition.intimation.analyzed event — the actionitem slice's materialized rows need both.
type IntimacaoAnaliseCtx struct {
	Content         string
	Type            *string
	CourtRecordID   string
	CNJNumber       string
	Court           string
	Degree          string
	Class           string
	Subject         string
	DeadlineID      string      // "" when no deadline yet
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

// PieceProfileOption is one entry of the GLOBAL peça catalog (piece_profile — key/nome/polo,
// no tenant_id). Injected into the analyze_intimation prompt as the closed list the model
// picks piece_profile_key from, and used to build the dynamic structured-output enum for that
// field (buildIntimationAnalysisSchema).
type PieceProfileOption struct {
	Key  string
	Nome string
	Polo string
}

// analiseReader is the narrow read the AnaliseUseCase needs — one intimation's analysis
// context by id, tenant-scoped, plus the GLOBAL peça catalog. The ReadUseCase satisfies it.
type analiseReader interface {
	GetIntimacaoAnaliseContext(ctx context.Context, tenantID, intimationID string) (IntimacaoAnaliseCtx, error)
	// ListPieceProfiles returns the global piece_profile catalog (key/nome/polo). Not
	// tenant-scoped (the catalog is shared). A fault degrades to an empty catalog — the
	// analysis still runs, just without the enum/list injection.
	ListPieceProfiles(ctx context.Context) ([]PieceProfileOption, error)
}

// SaveAnaliseParams is the analiseStore.SaveAnalise input: the intimation's fresh
// ai_summary + the providência candidates to publish on acquisition.intimation.analyzed, in
// one tx. DeadlineID/CourtRecordID ride through from IntimacaoAnaliseCtx untouched — the
// store has no other way to know them. LogActivity=true also appends a process_activity_log
// row (a real, successful analysis); false is the degraded write (no LLM / LLM fault).
type SaveAnaliseParams struct {
	TenantID     string
	IntimationID string
	Summary      string
	// Ato é o ato principal classificado pela IA (ex.: "Contestação") — persistido em
	// intimation.ai_act (migration 0082). "" na escrita degradada.
	Ato          string
	DeadlineID   string
	Providencias []ProvidenciaCandidate
	LogActivity  bool
}

// analiseStore persists the AI analysis (OVERWRITE — re-executable) and publishes
// acquisition.intimation.analyzed in the SAME tx. nil skips both.
type analiseStore interface {
	SaveAnalise(ctx context.Context, p SaveAnaliseParams) error
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

// buildIntimationAnalysisSchema constrains the model's output to {summary, ato, providencias:
// [{title, description, suggested_assignee_user_id, due_date, tipo, gera_peca,
// piece_profile_key, declarado, confianca}]} via OpenRouter's json_schema structured output
// (strict). tipo is the providência classification (docs §2: contestar|recorrer|manifestar|
// cumprir|ciencia); gera_peca + piece_profile_key name the tipo de peça when one is required;
// declarado marks whether the teor STATED the tipo explicitly (vs. the model inferring it);
// confianca is the model's confidence in ITS OWN inference (only meaningful when declarado is
// false — the use case ignores it otherwise). suggested_assignee_user_id / due_date /
// piece_profile_key / confianca are nullable. status is NOT in the schema — providência
// lifecycle is owned by the actionitem slice, not this analysis.
//
// piece_profile_key is DYNAMIC: when the catalog is non-empty its enum is exactly the catalog
// keys + null, so the model can only emit a real peça key (or null). An empty catalog falls
// back to a plain ["string","null"] with no enum (the actionitem sanitizer still degrades an
// unknown key downstream — belt-and-suspenders).
func buildIntimationAnalysisSchema(profiles []PieceProfileOption) json.RawMessage {
	pieceProfileKeySchema := `{ "type": ["string", "null"] }`
	if len(profiles) > 0 {
		enum := make([]string, 0, len(profiles)+1)
		for _, p := range profiles {
			enum = append(enum, strconv.Quote(p.Key))
		}
		enum = append(enum, "null")
		pieceProfileKeySchema = `{ "type": ["string", "null"], "enum": [` + strings.Join(enum, ", ") + `] }`
	}
	return json.RawMessage(`{
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
          "suggested_assignee_user_id": { "type": ["string", "null"] },
          "due_date": { "type": ["string", "null"] },
          "tipo": { "type": "string", "enum": ["contestar", "recorrer", "manifestar", "cumprir", "ciencia"] },
          "gera_peca": { "type": "boolean" },
          "piece_profile_key": ` + pieceProfileKeySchema + `,
          "declarado": { "type": "boolean" },
          "confianca": { "type": ["number", "null"] }
        },
        "required": ["title", "description", "suggested_assignee_user_id", "due_date",
          "tipo", "gera_peca", "piece_profile_key", "declarado", "confianca"],
        "additionalProperties": false
      }
    }
  },
  "required": ["summary", "ato", "providencias"],
  "additionalProperties": false
}`)
}

// maxProvidencias caps the derived list so a runaway model output can't bloat the row/UI.
const maxProvidencias = 12

// Analisar generates (and persists) the AI analysis of one intimation.
// A missing/foreign intimation → the read's typed ErrIntimationNotFound (→ 404).
// With no generator configured it persists+returns the degraded DTO (no LLM call).
// An LLM/parse fault ALSO degrades (log-not-fail): the lawyer still gets a pós-análise
// state, just empty — a failed analysis must not 5xx the button. The analysis is
// re-executable: every call OVERWRITES the persisted columns and re-publishes
// acquisition.intimation.analyzed (the actionitem listener's guard aditivo decides what a
// re-run may replace — see internal/actionitem/domain.go).
func (uc *AnaliseUseCase) Analisar(ctx context.Context, tenantID, intimationID string) (IntimacaoAnaliseView, error) {
	ctxData, err := uc.reader.GetIntimacaoAnaliseContext(ctx, tenantID, intimationID)
	if err != nil {
		return IntimacaoAnaliseView{}, err
	}

	// No generator configured → degraded (persist empty, return empty).
	if uc.gen == nil {
		return uc.persistDegraded(ctx, tenantID, intimationID, ctxData.DeadlineID), nil
	}

	view, ok := uc.generate(ctx, tenantID, intimationID, ctxData)
	if !ok {
		// Any LLM/compose/parse fault degrades — never a 5xx on the analyze button.
		return uc.persistDegraded(ctx, tenantID, intimationID, ctxData.DeadlineID), nil
	}

	// Persist best-effort (OVERWRITE). A store fault must not cost the answer — log and keep it.
	uc.persist(ctx, tenantID, intimationID, view.Summary, view.Ato, ctxData.DeadlineID, candidatesFromView(view.Providencias), true)
	return view, nil
}

// generate composes the prompt, calls the LLM, parses+sanitizes the output. ok=false on any
// fault (the caller degrades). Never returns an error — faults are logged here.
func (uc *AnaliseUseCase) generate(ctx context.Context, tenantID, intimationID string, ctxData IntimacaoAnaliseCtx) (IntimacaoAnaliseView, bool) {
	members := make([]advisory.MemberCtx, 0, len(ctxData.Members))
	for _, m := range ctxData.Members {
		members = append(members, advisory.MemberCtx{UserID: m.UserID, Name: m.Name})
	}

	// Fetch the global peça catalog — the closed list the prompt lists + the schema enums
	// piece_profile_key over. A fault degrades to an empty catalog (analysis still runs; the
	// schema falls back to string|null and the actionitem sanitizer guards downstream).
	profiles, err := uc.reader.ListPieceProfiles(ctx)
	if err != nil {
		slog.WarnContext(ctx, "acquisition: list piece profiles failed (degrading to empty catalog)",
			slog.String("intimation_id", intimationID), slog.Any("error", err))
		profiles = nil
	}
	pieceProfiles := make([]advisory.PieceProfileOption, 0, len(profiles))
	for _, p := range profiles {
		pieceProfiles = append(pieceProfiles, advisory.PieceProfileOption{Key: p.Key, Nome: p.Nome, Polo: p.Polo})
	}

	composed, err := uc.composer.Compose(advisory.AgentAnalyzeIntimation, advisory.CaseContext{
		Court:          ctxData.Court,
		Degree:         ctxData.Degree,
		Class:          ctxData.Class,
		Subject:        ctxData.Subject,
		IntimationType: deref(ctxData.Type),
		IntimationText: htmlPlaintext(ctxData.Content),
		DeadlineDate:   ctxData.DeadlineEndDate,
		Members:        members,
		PieceProfiles:  pieceProfiles,
	})
	if err != nil {
		slog.WarnContext(ctx, "acquisition: compose intimation analysis failed",
			slog.String("intimation_id", intimationID), slog.Any("error", err))
		return IntimacaoAnaliseView{}, false
	}

	out, err := uc.gen.GenerateJSON(ctx, llm.Request{
		System:     composed.System,
		User:       composed.User,
		Schema:     buildIntimationAnalysisSchema(profiles),
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
			Title                   string   `json:"title"`
			Description             string   `json:"description"`
			SuggestedAssigneeUserID *string  `json:"suggested_assignee_user_id"`
			DueDate                 *string  `json:"due_date"`
			Tipo                    string   `json:"tipo"`
			GeraPeca                bool     `json:"gera_peca"`
			PieceProfileKey         *string  `json:"piece_profile_key"`
			Declarado               bool     `json:"declarado"`
			Confianca               *float64 `json:"confianca"`
		} `json:"providencias"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		slog.WarnContext(ctx, "acquisition: parse intimation analysis failed",
			slog.String("intimation_id", intimationID), slog.Any("error", err))
		return IntimacaoAnaliseView{}, false
	}

	prov := make([]AnaliseProvidenciaView, 0, len(parsed.Providencias))
	for i, p := range parsed.Providencias {
		if i >= maxProvidencias {
			break
		}
		assigneeID, assigneeName := resolveAssignee(p.SuggestedAssigneeUserID, ctxData.Members)
		confianca := p.Confianca
		if p.Declarado {
			// A declared tipo needs no confidence score — the teor said it outright.
			confianca = nil
		}
		prov = append(prov, AnaliseProvidenciaView{
			Title:                   p.Title,
			Description:             p.Description,
			SuggestedAssigneeUserID: assigneeID,
			SuggestedAssigneeName:   assigneeName,
			DueDate:                 clampDueDate(p.DueDate, ctxData.DeadlineEndDate),
			Tipo:                    normalizeTipo(p.Tipo),
			GeraPeca:                p.GeraPeca,
			PieceProfileKey:         normalizeOptional(p.PieceProfileKey),
			Declarado:               p.Declarado,
			Confianca:               confianca,
		})
	}
	return IntimacaoAnaliseView{
		Summary:      parsed.Summary,
		Ato:          strings.TrimSpace(parsed.Ato),
		Providencias: prov,
		AnalyzedAt:   time.Now(),
	}, true
}

// normalizeTipo trims/lowercases the model's tipo so a trivial casing/whitespace quirk does
// not fail actionitem's closed-set check downstream. It performs NO validation itself — the
// actionitem slice owns the authoritative closed set + the safe-degrade fallback (single
// source of truth), since acquisition never imports it.
func normalizeTipo(t string) string {
	return strings.ToLower(strings.TrimSpace(t))
}

// normalizeOptional trims a nullable string field, collapsing a blank result to nil so an
// empty piece_profile_key round-trips as JSON null rather than "".
func normalizeOptional(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	return &v
}

// candidatesFromView narrows the rich response DTO to the minimal event payload
// (acquisition.intimation.analyzed carries only what actionitem needs — docs handoff
// "Payload mínimo"): title, description, tipo, gera_peca, piece_profile_key, confianca,
// declarado — title/description agora viajam para serem persistidos no action_item (0090).
func candidatesFromView(prov []AnaliseProvidenciaView) []ProvidenciaCandidate {
	out := make([]ProvidenciaCandidate, 0, len(prov))
	for _, p := range prov {
		out = append(out, ProvidenciaCandidate{
			Title:           p.Title,
			Description:     p.Description,
			Tipo:            p.Tipo,
			GeraPeca:        p.GeraPeca,
			PieceProfileKey: p.PieceProfileKey,
			Declarado:       p.Declarado,
			Confianca:       p.Confianca,
		})
	}
	return out
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
func (uc *AnaliseUseCase) persistDegraded(ctx context.Context, tenantID, intimationID, deadlineID string) IntimacaoAnaliseView {
	uc.persist(ctx, tenantID, intimationID, "", "", deadlineID, nil, false)
	return IntimacaoAnaliseView{Summary: "", Providencias: []AnaliseProvidenciaView{}, AnalyzedAt: time.Now()}
}

// persist write-throughs best-effort; a store fault is logged, never returned (the lawyer
// keeps the answer even if the row didn't update / the event didn't publish). logActivity=
// true only on a real analysis — the degraded write (no LLM / LLM fault) never logs a
// process activity row.
func (uc *AnaliseUseCase) persist(ctx context.Context, tenantID, intimationID, summary, ato, deadlineID string, candidates []ProvidenciaCandidate, logActivity bool) {
	if uc.store == nil {
		return
	}
	err := uc.store.SaveAnalise(ctx, SaveAnaliseParams{
		TenantID:     tenantID,
		IntimationID: intimationID,
		Summary:      summary,
		Ato:          ato,
		DeadlineID:   deadlineID,
		Providencias: candidates,
		LogActivity:  logActivity,
	})
	if err != nil {
		slog.WarnContext(ctx, "acquisition: persist intimation analysis failed",
			slog.String("intimation_id", intimationID), slog.Any("error", err))
	}
}
