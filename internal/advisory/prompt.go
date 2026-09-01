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
	Court          string      // sigla do tribunal (TJSP, TRT2, STJ…)
	Degree         string      // G1|G2|JE|SUPERIOR…
	Class          string      // classe/rito processual
	Subject        string      // assunto
	IntimationType string      // CITACAO|INTIMACAO|COMUNICACAO…
	IntimationText string      // o teor da intimação, quando disponível — o contexto mais rico
	PrazoKind      string      // CONTESTACAO|RECURSO|MANIFESTACAO… (o kind do prazo derivado)
	PrazoDays      int         // dias do prazo
	Counting       string      // BUSINESS|CALENDAR
	Phase          string      // fase processual, quando conhecida
	DeadlineDate   string      // prazo final "2006-01-02" (analyze_intimation) — teto do due_date sugerido; "" sem prazo
	Members        []MemberCtx // membros ativos do escritório (id+nome) para o IA sugerir responsável real

	// Playbook é o ponto de injeção da "voz do escritório" (§3). v0: stub (vazio → nada é
	// injetado). Depois: exemplos de tarefas HUMANO-APROVADAS (derivados do delta suggested×
	// confirmed) entram aqui como few-shot, e o prompt "aprende" o padrão do escritório sem um
	// LLM reescrever instruções. Preencher no futuro não muda a forma do Compose.
	Playbook string
}

// MemberCtx is one firm member the analyze_intimation prompt lists as an assignable
// responsável: the internal app_user id (never org_id) + display name. The model returns one
// of these ids as suggested_assignee_user_id; the caller validates it against the same list.
type MemberCtx struct {
	UserID string
	Name   string
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
	CNJNumber string
	Court     string
	Degree    string
	Class     string
	Subject   string
	Lifecycle string // ACTIVE|SUSPENDED|ARCHIVED|SUPERSEDED
	FiledAt   string // ISO date; vazio se nil

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

// PartyCtx is one party of the process for the draft_minuta prompt.
// Role is the raw DB value (PLAINTIFF, DEFENDANT, THIRD_PARTY).
// Counsel is a short human-readable label for the first advogado, or "" when absent.
type PartyCtx struct {
	Role    string
	Name    string
	Counsel string
}

// DraftContext is the per-draft signal the draft_minuta composer specializes the
// instruction-set with. It carries the intimation that triggered the draft (the richest
// context signal), the process metadata (court/class/subject, for legal register),
// the structured parties (PLAINTIFF/DEFENDANT) for qualificação, the signing lawyer
// resolved from the matched OAB recipient, and any RAG chunks retrieved from the case
// corpus (empty when the embedder is unconfigured or the corpus has no documents — the
// degraded path).
type DraftContext struct {
	PieceType      string // DEFENSE|COMPLAINT|APPEAL|MOTION|OTHER
	IntimationType string // CITACAO|INTIMACAO|COMUNICACAO…
	IntimationText string // teor da intimação — o contexto mais rico
	Court          string
	Degree         string
	Class          string
	Subject        string
	// CNJNumber is the process number in CNJ format (e.g. 0000001-23.2026.8.26.0001).
	CNJNumber string
	// JudgingBody is the órgão julgador / vara (e.g. "3ª Vara Cível da Comarca de São Paulo").
	JudgingBody string
	// DeadlineDate is the prazo end date formatted as "2006-01-02", or empty when unknown.
	DeadlineDate string

	// Parties is the structured list of process parties loaded from the party table.
	// Empty when case_id is unavailable or when the process has no seeded parties.
	Parties []PartyCtx

	// SigningLawyerName / SigningLawyerOAB / SigningLawyerUF are resolved from the
	// first matched=true recipient in intimation.recipients. All empty when the
	// intimation has no OAB-matched recipient (blank/processo draft or DJEN parse gap).
	SigningLawyerName string
	SigningLawyerOAB  string
	SigningLawyerUF   string

	// Chunks are the RAG top-K hits (text only). Empty → grounded=false (degraded).
	Chunks   []string
	Playbook string // always empty in v0

	// ── Fatia 5 — teses/tom/instruções (Gerar-time generation params) ─────────

	// Tone is the closed-set writing register: "tecnico" (default), "objetivo",
	// or "enfatico". Empty behaves exactly like "tecnico" — the historical
	// prompt wording (backward-compat). Migração 0055 encurtou os rótulos.
	Tone string
	// Instructions is free-text advogado guidance for this generation. Empty →
	// no extra section injected.
	Instructions string
	// SelectedTheses are tese labels (plain strings) the advogado picked from
	// /theses to steer this generation. Empty → no extra section injected.
	SelectedTheses []string
}

// ReviewContext is the per-draft signal the review_minuta composer uses. It carries the
// draft's current content (the text the advogado has in the editor — the target for review),
// the RAG chunks retrieved from the case corpus (for grounded citations), and the process
// metadata (court/class/subject, for legal register). Unlike DraftContext it has Content
// (the already-written minuta) instead of IntimationText.
type ReviewContext struct {
	Content   string   // current content of the draft (non-empty — caller guards)
	PieceType string   // DEFENSE|COMPLAINT|APPEAL|MOTION|OTHER
	Court     string   // sigla do tribunal (TJSP, TRT2, STJ…)
	Degree    string   // G1|G2|JE|SUPERIOR…
	Class     string   // classe/rito processual
	Subject   string   // assunto
	Chunks    []string // RAG top-K hits (text only). Empty → grounded=false (degraded).
	Playbook  string   // always empty in v0
}

// ChatTurn is one message in the conversation history passed to ComposeChat.
type ChatTurn struct {
	Role    string // "user" | "assistant"
	Content string
}

// IterateSection is one Roman-numbered section of the peça as seen by the
// iterate composer — mirrors the draft.StructuredContent shape without
// importing the slice (advisory has no downward dependency).
type IterateSection struct {
	ID         string
	Roman      string
	Title      string
	Paragraphs []string
}

// IterateScope tells the composer whether the advogado is asking for a
// reescrita of the whole peça ("whole") or of a single section ("section",
// with SectionID matching one of the IterateSection.ID entries).
type IterateScope struct {
	Kind      string // "whole" | "section"
	SectionID string // present only when Kind == "section"
}

// IterateContext is the per-iteration signal the draft_iterate composer uses.
// It carries the CURRENT structured draft (so the LLM sees exactly what the
// advogado is looking at), the case context (for legal register + grounding),
// the RAG chunks, and the iteration params (scope + kind/instruction).
type IterateContext struct {
	PieceType   string // DEFENSE|COMPLAINT|APPEAL|MOTION|OTHER
	Court       string
	Degree      string
	Class       string
	Subject     string
	CNJNumber   string
	JudgingBody string

	// Preamble is the pre-section block of the current draft (endereçamento
	// + qualificação) — read only, the composer never asks the LLM to
	// rewrite the preamble (it's mechanical + risky).
	Preamble []string
	// Sections is the current list of Roman-numbered sections — the LLM
	// picks which to rewrite based on Scope + Kind/Instruction.
	Sections []IterateSection

	// Scope tells the LLM whether to rewrite one section or all of them.
	Scope IterateScope
	// Kind is the "quick adjust" hint (empty when the advogado wrote a free
	// instruction). Closed set — mirrors QuickAdjustKind in the FE.
	Kind string
	// Instruction is the advogado's free-text ask, empty when a Kind was
	// clicked instead.
	Instruction string

	// Parties + Chunks for grounding (same shape as DraftContext).
	Parties []PartyCtx
	Chunks  []string

	Playbook string
}

// ChatContext is the per-question signal the chat_grounding composer uses. It carries the
// draft's current content (so the assistant can refer to the peça), the RAG chunks retrieved
// for the question (empty → ungrounded path), the conversation history (up to 50 turns), and
// the current question being answered.
type ChatContext struct {
	DraftContent string     // current content of the draft/peça (may be empty before first save)
	Chunks       []string   // RAG top-K hits (text only). Empty → grounded=false (degraded).
	History      []ChatTurn // last N turns of the conversation (oldest first)
	Question     string     // the user's current question
	Playbook     string     // always empty in v0
}

// PromptComposer composes the instruction-set for a named advisory agent from a case context.
// It is a first-class, versioned artifact (§3): the agent name + version identify exactly which
// template ran. Behind an interface so the deterministic v0 (TemplateComposer) can later be
// swapped for / wrapped by an LLM-refined composer (§8) without touching the callers.
type PromptComposer interface {
	Compose(agent string, c CaseContext) (Composed, error)
	ComposeProcess(agent string, c ProcessContext) (Composed, error)
	ComposeDraft(agent string, c DraftContext) (Composed, error)
	ComposeChat(agent string, c ChatContext) (Composed, error)
	ComposeReview(agent string, c ReviewContext) (Composed, error)
	// ComposeTheses builds the (system, user, version) for the suggest_theses agent
	// (POST /v1/pecas/:id/theses — stateless, read+LLM only). Reuses DraftContext
	// (same case signal draft_minuta uses) since the theses suggestion needs the
	// same grounding.
	ComposeTheses(agent string, c DraftContext) (Composed, error)
	// ComposeIterate builds the (system, user, version) for the draft_iterate
	// agent (POST /v1/pecas/:id/iterate — Peça v2, synchronous). Takes the
	// current structured draft + iteration params and instructs the LLM to
	// return 1..N SectionChange objects with category + explanation +
	// new_paragraphs.
	ComposeIterate(agent string, c IterateContext) (Composed, error)
}

// Agent identifiers — one per specialized advisory task (erd-ai-advisory.md §4). Each maps to a
// template + a version below. Kept as consts so a caller never passes a free string.
const (
	AgentSuggestTasks      = "suggest_tasks"
	AgentAnalyzeIntimation = "analyze_intimation"
	AgentSummarizeProcess  = "summarize_process"
	AgentDraftMinuta       = "draft_minuta"
	AgentChatGrounding     = "chat_grounding"
	AgentReviewMinuta      = "review_minuta"
	AgentSuggestTheses     = "suggest_theses"
	// AgentDraftIterate is the iteration/rewrite agent (Peça v2 — POST
	// /v1/pecas/:id/iterate). Given the current structured draft + an
	// escopo + kind/instruction, returns 1..N SectionChange objects with
	// category + explanation + new_paragraphs. Synchronous LLM call.
	AgentDraftIterate = "draft_iterate"
)

// suggestTasksVersion is the pinned version of the suggest_tasks template. BUMP IT whenever the
// template text changes so the feedback delta of the OLD prompt stays attributable to the OLD
// version (never silently mixed with a new one). This is the axis the A/B improvement turns on.
const suggestTasksVersion = "suggest_tasks/v2"

// analyzeIntimationVersion is the pinned version of the analyze_intimation template (the
// "Analisar esta intimação" card: "O que aconteceu" + "Providências derivadas"). BUMP IT
// whenever the template text changes so the feedback delta stays attributable to the version.
const analyzeIntimationVersion = "analyze_intimation/v2"

// summarizeProcessVersion is the pinned version of the summarize_process template. BUMP IT
// whenever the template text changes so the feedback delta of the OLD prompt stays attributable
// to the OLD version.
const summarizeProcessVersion = "process_summary/v1"

// draftMinutaVersion is the pinned version of the draft_minuta template. BUMP IT whenever the
// template text changes so the feedback delta of the OLD prompt stays attributable to the OLD version.
// Bumped to v4: injeta as PARTES estruturadas (party table) e o ADVOGADO SIGNATÁRIO (matched OAB
// recipient) no prompt, eliminando os placeholders [Nome do Advogado]/OAB nº [número] e os nomes
// de parte adivinhados do teor. Quando fornecidos, devem ser usados diretamente; marcadores só
// quando genuinamente ausentes.
// Bumped to v5: injeta TOM (diretiva de registro/tom quando != tecnico — o default produz
// wording IDÊNTICA ao v4, backward-compat), INSTRUÇÕES (texto livre do advogado) e TESES
// SELECIONADAS (rótulos escolhidos em /theses) no prompt.
// Bumped to v6: removeu o ADVOGADO SIGNATÁRIO do prompt e do fecho. O bloco de assinatura
// (nome + OAB do titular do CERTIFICADO) é adicionado no PDF pelo pdfgen no momento do Sign
// — não pela IA. Isso desacopla a intimação (que traz o advogado do recipient) da assinatura
// (que é do cert usado). O fecho agora termina em "Local, [data]." e para.
// Bumped to v7: output agora é `draft_html` (HTML rico do Tiptap) em vez de `draft_content`
// (texto puro). Formatação inline (<strong>, <em>, <blockquote>, <table>, <ol>) sai direto
// da IA e chega intacta ao editor e ao PDF final (chromedp). Elimina o passo de conversão
// structured_content → HTML no FE e prepara o pipeline pra streaming (cada chunk cai
// diretamente no editor sem parser incremental).
// Bumped to v8: output agora é `draft_markdown` em vez de `draft_html`. Motivo: streaming
// char-a-char do LLM corta tags HTML no meio (`<h2>` chega como `<`, `h`, `2>`) e o
// ProseMirror escapa fragmentos incompletos como texto (`&lt;`). Markdown é robusto ao
// streaming (não tem tags pareadas pra corromper) e é o padrão da indústria (ChatGPT
// Canvas, Claude Artifacts, Cursor). O BE converte markdown → HTML via goldmark antes
// de persistir em content_html; o FE usa tiptap-markdown pra renderizar incrementalmente
// no editor via streamContent().
const draftMinutaVersion = "draft_minuta/v9"

// suggestThesesVersion is the pinned version of the suggest_theses template (POST
// /v1/pecas/:id/theses — stateless read+LLM). BUMP IT whenever the template text changes.
const suggestThesesVersion = "suggest_theses/v2"

// chatGroundingVersion is the pinned version of the chat_grounding template. BUMP IT whenever the
// template text changes so the feedback delta of the OLD prompt stays attributable to the OLD version.
const chatGroundingVersion = "chat_grounding/v1"

// reviewMinutaVersion is the pinned version of the review_minuta template. BUMP IT whenever the
// template text changes so the feedback delta of the OLD prompt stays attributable to the OLD version.
const reviewMinutaVersion = "review_minuta/v1"

// draftIterateVersion is the pinned version of the draft_iterate template
// (POST /v1/pecas/:id/iterate — Peça v2). BUMP IT whenever the template text
// changes so the feedback delta of the OLD prompt stays attributable.
const draftIterateVersion = "draft_iterate/v1"

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
	case AgentAnalyzeIntimation:
		return composeAnalyzeIntimation(c), nil
	default:
		return Composed{}, apperr.NewInvalid("advisory: unknown prompt agent " + agent)
	}
}

// composeAnalyzeIntimation renders the analyze_intimation instruction-set (the detail
// card "Analisar esta intimação"). It reads ONE intimation's teor + its court context and
// produces two outputs: `summary` — "O que aconteceu", a plain-language pt-BR legal
// account of what the publication communicates — and `providencias` — the concrete steps
// the lawyer must take, each a short `title` + an actionable `description`. The output
// schema (summary + providencias[]) lives with the caller (AnaliseUseCase). The prompt
// stays about WHAT to produce, not the JSON shape.
func composeAnalyzeIntimation(c CaseContext) Composed {
	var sys strings.Builder
	sys.WriteString(
		"Você é um assistente jurídico brasileiro. A partir do TEOR de uma intimação/publicação " +
			"e do contexto do processo, produza NA MESMA resposta um JSON com exatamente três campos:\n" +
			"1. `summary` (string): \"O que aconteceu\" — um resumo objetivo, em português jurídico " +
			"conciso (2–4 frases), do que a publicação comunica (a decisão, o despacho, a intimação e seu " +
			"efeito prático para a parte).\n" +
			"2. `ato` (string): o ATO PRINCIPAL que a intimação exige da parte, em 1–3 palavras, no " +
			"substantivo (ex.: \"Contestação\", \"Manifestação\", \"Recurso de apelação\", \"Cumprimento " +
			"de sentença\", \"Ciência\"). É o rótulo curto que titula a intimação — NÃO uma frase.\n" +
			"3. `providencias` (array): as PROVIDÊNCIAS a cumprir em razão da intimação. Cada item tem:\n" +
			"   - `title` curto e imperativo, JÁ COM a citação legal quando cabível (ex.: \"Redigir defesa " +
			"(art. 919, CPC)\", \"Protocolar contestação (art. 335, CPC)\");\n" +
			"   - `description` curta e acionável explicando a providência;\n" +
			"   - `kind`: a natureza da providência — \"PECA\" quando exige redigir/protocolar uma peça " +
			"processual (contestação, recurso, manifestação escrita, embargos); \"CIENCIA\" quando é " +
			"comunicação/ciência ou providência de apoio de fluxo curto (dar ciência ao cliente, juntar " +
			"documentos, anotar prazo). Use EXATAMENTE um desses dois valores;\n" +
			"   - `suggested_assignee_user_id`: o id de UM membro do escritório (da lista \"Membros do " +
			"escritório\" fornecida no contexto) a quem esta providência deve ser atribuída — use " +
			"EXATAMENTE um dos ids listados, ou null se nenhum couber ou se a lista estiver vazia. NUNCA " +
			"invente um id.\n" +
			"   - `due_date`: a data-limite para cumprir a providência no formato \"AAAA-MM-DD\". Quando " +
			"houver \"Prazo final\" no contexto, o due_date DEVE ser ANTERIOR OU IGUAL a ele (reserve dias " +
			"úteis de folga para revisão/protocolo); use null quando não houver base para uma data.\n\n" +
			"REGRAS OBRIGATÓRIAS:\n" +
			"- NÃO invente fatos que não estejam no teor ou no contexto; prefira providências genéricas a " +
			"suposições.\n" +
			"- Se o teor for insuficiente para entender o que aconteceu, mantenha `summary` como string " +
			"vazia e `providencias` como array vazio, em vez de supor.\n" +
			"- Não repita providências; mantenha o tom jurídico formal brasileiro.",
	)
	if pb := strings.TrimSpace(c.Playbook); pb != "" {
		sys.WriteString("\n\nSiga o playbook do escritório (exemplos e preferências):\n")
		sys.WriteString(pb)
	}

	lines := make([]string, 0, 6)
	add := func(label, value string) {
		if v := strings.TrimSpace(value); v != "" {
			lines = append(lines, label+": "+v)
		}
	}
	add("Tribunal", c.Court)
	add("Grau", c.Degree)
	add("Classe/rito", c.Class)
	add("Assunto", c.Subject)
	add("Tipo de intimação", c.IntimationType)
	add("Prazo final", c.DeadlineDate)
	add("Teor da intimação", c.IntimationText)

	// Inject the firm's members so the model can pick a real suggested_assignee_user_id.
	if len(c.Members) > 0 {
		var memberLines []string
		for _, m := range c.Members {
			memberLines = append(memberLines, "- id="+m.UserID+" nome="+m.Name)
		}
		lines = append(lines, "Membros do escritório (escolha o suggested_assignee_user_id entre estes ids):\n"+strings.Join(memberLines, "\n"))
	}

	var usr strings.Builder
	usr.WriteString("Contexto:\n")
	if len(lines) == 0 {
		usr.WriteString("(sem contexto adicional)")
	} else {
		usr.WriteString(strings.Join(lines, "\n"))
	}
	usr.WriteString("\n\nProduza o resumo (\"O que aconteceu\"), o ato principal e as providências derivadas.")

	return Composed{System: sys.String(), User: usr.String(), PromptVersion: analyzeIntimationVersion}
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

// ComposeChat builds the (system, user, version) for the chat_grounding agent from the chat
// context. An unknown agent is a programmer error surfaced as a typed invalid.
func (*TemplateComposer) ComposeChat(agent string, c ChatContext) (Composed, error) {
	switch agent {
	case AgentChatGrounding:
		return composeChatGrounding(c), nil
	default:
		return Composed{}, apperr.NewInvalid("advisory: unknown prompt agent " + agent)
	}
}

// ComposeReview builds the (system, user, version) for the review_minuta agent from the review
// context. An unknown agent is a programmer error surfaced as a typed invalid.
func (*TemplateComposer) ComposeReview(agent string, c ReviewContext) (Composed, error) {
	switch agent {
	case AgentReviewMinuta:
		return composeReviewMinuta(c), nil
	default:
		return Composed{}, apperr.NewInvalid("advisory: unknown prompt agent " + agent)
	}
}

// ComposeTheses builds the (system, user, version) for the suggest_theses agent from the draft
// context. An unknown agent is a programmer error surfaced as a typed invalid.
func (*TemplateComposer) ComposeTheses(agent string, c DraftContext) (Composed, error) {
	switch agent {
	case AgentSuggestTheses:
		return composeSuggestTheses(c), nil
	default:
		return Composed{}, apperr.NewInvalid("advisory: unknown prompt agent " + agent)
	}
}

// ComposeIterate builds the (system, user, version) for the draft_iterate agent
// from the iterate context. An unknown agent is a programmer error surfaced
// as a typed invalid. (Peça v2 — synchronous LLM iteration.)
func (*TemplateComposer) ComposeIterate(agent string, c IterateContext) (Composed, error) {
	switch agent {
	case AgentDraftIterate:
		return composeDraftIterate(c), nil
	default:
		return Composed{}, apperr.NewInvalid("advisory: unknown prompt agent " + agent)
	}
}

// composeDraftMinuta renders the draft_minuta instruction-set (v4). The system message describes
// the legal writing role with the canonical structure and the gold rule (use real data, no
// placeholders for provided fields, including structured parties and signing-lawyer OAB). The user
// message carries the fully injected process context — nº do processo, vara/órgão julgador,
// tribunal, classe, assunto, prazo, teor da intimação, partes estruturadas, advogado signatário e
// trechos RAG — so the LLM has everything it needs to produce a complete, non-generic minuta.
// Gerar produces ONLY draft_content — suggestions are a separate Revisar step (review_minuta agent).
// The output schema lives with the caller (use case).
func composeDraftMinuta(c DraftContext) Composed {
	var sys strings.Builder
	sys.WriteString(
		"Você é um assistente jurídico brasileiro que redige a MINUTA COMPLETA de uma peça processual, " +
			"pronta para revisão do advogado no editor rico. Produza SOMENTE o campo `draft_markdown`: " +
			"o texto da minuta em MARKDOWN (CommonMark + GFM tables), sem front-matter e sem code fences " +
			"envolvendo o output.\n\n" +

			"REGRA DE OURO (a mais importante): USE OS DADOS REAIS fornecidos no contexto. NUNCA deixe " +
			"placeholder (___, [assim], \"NOME DA PARTE\") para um dado que foi fornecido. Só use " +
			"marcador [entre colchetes] quando o dado for genuinamente DESCONHECIDO (não fornecido no " +
			"contexto). Se o tribunal, a vara (órgão julgador), o número do processo, a classe e o " +
			"assunto foram dados, escreva-os.\n" +
			"As PARTES do processo (autor/réu) são fornecidas ESTRUTURADAS no " +
			"contexto — USE-AS na qualificação/endereçamento conforme o papel " +
			"(PLAINTIFF = autor/exequente/requerente; DEFENDANT = réu/executado/requerido). " +
			"Prefira SEMPRE a parte estruturada ao que você extrairia do teor da intimação.\n\n" +

			"ESTRUTURA CANÔNICA (nesta ordem, blocos separados por linha em branco):\n" +
			"1) ENDEREÇAMENTO em CAIXA ALTA, adaptado ao foro: Vara Cível comum → " +
			"\"EXCELENTÍSSIMO SENHOR DOUTOR JUIZ DE DIREITO DA [vara] DA COMARCA DE [comarca]/[UF]\"; " +
			"Juizado Especial (degree=JE ou órgão contém \"JUIZADO ESPECIAL\") → " +
			"\"EXCELENTÍSSIMO SENHOR JUIZ DE DIREITO DO JUIZADO ESPECIAL CÍVEL DA COMARCA DE [comarca]/[UF]\" " +
			"(tom simples, SEM \"Doutor\"); 2ª instância → Tribunal/Desembargador. " +
			"Derive a comarca do órgão julgador quando possível.\n" +
			"2) Referência: \"Processo nº [CNJ]\" + classe/assunto.\n" +
			"3) Identificação/qualificação: em autos em curso basta \"[PARTE], já qualificado(a) nos autos " +
			"em epígrafe, por seu advogado infra-assinado,\"; em petição inicial, qualificar as partes.\n" +
			"4) EXÓRDIO com base legal: \"vem, respeitosamente, à presença de Vossa Excelência, com " +
			"fundamento no [artigo], apresentar [TIPO DA PEÇA EM CAIXA ALTA]\" pelos fatos e fundamentos " +
			"a seguir expostos.\n" +
			"5) \"I – DOS FATOS\" (CAIXA ALTA), parágrafos em arábico, narrativa objetiva ancorada no " +
			"TEOR DA INTIMAÇÃO e nos trechos dos autos fornecidos.\n" +
			"6) \"II – DO DIREITO\" (CAIXA ALTA), fundamentação com artigos de lei no formato " +
			"\"art. XXX, inciso, da Lei nº .../CPC\"; em defesa, impugnação especificada (art. 341 CPC) " +
			"e todas as teses (art. 336); adapte ao tipo de peça.\n" +
			"7) \"III – DOS PEDIDOS\" (CAIXA ALTA): \"Ante o exposto, requer a Vossa Excelência:\" + " +
			"alíneas a), b), c) (pedido certo e determinado, art. 322/324 CPC; inclua produção de provas " +
			"e sucumbência quando cabível).\n" +
			"8) FECHO: escreva EXATAMENTE \"Nestes termos,\\nPede deferimento.\" e PARE AÍ. NÃO " +
			"escreva NADA depois disso. Especificamente PROIBIDO: cidade, UF, comarca, data, dia/mês/" +
			"ano por extenso, marcador [data], marcador [local], nome do advogado, OAB, assinatura, " +
			"linha em branco extra para assinatura, traços/underscores simulando linha de assinatura. " +
			"O bloco COMPLETO de fechamento (cidade do foro, data por extenso e nome+OAB do titular " +
			"do certificado) é adicionado AUTOMATICAMENTE pelo sistema no momento da geração do PDF, " +
			"a partir do court_record e do certificado digital. Qualquer coisa que você escreva após " +
			"\"Pede deferimento.\" vai aparecer duplicado no PDF final.\n\n" +

			"REQUISITOS LEGAIS POR TIPO DE PEÇA:\n" +
			"- PETIÇÃO INICIAL (COMPLAINT): art. 319 CPC (juízo, partes, causa de pedir, pedido, valor " +
			"da causa, provas).\n" +
			"- CONTESTAÇÃO/DEFESA (DEFENSE): arts. 336 (eventualidade) e 341 (impugnação especificada); " +
			"preliminares antes do mérito (art. 337). Em execução de título extrajudicial no JE (ex.: nota " +
			"promissória), a defesa pode ser EMBARGOS À EXECUÇÃO (art. 917 CPC — excesso de execução " +
			"§2º com valor correto + cálculo; e prescrição: nota promissória = 3 anos, art. 70 da " +
			"LUG/Dec. 57.663/66).\n" +
			"- RECURSO (APPEAL): síntese do decidido + razões de reforma + pedido de provimento. No JE, " +
			"recurso inominado (art. 41 Lei 9.099/95).\n\n" +

			"FORMATAÇÃO MARKDOWN (CommonMark + GFM tables — nada de HTML, nada de code fences):\n" +
			"- Endereçamento (linha inteira em negrito): `**EXCELENTÍSSIMO SENHOR DOUTOR JUIZ...**`\n" +
			"- Referência do processo: `Processo nº [CNJ] — [Classe] ([Assunto])` (parágrafo simples)\n" +
			"- Qualificação/exórdio: parágrafos simples separados por linha em branco\n" +
			"- Cabeçalho de seção: `## I — DOS FATOS`  ·  `## II — DO DIREITO`  ·  `## III — DOS PEDIDOS`\n" +
			"- Parágrafos numerados dos fatos/direito: escreva o número no início do texto, sem lista: " +
			"`1. Trata-se de ...` em parágrafo próprio; próxima linha em branco; `2. ...`\n" +
			"- Pedidos com alíneas: lista ordenada markdown `1. ...\\n2. ...` (o renderer converte para a), b), c) — " +
			"não use asteriscos nem letras manualmente)\n" +
			"- Ênfase pontual (nome da peça, valores, nº do processo, artigos citados): `**texto**`\n" +
			"- Termos técnicos em latim/estrangeiro: `*texto*`\n" +
			"- Citações longas (>3 linhas de lei/jurisprudência): `> ...` (blockquote, uma linha por linha citada)\n" +
			"- Tabelas de cálculo (impugnação de valor): tabela GFM `| Coluna | Valor |\\n|---|---|\\n| ... | ... |`\n" +
			"- Fecho: EXATAMENTE `Nestes termos,\\nPede deferimento.` em um parágrafo e PARE. Sem " +
			"nova linha, sem cidade/data/nome/OAB, sem espaços em branco para assinatura. O PDF " +
			"final injeta o bloco de assinatura completo (cidade do foro + data + nome + OAB do " +
			"certificado) automaticamente — sua saída termina em \"deferimento.\".\n" +
			"NUNCA escreva tags HTML (`<p>`, `<strong>`, `<h2>`, etc). NUNCA envolva o output em ```markdown``` " +
			"ou qualquer code fence. Devolva apenas o markdown puro. Se faltar dado essencial, escreva a melhor " +
			"minuta possível com marcador entre colchetes no que faltar — NUNCA invente fatos, valores, súmulas, " +
			"datas ou nºs de processo que não constem do contexto.",
	)
	if pb := strings.TrimSpace(c.Playbook); pb != "" {
		sys.WriteString("\n\nSiga o playbook do escritório:\n")
		sys.WriteString(pb)
	}
	// Tone directive (Fatia 5): empty or "tecnico" adds NOTHING — the base
	// system prompt above already IS the técnico register, so the wording
	// stays byte-identical to v4 for the default/omitted case (backward-compat).
	if td := toneDirective(c.Tone); td != "" {
		sys.WriteString("\n\n")
		sys.WriteString(td)
	}

	lines := make([]string, 0, 16)
	add := func(label, value string) {
		if v := strings.TrimSpace(value); v != "" {
			lines = append(lines, label+": "+v)
		}
	}
	add("Tipo da peça", c.PieceType)
	add("Tribunal", c.Court)
	add("Órgão julgador/Vara", c.JudgingBody)
	add("Nº do processo", c.CNJNumber)
	add("Classe/rito", c.Class)
	add("Assunto", c.Subject)
	add("Grau", c.Degree)
	add("Tipo de intimação", c.IntimationType)
	add("Prazo final", c.DeadlineDate)

	// Inject structured parties (PLAINTIFF/DEFENDANT/THIRD_PARTY).
	for _, p := range c.Parties {
		label := roleLabel(p.Role)
		value := p.Name
		if p.Counsel != "" {
			value += " (adv. " + p.Counsel + ")"
		}
		add(label, value)
	}

	// Advogado signatário: não injetado no prompt. O bloco de assinatura vem
	// do TITULAR DO CERTIFICADO usado na hora do Sign (pdfgen.Signer), não do
	// recipient da intimação — o cert pode pertencer a um sócio diferente do
	// que foi intimado. Campos SigningLawyer* na struct mantidos para compat
	// com testes/callers antigos; ficam ignorados aqui.

	add("TEOR DA INTIMAÇÃO", c.IntimationText)

	if len(c.Chunks) > 0 {
		lines = append(lines, "Trechos relevantes dos autos:")
		for i, chunk := range c.Chunks {
			if i >= 8 {
				break
			}
			lines = append(lines, strconv.Itoa(i+1)+". "+chunk)
		}
	}

	// Instructions (Fatia 5): free-text advogado guidance for this generation.
	// Omitted entirely when empty (no dangling label).
	if instr := strings.TrimSpace(c.Instructions); instr != "" {
		lines = append(lines, "Instruções específicas do advogado para esta minuta:\n"+instr)
	}

	// Selected theses (Fatia 5): tese labels picked from /theses to steer this
	// generation. Omitted entirely when empty.
	if len(c.SelectedTheses) > 0 {
		lines = append(lines, "Teses selecionadas pelo advogado (desenvolva estas no \"II – DO DIREITO\"):\n- "+
			strings.Join(c.SelectedTheses, "\n- "))
	}

	var usr strings.Builder
	if len(lines) == 0 {
		usr.WriteString("(sem contexto adicional — modo degradado)")
	} else {
		usr.WriteString(strings.Join(lines, "\n"))
	}
	usr.WriteString("\n\nRedija a minuta completa da peça seguindo as instruções.")

	return Composed{System: sys.String(), User: usr.String(), PromptVersion: draftMinutaVersion}
}

// toneDirective returns the tone-specific system directive to append for the
// draft_minuta prompt (Fatia 5). "" and "tecnico" both return "" — the base
// system prompt above IS already the técnico register, so omitting the
// directive for the default/empty case keeps the prompt byte-identical to the
// pre-Fatia-5 (v4) wording, the backward-compat contract. The literal string
// values mirror the closed set in internal/draft/entity.go (Tone*) without
// importing that package (advisory has no downward dependency on any slice).
func toneDirective(tone string) string {
	switch tone {
	case "objetivo":
		return "TOM: adote um registro OBJETIVO — frases curtas e diretas, " +
			"argumentação incisiva, sem rodeios ou hedging (evite \"salvo melhor juízo\", " +
			"\"data venia\" em excesso); mantenha o rigor técnico-jurídico."
	case "enfatico":
		return "TOM: adote um registro ENFÁTICO — vigoroso e assertivo, sublinhando " +
			"a força dos argumentos e a gravidade das consequências, sem abrir mão do " +
			"rigor técnico nem escorregar em adjetivação vazia."
	default:
		return ""
	}
}

// composeSuggestTheses renders the suggest_theses instruction-set (POST
// /v1/pecas/:id/theses — stateless, read+LLM only, no writer). The system message
// instructs the model to SUGGEST candidate legal theses (not draft the minuta), each
// mapped to a plain-text reference (jurisprudência or dispositivo legal) — unlike a
// review Finding, a thesis reference is NOT a chunk anchored in the case corpus, so
// it carries no document_id/page/quote. The output schema (theses array) lives with
// the caller (ThesesUseCase in theses.go). Reuses the same context-injection lines as
// composeDraftMinuta (same case signal), minus the tone/instructions/theses fields
// (those steer Gerar, not the thesis suggestion itself).
func composeSuggestTheses(c DraftContext) Composed {
	var sys strings.Builder
	sys.WriteString(
		"Você é um assistente jurídico brasileiro especializado em teses de defesa/ataque. " +
			"A partir do contexto do caso (teor da intimação, partes, trechos dos autos), sugira " +
			"TESES JURÍDICAS candidatas que o advogado pode desenvolver na peça. Cada tese tem:\n" +
			"- `label`: nome curto da tese (ex.: \"Prescrição intercorrente\").\n" +
			"- `confidence`: um de alta|media|baixa — CLASSIFIQUE COM RIGOR conforme os critérios abaixo.\n" +
			"- `reference`: jurisprudência ou dispositivo legal, texto livre (ex.: \"art. 206, §5º, I, CC\" ou \"STJ, REsp 1.234.567/SP\"). NÃO invente números de processo.\n" +
			"- `foundation`: explicação CURTA (1-2 frases) de por que a tese se aplica a este caso.\n" +
			"- `evidence`: ARRAY de trechos LITERAIS extraídos do TEOR DA INTIMAÇÃO ou dos Trechos relevantes dos autos que sustentam a tese. Cada item é um recorte curto (10-40 palavras), copiado sem alterar. NÃO parafraseie. NÃO invente. Se não houver trecho literal, deixe o array vazio — e nesse caso confidence DEVE ser baixa.\n\n" +
			"CRITÉRIOS DE CONFIDENCE (siga à risca):\n" +
			"- alta: ao menos 2 trechos literais do contexto (evidence.length ≥ 2) apoiam DIRETAMENTE a tese, e o dispositivo/precedente citado é claramente aplicável ao caso concreto.\n" +
			"- media: 1 trecho literal apoia (evidence.length == 1), OU 2+ trechos apoiam de forma indireta (contexto sugere mas não afirma o fato-chave).\n" +
			"- baixa: nenhum trecho literal (evidence.length == 0), OU a tese é aplicável só em tese/doutrina sem amarração ao caso. Prefira retornar tese baixa a inventar evidência.\n\n" +
			"REGRAS OBRIGATÓRIAS:\n" +
			"- NÃO invente fatos que não estejam no contexto; a tese deve ser aplicável ao caso descrito, não genérica.\n" +
			"- Todo item de `evidence` deve ser cópia literal do TEOR DA INTIMAÇÃO ou de um dos Trechos relevantes dos autos. Não parafraseie, não resuma. Copiar exato o recorte que sustenta.\n" +
			"- Máximo 8 teses, ordenadas da mais forte (alta) para a mais fraca (baixa).\n" +
			"- Se o contexto for insuficiente para qualquer tese com fundamento real, retorne uma lista vazia em vez de supor.",
	)
	if pb := strings.TrimSpace(c.Playbook); pb != "" {
		sys.WriteString("\n\nSiga o playbook do escritório:\n")
		sys.WriteString(pb)
	}

	lines := make([]string, 0, 12)
	add := func(label, value string) {
		if v := strings.TrimSpace(value); v != "" {
			lines = append(lines, label+": "+v)
		}
	}
	add("Tipo da peça", c.PieceType)
	add("Tribunal", c.Court)
	add("Órgão julgador/Vara", c.JudgingBody)
	add("Classe/rito", c.Class)
	add("Assunto", c.Subject)
	add("Grau", c.Degree)
	add("Tipo de intimação", c.IntimationType)

	for _, p := range c.Parties {
		label := roleLabel(p.Role)
		value := p.Name
		if p.Counsel != "" {
			value += " (adv. " + p.Counsel + ")"
		}
		add(label, value)
	}

	add("TEOR DA INTIMAÇÃO", c.IntimationText)

	if len(c.Chunks) > 0 {
		lines = append(lines, "Trechos relevantes dos autos:")
		for i, chunk := range c.Chunks {
			if i >= 8 {
				break
			}
			lines = append(lines, strconv.Itoa(i+1)+". "+chunk)
		}
	}

	var usr strings.Builder
	if len(lines) == 0 {
		usr.WriteString("(sem contexto adicional — modo degradado)")
	} else {
		usr.WriteString(strings.Join(lines, "\n"))
	}
	usr.WriteString("\n\nSugira as teses jurídicas aplicáveis a este caso.")

	return Composed{System: sys.String(), User: usr.String(), PromptVersion: suggestThesesVersion}
}

// roleLabel converts the DB party role to a Portuguese label for the prompt.
func roleLabel(role string) string {
	switch role {
	case "PLAINTIFF":
		return "Autor/Exequente/Requerente"
	case "DEFENDANT":
		return "Réu/Executado/Requerido"
	default:
		return "Terceiro"
	}
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

// composeChatGrounding renders the chat_grounding instruction-set. The assistant answers
// the lawyer's question using ONLY the retrieved chunks from the case corpus as context.
// When no chunks are available it still answers, but grounded=false (no citations). The
// multi-turn history is injected in the user message so the model can follow the conversation.
func composeChatGrounding(c ChatContext) Composed {
	var sys strings.Builder
	sys.WriteString(
		"Você é um assistente jurídico brasileiro especializado em análise de autos processuais. " +
			"Responda à pergunta do advogado com base EXCLUSIVAMENTE nos trechos dos autos fornecidos " +
			"como contexto. Para cada afirmação factual, cite o trecho correspondente usando " +
			"document_id, page e quote. Se não houver trechos dos autos ou se a informação " +
			"necessária não estiver nos trechos, responda honestamente que não encontrou essa " +
			"informação nos documentos disponíveis — nunca invente fatos. " +
			"Quando citar, use apenas document_ids que apareçam exatamente nos trechos fornecidos.\n\n" +
			"REGRAS OBRIGATÓRIAS:\n" +
			"- Responda SOMENTE com base nos trechos fornecidos; se não houver trechos suficientes, " +
			"diga que não encontrou a informação nos autos disponíveis.\n" +
			"- `citations` deve conter APENAS citações de trechos que aparecem literalmente no contexto.\n" +
			"- `citations` pode ser vazio [] quando não houver contexto suficiente — nunca invente document_ids.\n" +
			"- Mantenha o tom jurídico formal brasileiro.",
	)
	if pb := strings.TrimSpace(c.Playbook); pb != "" {
		sys.WriteString("\n\nSiga o playbook do escritório:\n")
		sys.WriteString(pb)
	}

	var usr strings.Builder

	// Inject the draft content for reference (may be empty before first save).
	if draft := strings.TrimSpace(c.DraftContent); draft != "" {
		usr.WriteString("Conteúdo atual da minuta:\n")
		usr.WriteString(draft)
		usr.WriteString("\n\n")
	}

	// Inject the retrieved RAG chunks.
	if len(c.Chunks) > 0 {
		usr.WriteString("Trechos dos autos disponíveis (RAG):\n")
		for i, chunk := range c.Chunks {
			if i >= 8 {
				break
			}
			usr.WriteString(strconv.Itoa(i+1) + ". " + chunk + "\n")
		}
		usr.WriteString("\n")
	} else {
		usr.WriteString("(Nenhum trecho dos autos disponível para esta pergunta.)\n\n")
	}

	// Inject the conversation history (multi-turn context).
	if len(c.History) > 0 {
		usr.WriteString("Histórico da conversa:\n")
		for _, turn := range c.History {
			role := "Advogado"
			if turn.Role == "assistant" {
				role = "Assistente"
			}
			usr.WriteString(role + ": " + turn.Content + "\n")
		}
		usr.WriteString("\n")
	}

	// The current question.
	usr.WriteString("Pergunta atual do advogado: ")
	usr.WriteString(c.Question)
	usr.WriteString("\n\nResponda à pergunta citando os trechos dos autos relevantes.")

	return Composed{System: sys.String(), User: usr.String(), PromptVersion: chatGroundingVersion}
}

// composeReviewMinuta renders the review_minuta instruction-set. The system message instructs
// the model to act as a critical reviewer of an already-written minuta, not as the author.
// The output schema (suggestions array) lives with the caller (ReviewUseCase in review.go).
// Categories and citation rules are identical to composeDraftMinuta (same closed set).
func composeReviewMinuta(c ReviewContext) Composed {
	var sys strings.Builder
	sys.WriteString(
		"Você é um revisor jurídico brasileiro especializado em peças processuais. " +
			"Revise esta minuta JÁ redigida e aponte melhorias pontuais mapeadas a trechos EXATOS do texto. " +
			"Para cada melhoria, indique: `category` (um de: Clareza, Argumento, Coerência, Estilo), " +
			"`original` (trecho EXATO que aparece literalmente na minuta), `replacement` (versão melhorada), " +
			"`problem` (descrição curta do problema), `description` (explicação acionável). " +
			"Para categorias Argumento e Coerência, inclua obrigatoriamente `citation` com `document_id`, " +
			"`page` e `quote` extraídos dos trechos dos autos fornecidos — sem trechos dos autos, omita " +
			"sugestões dessas categorias. Para Clareza e Estilo, `citation` é opcional.\n\n" +
			"REGRAS OBRIGATÓRIAS:\n" +
			"- `original` DEVE ser um trecho que aparece literalmente na minuta fornecida.\n" +
			"- Máximo 10 sugestões após filtragem.\n" +
			"- NÃO reescreva a minuta inteira — apenas aponte melhorias cirúrgicas.\n" +
			"- NÃO invente fatos ou document_ids ausentes dos trechos dos autos fornecidos.\n" +
			"- Cite os autos quando aplicável para Argumento e Coerência.",
	)
	if pb := strings.TrimSpace(c.Playbook); pb != "" {
		sys.WriteString("\n\nSiga o playbook do escritório:\n")
		sys.WriteString(pb)
	}

	lines := make([]string, 0, 8)
	add := func(label, value string) {
		if v := strings.TrimSpace(value); v != "" {
			lines = append(lines, label+": "+v)
		}
	}
	add("Tipo de peça", c.PieceType)
	add("Tribunal", c.Court)
	add("Grau", c.Degree)
	add("Classe/rito", c.Class)
	add("Assunto", c.Subject)

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
	if len(lines) > 0 {
		usr.WriteString("Contexto do caso:\n")
		usr.WriteString(strings.Join(lines, "\n"))
		usr.WriteString("\n\n")
	}
	usr.WriteString("Minuta para revisão:\n")
	usr.WriteString(c.Content)
	usr.WriteString("\n\nRevise a minuta e produza as sugestões de melhoria.")

	return Composed{System: sys.String(), User: usr.String(), PromptVersion: reviewMinutaVersion}
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

// composeDraftIterate renders the draft_iterate instruction-set (Peça v2 —
// POST /v1/pecas/:id/iterate). Given the CURRENT structured draft + an escopo
// (whole ou uma seção específica) + kind ("quick adjust") ou instruction (livre),
// instrui o LLM a devolver 1..N mudanças por seção, cada uma com categoria e
// explicação curta do porquê. O JSON schema fica no caller (IterateUseCase).
//
// Regras-chave do prompt:
//   - NUNCA reescrever o preâmbulo (endereçamento + qualificação são mecânicos).
//   - Devolver mudanças SOMENTE nas seções afetadas pelo escopo.
//   - Cada mudança traz section_id (do array fornecido), category (enum fechado),
//     explanation curta e new_paragraphs (o texto novo, em ordem).
//   - Sem placeholders inventados; se contexto insuficiente, devolver changes vazio.
func composeDraftIterate(c IterateContext) Composed {
	var sys strings.Builder
	sys.WriteString(
		"Você é um assistente jurídico brasileiro especializado em reescrita cirúrgica " +
			"de peças processuais. O advogado tem uma peça JÁ redigida e quer ajustar " +
			"trecho(s) dela — nunca redigir de novo, nunca tocar no preâmbulo (endereçamento " +
			"+ qualificação são mecânicos). Sua resposta é um JSON com o campo `changes` — " +
			"um array de mudanças, uma por SEÇÃO afetada. Cada mudança tem:\n" +
			"1. `section_id` (string): o id da seção alterada, EXATAMENTE como aparece na " +
			"lista de seções fornecida no contexto (\"fatos\", \"direito\", \"pedidos\"…).\n" +
			"2. `category` (string): a categoria da mudança, um dos valores exatos: " +
			"CLAREZA, CONCISÃO, ÊNFASE, FUNDAMENTAÇÃO, COMPLETUDE, COERÊNCIA, AJUSTE. " +
			"Use AJUSTE quando nenhuma outra couber.\n" +
			"3. `explanation` (string): 1 frase (até 140 chars) explicando POR QUÊ da " +
			"mudança, do ponto de vista do advogado (o que melhora).\n" +
			"4. `new_paragraphs` (array de strings): o novo conteúdo COMPLETO da seção, " +
			"parágrafo a parágrafo. Substitui os parágrafos atuais dessa seção.\n\n" +
			"REGRAS OBRIGATÓRIAS:\n" +
			"- Respeite o ESCOPO fornecido:\n" +
			"  · scope=whole → considere reescrever qualquer seção que possa melhorar; " +
			"NÃO se obrigue a mexer em todas (só onde a mudança agrega).\n" +
			"  · scope=section (com section_id) → devolva NO MÁXIMO 1 mudança, para essa " +
			"seção específica. Ignore outras.\n" +
			"- NÃO reescreva o preâmbulo (endereçamento + qualificação). Nunca inclua uma " +
			"mudança para \"preambulo\" / \"preâmbulo\".\n" +
			"- Se nada melhora com o pedido, devolva `changes: []` (não force reescritas).\n" +
			"- NÃO invente fatos, valores, súmulas, datas ou nºs de processo que não " +
			"constem do contexto.\n" +
			"- Preserve tom técnico-jurídico brasileiro, artigos de lei no formato " +
			"\"art. XXX, inciso, da Lei nº .../CPC\".",
	)
	if pb := strings.TrimSpace(c.Playbook); pb != "" {
		sys.WriteString("\n\nSiga o playbook do escritório:\n")
		sys.WriteString(pb)
	}

	// Kind-specific system directive (concise, emphatic, etc.) — appended
	// AFTER the base rules so the model still respects the JSON schema.
	if kd := kindDirective(c.Kind); kd != "" {
		sys.WriteString("\n\n")
		sys.WriteString(kd)
	}

	var usr strings.Builder

	// ── Case context (grounding) ─────────────────────────────────────────────
	lines := make([]string, 0, 12)
	add := func(label, value string) {
		if v := strings.TrimSpace(value); v != "" {
			lines = append(lines, label+": "+v)
		}
	}
	add("Tipo da peça", c.PieceType)
	add("Tribunal", c.Court)
	add("Órgão julgador/Vara", c.JudgingBody)
	add("Nº do processo", c.CNJNumber)
	add("Classe/rito", c.Class)
	add("Assunto", c.Subject)
	add("Grau", c.Degree)
	for _, p := range c.Parties {
		add(roleLabel(p.Role), p.Name)
	}
	if len(c.Chunks) > 0 {
		lines = append(lines, "Trechos relevantes dos autos:")
		for i, ch := range c.Chunks {
			if i >= 8 {
				break
			}
			lines = append(lines, strconv.Itoa(i+1)+". "+ch)
		}
	}
	if len(lines) > 0 {
		usr.WriteString("Contexto do caso:\n")
		usr.WriteString(strings.Join(lines, "\n"))
		usr.WriteString("\n\n")
	}

	// ── Current draft (preamble + sections) ──────────────────────────────────
	if len(c.Preamble) > 0 {
		usr.WriteString("Preâmbulo atual (NÃO reescreva — só leitura):\n")
		for _, p := range c.Preamble {
			usr.WriteString(p)
			usr.WriteString("\n")
		}
		usr.WriteString("\n")
	}
	if len(c.Sections) > 0 {
		usr.WriteString("Seções atuais:\n")
		for _, s := range c.Sections {
			usr.WriteString("• section_id=\"" + s.ID + "\" — " + s.Roman + " — " + s.Title + "\n")
			for _, p := range s.Paragraphs {
				usr.WriteString("  " + p + "\n")
			}
		}
		usr.WriteString("\n")
	}

	// ── Iteration params ─────────────────────────────────────────────────────
	usr.WriteString("Escopo do pedido: ")
	if c.Scope.Kind == "section" && c.Scope.SectionID != "" {
		usr.WriteString("APENAS a seção com section_id=\"" + c.Scope.SectionID + "\".\n")
	} else {
		usr.WriteString("A peça toda (qualquer seção pode ser reescrita).\n")
	}
	if instr := strings.TrimSpace(c.Instruction); instr != "" {
		usr.WriteString("Instrução do advogado: " + instr + "\n")
	} else if kl := kindLabel(c.Kind); kl != "" {
		usr.WriteString("Tipo de ajuste (quick): " + kl + "\n")
	}
	usr.WriteString("\nProduza as mudanças (JSON) conforme as regras.")

	return Composed{System: sys.String(), User: usr.String(), PromptVersion: draftIterateVersion}
}

// kindDirective returns the extra system directive for a "quick adjust" kind.
// Empty when kind is empty (free instruction) or unknown.
func kindDirective(kind string) string {
	switch kind {
	case "concise":
		return "DIRETIVA CONCISÃO: corte redundâncias e repetições; mantenha o essencial. " +
			"Frases curtas, sem \"data venia\", \"salvo melhor juízo\" em excesso."
	case "emphatic":
		return "DIRETIVA ÊNFASE: reforce a força retórica sem perder rigor técnico. " +
			"Argumentos sublinhados, verbos assertivos; evite adjetivação vazia."
	case "reinforce_thesis":
		return "DIRETIVA REFORÇAR A TESE PRINCIPAL: identifique o eixo argumentativo " +
			"central e traga-o de volta em cada seção; elimine desvios que enfraquecem a tese."
	case "add_grounds":
		return "DIRETIVA ADICIONAR FUNDAMENTO: ancore a argumentação em base legal ou " +
			"precedente citando o inciso específico do artigo e, quando cabível, " +
			"jurisprudência do STJ/STF ou súmula. Nunca invente números de processo."
	default:
		return ""
	}
}

// kindLabel is the human-readable label of a kind, for injection in the user
// prompt when there's no free-text instruction. Returns "" for empty/unknown.
func kindLabel(kind string) string {
	switch kind {
	case "concise":
		return "Mais conciso"
	case "emphatic":
		return "Mais enfático"
	case "reinforce_thesis":
		return "Reforçar a tese principal"
	case "add_grounds":
		return "Adicionar fundamento"
	default:
		return ""
	}
}
