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

// Composed is the composer's output: the system + user messages and the prompt_version that
// produced them. The caller passes System/User to the LLM (lib/llm) and stamps PromptVersion on
// the persisted suggestion (proveniência) so the feedback delta is comparable across versions.
type Composed struct {
	System        string
	User          string
	PromptVersion string
}

// PromptComposer composes the instruction-set for a named advisory agent from a case context.
// It is a first-class, versioned artifact (§3): the agent name + version identify exactly which
// template ran. Behind an interface so the deterministic v0 (TemplateComposer) can later be
// swapped for / wrapped by an LLM-refined composer (§8) without touching the callers.
type PromptComposer interface {
	Compose(agent string, c CaseContext) (Composed, error)
}

// Agent identifiers — one per specialized advisory task (erd-ai-advisory.md §4). Each maps to a
// template + a version below. Kept as consts so a caller never passes a free string.
const (
	AgentSuggestTasks = "suggest_tasks"
)

// suggestTasksVersion is the pinned version of the suggest_tasks template. BUMP IT whenever the
// template text changes so the feedback delta of the OLD prompt stays attributable to the OLD
// version (never silently mixed with a new one). This is the axis the A/B improvement turns on.
const suggestTasksVersion = "suggest_tasks/v1"

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

// composeSuggestTasks renders the suggest_tasks instruction-set. The system message is the stable
// role/task (the validated prompt), with the playbook injected when present; the user message
// carries the case context, built line by line so empty fields drop out. The model returns the
// tasks via structured output (the schema lives with the caller), so the prompt stays about WHAT
// to produce, not the JSON shape.
func composeSuggestTasks(c CaseContext) Composed {
	var sys strings.Builder
	sys.WriteString(
		"Você é um assistente jurídico brasileiro. A partir de uma intimação e do prazo dela " +
			"derivado, liste as tarefas OBJETIVAS e ACIONÁVEIS que o advogado deve executar para " +
			"cumprir o prazo. Cada tarefa tem um título curto e imperativo e um kind (categoria curta, " +
			"ex.: ANALISE, PECA, PROTOCOLO, PROVIDENCIA, CIENCIA). Não invente fatos que não estejam no " +
			"contexto; prefira tarefas genéricas a suposições. Não repita tarefas.",
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
	usr.WriteString("\n\nListe as tarefas sugeridas.")

	return Composed{System: sys.String(), User: usr.String(), PromptVersion: suggestTasksVersion}
}
