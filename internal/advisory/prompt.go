// Package advisory holds the cross-cutting meta-prompt framework of the IA advisory
// (docs/erd-ai-advisory.md §3). Its heart is the PromptComposer: instead of a single static
// prompt per feature ("raso ou vira um monstro que dá drift"), it COMPOSES the instruction-set
// PER CASE from the case context (tribunal/rito/tipo de peça/fase), a versioned rules layer, and
// the firm's playbook. Every composition carries a prompt_version so its output correlates with
// the human-edit delta + real outcomes (§6, the feedback loop) — the basis of the A/B that
// improves the prompt.
//
// v0 is DETERMINISTIC composition (templates + context), which the ERD says already delivers 80%.
// The "meta-prompt real" (an LLM step that generates/refines the templates against a golden set)
// is a later fatia (§8). The Playbook field is the injection point for that evolution: today a
// stub (empty), tomorrow fed with the firm's human-APPROVED examples derived from the deltas —
// so plugging it in later refactors nothing.
package advisory

import (
	"strconv"
	"strings"

	"github.com/jusassessoria/platform/lib/apperr"
)

// CaseContext is the per-case signal the composer specializes the instruction-set with. Every
// field is optional (the caller injects whatever the case has); the composer omits the empty ones
// from the rendered prompt so a sparse case never produces dangling labels. Mirrors the signals
// erd-ai-advisory.md §3 lists (court/degree/class/subject/piece_type/fase) plus the prazo the
// suggestion hangs on and — the richest signal when present — the intimação's own text.
type CaseContext struct {
	Court          string // sigla do tribunal (TJSP, TRT2, STJ…)
	Degree         string // G1|G2|JE|SUPERIOR…
	Class          string // classe/rito processual
	Subject        string // assunto
	IntimationType string // CITACAO|INTIMACAO|COMUNICACAO…
	IntimationText string // o teor da intimação, quando disponível — o contexto mais rico
	PrazoKind      string // CONTESTACAO|RECURSO|MANIFESTACAO… (o kind do prazo derivado)
	PrazoDays      int    // dias do prazo
	Counting       string // BUSINESS|CALENDAR
	Phase          string // fase processual, quando conhecida

	// Playbook é o ponto de injeção da "voz do escritório" (§3). v0: stub (vazio → nada é
	// injetado). Depois: exemplos de tarefas HUMANO-APROVADAS (derivados do delta suggested×
	// confirmed) entram aqui como few-shot, e o prompt "aprende" o padrão do escritório sem um
	// LLM reescrever instruções. Preencher no futuro não muda a forma do Compose.
	Playbook string
}

// DocketEntryCtx is one recent docket entry for the process summary context.
type DocketEntryCtx struct {
	OccurredAt string // ISO date
	Text       string // truncado a 500 chars
}

// IntimationCtx is one active intimation for the process summary context.
type IntimationCtx struct {
	Type         string
	Teor         string // truncado a 500 chars
	DeadlineDays int    // 0 se sem prazo
}

// DeadlineCtx is one open deadline for the process summary context.
type DeadlineCtx struct {
	Kind          string
	EndDate       string // ISO date
	DaysRemaining int
	Counting      string // BUSINESS|CALENDAR
}

// ProcessContext is the full process context for the summarize_process agent.
// It aggregates the court_record identification, recent movements, active intimations,
// open deadlines, and optional RAG document chunks.
type ProcessContext struct {
	// court_record identification
	CNJNumber  string
	Court      string
	Degree     string
	Class      string
	Subject    string
	ClaimValue string // formatado; vazio se nil
	Lifecycle  string // ACTIVE|SUSPENDED|ARCHIVED|SUPERSEDED
	FiledAt    string // ISO date; vazio se nil

	// dados agregados (pré-truncados pelo use case)
	RecentMovements   []DocketEntryCtx // últimos 10, text truncado 500 chars
	ActiveIntimations []IntimationCtx  // todas com status = ACTIVE
	OpenDeadlines     []DeadlineCtx    // todas com status != CLOSED
	DocumentChunks    []string         // RAG top-5, cada chunk ~300 chars

	Playbook string // sempre vazio em v0
}

// Composed is the composer's output: the system + user messages and the prompt_version that
// produced them. The caller passes System/User to the LLM (lib/llm) and stamps PromptVersion on
// the persisted suggestion (proveniência) so the feedback delta is comparable across versions.
type Composed struct {
	System        string
	User          string
	PromptVersion string
}

// DraftContext is the per-draft signal the draft_minuta composer specializes the
// instruction-set with. It carries the intimation that triggered the draft (the richest
// context signal), the process metadata (court/class/subject, for legal register), and
// any RAG chunks retrieved from the case corpus (empty when the embedder is unconfigured
// or the corpus has no documents — the degraded path).
type DraftContext struct {
	PieceType      string // DEFENSE|COMPLAINT|APPEAL|MOTION|OTHER
	IntimationType string // CITACAO|INTIMACAO|COMUNICACAO…
	IntimationText string // teor da intimação — o contexto mais rico
	Court          string
	Degree         string
	Class          string
	Subject        string
	// Chunks are the RAG top-K hits (text only). Empty → grounded=false (degraded).
	Chunks   []string
	Playbook string // always empty in v0
}

// PromptComposer composes the instruction-set for a named advisory agent from a case context.
// It is a first-class, versioned artifact (§3): the agent name + version identify exactly which
// template ran. Behind an interface so the deterministic v0 (TemplateComposer) can later be
// swapped for / wrapped by an LLM-refined composer (§8) without touching the callers.
type PromptComposer interface {
	Compose(agent string, c CaseContext) (Composed, error)
	ComposeProcess(agent string, c ProcessContext) (Composed, error)
	ComposeDraft(agent string, c DraftContext) (Composed, error)
}

// Agent identifiers — one per specialized advisory task (erd-ai-advisory.md §4). Each maps to a
// template + a version below. Kept as consts so a caller never passes a free string.
const (
	AgentSuggestTasks     = "suggest_tasks"
	AgentSummarizeProcess = "summarize_process"
	AgentDraftMinuta      = "draft_minuta"
)

// suggestTasksVersion is the pinned version of the suggest_tasks template. BUMP IT whenever the
// template text changes so the feedback delta of the OLD prompt stays attributable to the OLD
// version (never silently mixed with a new one). This is the axis the A/B improvement turns on.
const suggestTasksVersion = "suggest_tasks/v2"

// summarizeProcessVersion is the pinned version of the summarize_process template. BUMP IT
// whenever the template text changes so the feedback delta of the OLD prompt stays attributable
// to the OLD version.
const summarizeProcessVersion = "process_summary/v1"

// draftMinutaVersion is the pinned version of the draft_minuta template. BUMP IT whenever the
// template text changes so the feedback delta of the OLD prompt stays attributable to the OLD version.
const draftMinutaVersion = "draft_minuta/v1"

// TemplateComposer is the deterministic v0 composer: templates + context injection, no LLM. It is
// stateless.
type TemplateComposer struct{}

// NewTemplateComposer returns the deterministic composer.
func NewTemplateComposer() *TemplateComposer { return &TemplateComposer{} }

var _ PromptComposer = (*TemplateComposer)(nil)

// Compose builds the (system, user, version) for the named agent from the case context. An
// unknown agent is a programmer error surfaced as a typed invalid (never a silent empty prompt).
func (*TemplateComposer) Compose(agent string, c CaseContext) (Composed, error) {
	switch agent {
	case AgentSuggestTasks:
		return composeSuggestTasks(c), nil
	default:
		return Composed{}, apperr.NewInvalid("advisory: unknown prompt agent " + agent)
	}
}

// ComposeProcess builds the (system, user, version) for the summarize_process agent from the
// process context. An unknown agent is a programmer error surfaced as a typed invalid.
func (*TemplateComposer) ComposeProcess(agent string, c ProcessContext) (Composed, error) {
	switch agent {
	case AgentSummarizeProcess:
		return composeSummarizeProcess(c), nil
	default:
		return Composed{}, apperr.NewInvalid("advisory: unknown prompt agent " + agent)
	}
}

// ComposeDraft builds the (system, user, version) for the draft_minuta agent from the draft
// context. An unknown agent is a programmer error surfaced as a typed invalid.
func (*TemplateComposer) ComposeDraft(agent string, c DraftContext) (Composed, error) {
	switch agent {
	case AgentDraftMinuta:
		return composeDraftMinuta(c), nil
	default:
		return Composed{}, apperr.NewInvalid("advisory: unknown prompt agent " + agent)
	}
}

// composeDraftMinuta renders the draft_minuta instruction-set. The system message describes
// the legal writing role; the user message carries the intimation context + RAG chunks (when
// available). The output schema (draft_content + suggestions array) lives with the caller (use
// case), so the prompt stays about WHAT to produce, not the JSON shape.
func composeDraftMinuta(c DraftContext) Composed {
	var sys strings.Builder
	sys.WriteString(
		"Você é um assistente jurídico brasileiro especializado em redação de peças processuais. " +
			"A partir do contexto da intimação e dos autos (quando disponível), gere:\n" +
			"1. `draft_content`: o texto completo da minuta da peça processual em formato jurídico " +
			"brasileiro, pronto para revisão do advogado.\n" +
			"2. `suggestions`: lista de sugestões de melhoria mapeadas a trechos EXATOS do draft_content. " +
			"Cada sugestão tem: `category` (um de: Clareza, Argumento, Coerência, Estilo), `original` " +
			"(trecho EXATO do draft_content), `replacement` (versão melhorada), `problem` (descrição " +
			"curta do problema), `description` (explicação acionável). Para categorias Argumento e " +
			"Coerência, inclua obrigatoriamente `citation` com `document_id`, `page` e `quote` " +
			"extraídos dos trechos dos autos fornecidos. Para Clareza e Estilo, `citation` é opcional.\n\n" +
			"REGRAS OBRIGATÓRIAS:\n" +
			"- `original` DEVE ser um trecho que aparece literalmente no `draft_content`.\n" +
			"- Máximo 10 sugestões após filtragem.\n" +
			"- NÃO invente fatos; fundamente-se nos autos quando possível.\n" +
			"- Se não houver trechos dos autos, gere a minuta sem citações (modo degradado).",
	)
	if pb := strings.TrimSpace(c.Playbook); pb != "" {
		sys.WriteString("\n\nSiga o playbook do escritório:\n")
		sys.WriteString(pb)
	}

	lines := make([]string, 0, 10)
	add := func(label, value string) {
		if v := strings.TrimSpace(value); v != "" {
			lines = append(lines, label+": "+v)
		}
	}
	add("Tipo de peça", c.PieceType)
	add("Tipo de intimação", c.IntimationType)
	add("Tribunal", c.Court)
	add("Grau", c.Degree)
	add("Classe/rito", c.Class)
	add("Assunto", c.Subject)
	add("Teor da intimação", c.IntimationText)

	if len(c.Chunks) > 0 {
		lines = append(lines, "Trechos dos autos (RAG):")
		for i, chunk := range c.Chunks {
			if i >= 8 {
				break
			}
			lines = append(lines, strconv.Itoa(i+1)+". "+chunk)
		}
	}

	var usr strings.Builder
	usr.WriteString("Contexto do caso:\n")
	if len(lines) == 0 {
		usr.WriteString("(sem contexto adicional)")
	} else {
		usr.WriteString(strings.Join(lines, "\n"))
	}
	usr.WriteString("\n\nGere a minuta e as sugestões de melhoria.")

	return Composed{System: sys.String(), User: usr.String(), PromptVersion: draftMinutaVersion}
}

// composeSuggestTasks renders the suggest_tasks instruction-set. The system message is the stable
// role/task (the validated prompt), with the playbook injected when present; the user message
// carries the case context, built line by line so empty fields drop out. The model returns the
// tasks via structured output (the schema lives with the caller), so the prompt stays about WHAT
// to produce, not the JSON shape.
func composeSuggestTasks(c CaseContext) Composed {
	var sys strings.Builder
	sys.WriteString(
		"Você é um assistente jurídico brasileiro. A partir de uma intimação e do prazo dela " +
			"derivado, produza NA MESMA resposta três saídas: (1) um `summary` — \"O que aconteceu\": " +
			"resumo objetivo do que a intimação comunica; (2) uma `recommendation` — \"O que fazer\": os " +
			"próximos passos recomendados ao advogado, em texto corrido; (3) as tarefas OBJETIVAS e " +
			"ACIONÁVEIS que o advogado deve executar para cumprir o prazo. Cada tarefa tem um `title` " +
			"curto e imperativo, um `kind` (categoria curta, um destes valores exatos: ANALISE, PECA, " +
			"PROTOCOLO, PROVIDENCIA, CIENCIA) e uma `description` curta e acionável. Não invente fatos " +
			"que não estejam no contexto; prefira tarefas genéricas a suposições. Não repita tarefas. " +
			"Se faltar contexto, mantenha `summary` e `recommendation` como string vazia em vez de supor.",
	)
	if pb := strings.TrimSpace(c.Playbook); pb != "" {
		sys.WriteString("\n\nSiga o playbook do escritório (exemplos e preferências):\n")
		sys.WriteString(pb)
	}

	lines := make([]string, 0, 8)
	add := func(label, value string) {
		if v := strings.TrimSpace(value); v != "" {
			lines = append(lines, label+": "+v)
		}
	}
	add("Tribunal", c.Court)
	add("Grau", c.Degree)
	add("Classe/rito", c.Class)
	add("Assunto", c.Subject)
	add("Fase", c.Phase)
	add("Tipo de intimação", c.IntimationType)
	add("Tipo de prazo", c.PrazoKind)
	if c.PrazoDays > 0 {
		unit := "dias corridos"
		if c.Counting == "BUSINESS" {
			unit = "dias úteis"
		}
		lines = append(lines, "Prazo: "+strconv.Itoa(c.PrazoDays)+" "+unit)
	}
	add("Teor da intimação", c.IntimationText)

	var usr strings.Builder
	usr.WriteString("Contexto do caso:\n")
	if len(lines) == 0 {
		usr.WriteString("(sem contexto adicional)")
	} else {
		usr.WriteString(strings.Join(lines, "\n"))
	}
	usr.WriteString("\n\nProduza o resumo, a recomendação e as tarefas sugeridas.")

	return Composed{System: sys.String(), User: usr.String(), PromptVersion: suggestTasksVersion}
}

// composeSummarizeProcess renders the summarize_process instruction-set. The system message is
// the stable role/task (the validated prompt), with the playbook injected when present; the user
// message carries the full process context, built so empty fields/arrays drop out. The model
// returns the structured JSON via the schema (lives with the caller), so the prompt stays about
// WHAT to produce, not the JSON shape.
func composeSummarizeProcess(c ProcessContext) Composed {
	var sys strings.Builder
	sys.WriteString(
		"Você é um assistente jurídico brasileiro. A partir do contexto COMPLETO de um processo " +
			"(identificação, últimos andamentos, intimações ativas, prazos abertos e trechos de documentos " +
			"relevantes), produza NA MESMA resposta um JSON estruturado com exatamente estes campos:\n" +
			"1. `summary` (string): \"O que é este processo\" — 2–3 linhas sobre a lide, partes e estágio.\n" +
			"2. `current_status` (string): \"O que está acontecendo agora\" — situação atual (intimação recebida, " +
			"prazo correndo, processo suspenso).\n" +
			"3. `key_dates_and_deadlines` (array): prazos abertos com sinalização de urgência. Cada item tem " +
			"`kind`, `end_date`, `days_remaining`, `urgency` (um de: OVERDUE, DUE_SOON, OK), `source` (identifica " +
			"o deadline_id ou intimação).\n" +
			"4. `recent_movements` (array): últimos 2–3 andamentos significativos. Cada item tem `occurred_at`, " +
			"`text`, `source` (docket_entry_id).\n" +
			"5. `risks` (array): sinais vermelhos (prazo vencido, silêncio prolongado, mudança de competência). " +
			"Cada item tem `description`, `source`.\n" +
			"6. `recommended_actions` (array): próximos passos sugeridos (contestar, interpor recurso, ciência, " +
			"análise de documento). Cada item tem `action`, `source`.\n\n" +
			"REGRAS OBRIGATÓRIAS:\n" +
			"- NÃO invente fatos que não estejam no contexto; se faltar informação, OMITA o campo (string vazia " +
			"ou array vazio) em vez de supor.\n" +
			"- Cada risco e recomendação DEVE citar seu `source` (document_id, docket_entry_id, deadline_id ou " +
			"descrição textual).\n" +
			"- Urgência: OVERDUE se days_remaining < 0; DUE_SOON se 0 <= days_remaining <= 5 (úteis); OK caso " +
			"contrário.\n" +
			"- Omitir campos vazios do output (não enviar null).\n" +
			"- Se playbook do escritório estiver presente, siga-o como preferência de estilo/estrutura.",
	)
	if pb := strings.TrimSpace(c.Playbook); pb != "" {
		sys.WriteString("\n\nSiga o playbook do escritório (exemplos e preferências):\n")
		sys.WriteString(pb)
	}

	lines := make([]string, 0, 16)
	add := func(label, value string) {
		if v := strings.TrimSpace(value); v != "" {
			lines = append(lines, label+": "+v)
		}
	}
	add("Número CNJ", c.CNJNumber)
	add("Tribunal", c.Court)
	add("Grau", c.Degree)
	add("Classe/rito", c.Class)
	add("Assunto", c.Subject)
	add("Valor da causa", c.ClaimValue)
	add("Fase de vida", c.Lifecycle)
	add("Data de ajuizamento", c.FiledAt)

	if len(c.RecentMovements) > 0 {
		var movLines []string
		for i, m := range c.RecentMovements {
			if i >= 10 {
				break
			}
			movLines = append(movLines, m.OccurredAt+" — "+m.Text)
		}
		if len(movLines) > 0 {
			lines = append(lines, "Últimos andamentos:\n"+strings.Join(movLines, "\n"))
		}
	}

	if len(c.ActiveIntimations) > 0 {
		var intLines []string
		for _, i := range c.ActiveIntimations {
			deadlineInfo := ""
			if i.DeadlineDays > 0 {
				deadlineInfo = " (prazo: " + strconv.Itoa(i.DeadlineDays) + " dias)"
			}
			intLines = append(intLines, i.Type+": "+i.Teor+deadlineInfo)
		}
		lines = append(lines, "Intimações ativas:\n"+strings.Join(intLines, "\n"))
	}

	if len(c.OpenDeadlines) > 0 {
		var dlLines []string
		for _, d := range c.OpenDeadlines {
			dlLines = append(dlLines, d.Kind+" — vence em "+d.EndDate+" ("+strconv.Itoa(d.DaysRemaining)+" dias, "+d.Counting+")")
		}
		lines = append(lines, "Prazos abertos:\n"+strings.Join(dlLines, "\n"))
	}

	if len(c.DocumentChunks) > 0 {
		var chunkLines []string
		for i, chunk := range c.DocumentChunks {
			if i >= 5 {
				break
			}
			chunkLines = append(chunkLines, "Trecho "+strconv.Itoa(i+1)+": "+chunk)
		}
		lines = append(lines, "Trechos de documentos relevantes (RAG):\n"+strings.Join(chunkLines, "\n"))
	}

	var usr strings.Builder
	usr.WriteString("Contexto do processo:\n")
	if len(lines) == 0 {
		usr.WriteString("(sem contexto adicional)")
	} else {
		usr.WriteString(strings.Join(lines, "\n"))
	}
	usr.WriteString("\n\nProduza o resumo estruturado do processo (JSON).")

	return Composed{System: sys.String(), User: usr.String(), PromptVersion: summarizeProcessVersion}
}
