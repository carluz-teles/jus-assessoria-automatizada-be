package advisory

import (
	"strings"
	"testing"
)

func TestTemplateComposer_SuggestTasks(t *testing.T) {
	c := NewTemplateComposer()
	out, err := c.Compose(AgentSuggestTasks, CaseContext{
		Court:          "TJSP",
		Class:          "Procedimento Comum",
		IntimationType: "INTIMACAO",
		PrazoKind:      "CONTESTACAO",
		PrazoDays:      15,
		Counting:       "BUSINESS",
		IntimationText: "Fica o réu intimado a contestar.",
		// Degree/Subject/Phase left empty → must NOT appear.
	})
	if err != nil {
		t.Fatalf("Compose() error = %v", err)
	}

	if out.PromptVersion != "suggest_tasks/v2" {
		t.Errorf("PromptVersion = %q, want suggest_tasks/v2", out.PromptVersion)
	}
	if strings.TrimSpace(out.System) == "" || strings.TrimSpace(out.User) == "" {
		t.Fatalf("empty system/user: system=%q user=%q", out.System, out.User)
	}
	// v2 contract: the system instructs the three outputs of one LLM call — the "O que
	// aconteceu" summary, the "O que fazer" recommendation, and a per-task description.
	for _, want := range []string{"summary", "recommendation", "description"} {
		if !strings.Contains(out.System, want) {
			t.Errorf("system prompt missing %q output instruction\n---\n%s", want, out.System)
		}
	}
	// The kind enum is unchanged — no invented values.
	if !strings.Contains(out.System, "ANALISE") || !strings.Contains(out.System, "CIENCIA") {
		t.Errorf("system prompt dropped the existing kind enum\n---\n%s", out.System)
	}
	// Context injected.
	for _, want := range []string{"TJSP", "Procedimento Comum", "INTIMACAO", "CONTESTACAO", "15 dias úteis", "Fica o réu intimado"} {
		if !strings.Contains(out.User, want) {
			t.Errorf("user prompt missing %q\n---\n%s", want, out.User)
		}
	}
	// Empty fields dropped (no dangling labels).
	for _, absent := range []string{"Grau:", "Assunto:", "Fase:"} {
		if strings.Contains(out.User, absent) {
			t.Errorf("user prompt should omit empty field label %q\n---\n%s", absent, out.User)
		}
	}
	// No playbook → system carries no playbook section.
	if strings.Contains(out.System, "playbook do escritório") {
		t.Errorf("system injected playbook section with empty Playbook")
	}
}

func TestTemplateComposer_PlaybookInjected(t *testing.T) {
	out, err := NewTemplateComposer().Compose(AgentSuggestTasks, CaseContext{
		PrazoKind: "RECURSO",
		Playbook:  "Sempre incluir 'Recolher o preparo' antes de protocolar.",
	})
	if err != nil {
		t.Fatalf("Compose() error = %v", err)
	}
	if !strings.Contains(out.System, "playbook do escritório") ||
		!strings.Contains(out.System, "Recolher o preparo") {
		t.Errorf("playbook not injected into system:\n%s", out.System)
	}
}

func TestTemplateComposer_EmptyContext(t *testing.T) {
	out, err := NewTemplateComposer().Compose(AgentSuggestTasks, CaseContext{})
	if err != nil {
		t.Fatalf("Compose() error = %v", err)
	}
	if !strings.Contains(out.User, "sem contexto adicional") {
		t.Errorf("empty context should say '(sem contexto adicional)':\n%s", out.User)
	}
}

func TestTemplateComposer_UnknownAgent(t *testing.T) {
	if _, err := NewTemplateComposer().Compose("nope", CaseContext{}); err == nil {
		t.Fatal("Compose(unknown) error = nil, want invalid")
	}
}

// The summarize_process template renders the full process context (identification +
// andamentos + intimações + prazos + RAG chunks) and pins its version. Unknown agent
// must stay a typed invalid.
func TestTemplateComposer_ComposeProcess(t *testing.T) {
	c := NewTemplateComposer()
	out, err := c.ComposeProcess(AgentSummarizeProcess, ProcessContext{
		CNJNumber: "0000001-23.2026.8.26.0001",
		Court:     "TJSP",
		Degree:    "G1",
		Class:     "Procedimento Comum",
		Subject:   "Contrato",
		Lifecycle: "ACTIVE",
		RecentMovements: []DocketEntryCtx{
			{OccurredAt: "2026-08-01", Text: "Partes intimadas para manifestação"},
		},
		ActiveIntimations: []IntimationCtx{
			{Type: "INTIMACAO", Teor: "Manifeste-se sobre a contestação", DeadlineDays: 5},
		},
		OpenDeadlines: []DeadlineCtx{
			{Kind: "MANIFESTACAO", EndDate: "2026-08-04", DaysRemaining: 3, Counting: "BUSINESS"},
		},
		DocumentChunks: []string{"trecho do contrato"},
	})
	if err != nil {
		t.Fatalf("ComposeProcess() error = %v", err)
	}

	if out.PromptVersion != "process_summary/v1" {
		t.Errorf("PromptVersion = %q, want process_summary/v1", out.PromptVersion)
	}
	if strings.TrimSpace(out.System) == "" || strings.TrimSpace(out.User) == "" {
		t.Fatalf("empty system/user: system=%q user=%q", out.System, out.User)
	}
	// The six output fields are instructed in the system prompt.
	for _, want := range []string{"summary", "current_status", "key_dates_and_deadlines", "recent_movements", "risks", "recommended_actions"} {
		if !strings.Contains(out.System, want) {
			t.Errorf("system prompt missing %q output instruction\n---\n%s", want, out.System)
		}
	}
	// Context injected.
	for _, want := range []string{"0000001-23.2026.8.26.0001", "TJSP", "Partes intimadas para manifestação", "Manifeste-se sobre a contestação", "vence em 2026-08-04", "Trecho 1: trecho do contrato"} {
		if !strings.Contains(out.User, want) {
			t.Errorf("user prompt missing %q\n---\n%s", want, out.User)
		}
	}
	// No playbook → the playbook SECTION (with examples/preferences) is not injected.
	if strings.Contains(out.System, "Siga o playbook do escritório (exemplos e preferências)") {
		t.Errorf("system injected playbook section with empty Playbook")
	}
}

func TestTemplateComposer_ComposeProcess_UnknownAgent(t *testing.T) {
	if _, err := NewTemplateComposer().ComposeProcess("nope", ProcessContext{}); err == nil {
		t.Fatal("ComposeProcess(unknown) error = nil, want invalid")
	}
}

func TestTemplateComposer_ComposeProcess_EmptyContext(t *testing.T) {
	out, err := NewTemplateComposer().ComposeProcess(AgentSummarizeProcess, ProcessContext{})
	if err != nil {
		t.Fatalf("ComposeProcess() error = %v", err)
	}
	if !strings.Contains(out.User, "sem contexto adicional") {
		t.Errorf("empty context should say '(sem contexto adicional)':\n%s", out.User)
	}
}

// TestTemplateComposer_ComposeDraft_v4_FullContext verifies that v4 injects all real
// data fields into the user message — including parties and signing lawyer — and that
// the system prompt contains the canonical structure instructions and v4 gold rule.
func TestTemplateComposer_ComposeDraft_v4_FullContext(t *testing.T) {
	c := NewTemplateComposer()
	out, err := c.ComposeDraft(AgentDraftMinuta, DraftContext{
		PieceType:      "DEFENSE",
		IntimationType: "CITACAO",
		IntimationText: "Fica o réu citado a contestar em 15 dias úteis.",
		Court:          "TJSP",
		Degree:         "G1",
		Class:          "Procedimento Comum",
		Subject:        "Contrato",
		CNJNumber:      "0000001-23.2026.8.26.0001",
		JudgingBody:    "3ª Vara Cível da Comarca de São Paulo",
		DeadlineDate:   "2026-09-01",
		Parties: []PartyCtx{
			{Role: "PLAINTIFF", Name: "AUTOR LTDA", Counsel: "Pedro (OAB/RS nº 119938)"},
			{Role: "DEFENDANT", Name: "RÉU SA"},
		},
		SigningLawyerName: "Dr. João Silva",
		SigningLawyerOAB:  "12345",
		SigningLawyerUF:   "SP",
		Chunks:            []string{"trecho 1", "trecho 2"},
	})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}

	if out.PromptVersion != "draft_minuta/v12" {
		t.Errorf("PromptVersion = %q, want draft_minuta/v12", out.PromptVersion)
	}

	// System must contain the gold rule (v4: parties + signing lawyer instruction).
	for _, want := range []string{
		"REGRA DE OURO",
		"NUNCA deixe",
		"placeholder",
		"PARTES do processo",
		"PLAINTIFF",
		"DEFENDANT",
		"ESTRUTURA CANÔNICA",
		"ENDEREÇAMENTO",
		"DOS FATOS",
		"DO DIREITO",
		"DOS PEDIDOS",
		"REQUISITOS LEGAIS",
		"DEFENSE",
		"COMPLAINT",
		"APPEAL",
	} {
		if !strings.Contains(out.System, want) {
			t.Errorf("system prompt missing %q\n---\n%s", want, out.System)
		}
	}

	// User message must contain all injected real fields including parties + lawyer.
	for _, want := range []string{
		"0000001-23.2026.8.26.0001",
		"3ª Vara Cível da Comarca de São Paulo",
		"TJSP",
		"G1",
		"Procedimento Comum",
		"Contrato",
		"CITACAO",
		"2026-09-01",
		"Fica o réu citado a contestar",
		"AUTOR LTDA",
		"RÉU SA",
		"Trechos relevantes dos autos:",
		"trecho 1",
	} {
		if !strings.Contains(out.User, want) {
			t.Errorf("user prompt missing %q\n---\n%s", want, out.User)
		}
	}

	// User must NOT start with "Contexto do caso:\n" header (dropped in v3+).
	if strings.HasPrefix(out.User, "Contexto do caso:") {
		t.Errorf("v4 user prompt should not start with 'Contexto do caso:' header")
	}
}

// TestTemplateComposer_ComposeDraft_v6_NoSigningBlock verifies that v6 nunca
// injeta linha de "Advogado signatário" no user prompt e nunca instrui o LLM
// a colocar nome/OAB no fecho — o bloco de assinatura é adicionado no PDF
// pelo pdfgen no momento do Sign.
func TestTemplateComposer_ComposeDraft_v6_NoSigningBlock(t *testing.T) {
	out, err := NewTemplateComposer().ComposeDraft(AgentDraftMinuta, DraftContext{
		PieceType:         "DEFENSE",
		SigningLawyerName: "Dr. João Silva",
		SigningLawyerOAB:  "12345",
		SigningLawyerUF:   "SP",
	})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	if strings.Contains(out.User, "Advogado signatário:") {
		t.Errorf("v6 user prompt must not inject 'Advogado signatário:' line\n---\n%s", out.User)
	}
	if strings.Contains(out.User, "Dr. João Silva") {
		t.Errorf("v6 user prompt must not leak SigningLawyerName\n---\n%s", out.User)
	}
	if strings.Contains(out.System, "[Nome do Advogado]") {
		t.Errorf("v6 system prompt must not mention [Nome do Advogado] fallback\n---\n%s", out.System)
	}
	if !strings.Contains(out.System, "PARE AÍ") {
		t.Errorf("v6 system must instruct to STOP after [data].\n---\n%s", out.System)
	}
}

// TestTemplateComposer_ComposeDraft_v4_EmptyContext verifies that an empty DraftContext
// renders the degraded-mode message (no placeholders injected).
func TestTemplateComposer_ComposeDraft_v4_EmptyContext(t *testing.T) {
	out, err := NewTemplateComposer().ComposeDraft(AgentDraftMinuta, DraftContext{})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	if !strings.Contains(out.User, "sem contexto adicional") {
		t.Errorf("empty context should say '(sem contexto adicional)':\n%s", out.User)
	}
	if out.PromptVersion != "draft_minuta/v12" {
		t.Errorf("PromptVersion = %q, want draft_minuta/v12", out.PromptVersion)
	}
}

// TestTemplateComposer_ComposeDraft_v4_EmptyFieldsDropped verifies that empty
// fields are not rendered as dangling labels (regression from v2 pattern).
func TestTemplateComposer_ComposeDraft_v4_EmptyFieldsDropped(t *testing.T) {
	out, err := NewTemplateComposer().ComposeDraft(AgentDraftMinuta, DraftContext{
		PieceType: "MOTION",
		// All other fields empty — should not appear in user message.
	})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	for _, absent := range []string{
		"Tribunal:", "Grau:", "Classe/rito:", "Assunto:", "Nº do processo:",
		"Órgão julgador/Vara:", "Prazo final:", "TEOR DA INTIMAÇÃO:",
	} {
		if strings.Contains(out.User, absent) {
			t.Errorf("user prompt should omit empty field label %q\n---\n%s", absent, out.User)
		}
	}
}

// TestTemplateComposer_ComposeDraft_v10_ProfileSectionsRendered verifies PART B:
// when ProfileSections are supplied (contestação), the system prompt renders the
// REAL section headers (Preliminares/Impugnação/Mérito/…) instead of the fixed trio,
// with the conditional + aceita_teses rules, and instructs to OVERRIDE the generic
// miolo.
func TestTemplateComposer_ComposeDraft_v10_ProfileSectionsRendered(t *testing.T) {
	out, err := NewTemplateComposer().ComposeDraft(AgentDraftMinuta, DraftContext{
		PieceType:       "DEFENSE",
		PieceProfileKey: "contestacao",
		ProfileSections: []ProfileSectionCtx{
			{Key: "preliminares", Titulo: "Das Preliminares", Ordem: 1, Obrigatoria: "condicional", AceitaTeses: true},
			{Key: "prejudiciais", Titulo: "Das Prejudiciais de Mérito", Ordem: 2, Obrigatoria: "condicional", AceitaTeses: true},
			{Key: "impugnacao_especifica", Titulo: "Da Impugnação Específica dos Fatos", Ordem: 3, Obrigatoria: "sim", AceitaTeses: true},
			{Key: "merito", Titulo: "Do Mérito", Ordem: 4, Obrigatoria: "sim", AceitaTeses: true},
			{Key: "pedidos", Titulo: "Dos Pedidos", Ordem: 5, Obrigatoria: "sim", AceitaTeses: false},
			{Key: "provas", Titulo: "Das Provas", Ordem: 6, Obrigatoria: "nao", AceitaTeses: false},
		},
	})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	// Real section titles (upper-cased) must appear in the system prompt.
	for _, want := range []string{
		"DAS PRELIMINARES", "DA IMPUGNAÇÃO ESPECÍFICA DOS FATOS", "DO MÉRITO", "DAS PROVAS",
		"ESTRUTURA DO MIOLO", "SUBSTITUI os itens 5, 6 e 7",
	} {
		if !strings.Contains(out.System, want) {
			t.Errorf("profile system prompt missing %q\n---\n%s", want, out.System)
		}
	}
	// Conditional rule + aceita_teses guidance must be present.
	if !strings.Contains(out.System, "CONDICIONAL") {
		t.Errorf("profile prompt missing CONDICIONAL rule")
	}
	if !strings.Contains(out.System, "TESES SELECIONADAS") {
		t.Errorf("profile prompt missing aceita_teses guidance")
	}
	if out.PromptVersion != "draft_minuta/v12" {
		t.Errorf("PromptVersion = %q, want draft_minuta/v12", out.PromptVersion)
	}
}

// TestTemplateComposer_ComposeDraft_v10_NoProfile_GenericFallback verifies that a
// draft WITHOUT ProfileSections keeps the generic Fatos/Direito/Pedidos structure and
// does NOT emit the profile-override block (backward-compat with legacy drafts).
func TestTemplateComposer_ComposeDraft_v10_NoProfile_GenericFallback(t *testing.T) {
	out, err := NewTemplateComposer().ComposeDraft(AgentDraftMinuta, DraftContext{
		PieceType: "DEFENSE",
		// No ProfileSections.
	})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	if strings.Contains(out.System, "ESTRUTURA DO MIOLO") {
		t.Errorf("no-profile prompt must NOT emit the profile-override block:\n%s", out.System)
	}
	// The generic canonical trio stays.
	for _, want := range []string{"DOS FATOS", "DO DIREITO", "DOS PEDIDOS"} {
		if !strings.Contains(out.System, want) {
			t.Errorf("generic fallback missing %q", want)
		}
	}
}

// TestTemplateComposer_ComposeDraft_v4_UnknownAgent verifies that an unknown agent
// returns a typed invalid error (programmer safety net).
func TestTemplateComposer_ComposeDraft_v4_UnknownAgent(t *testing.T) {
	if _, err := NewTemplateComposer().ComposeDraft("nope", DraftContext{}); err == nil {
		t.Fatal("ComposeDraft(unknown) error = nil, want invalid")
	}
}

// TestTemplateComposer_ComposeDraft_v4_PlaybookInjected verifies that a non-empty
// Playbook is injected into the system prompt.
func TestTemplateComposer_ComposeDraft_v4_PlaybookInjected(t *testing.T) {
	out, err := NewTemplateComposer().ComposeDraft(AgentDraftMinuta, DraftContext{
		PieceType: "DEFENSE",
		Playbook:  "Sempre incluir citação ao CPC.",
	})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	if !strings.Contains(out.System, "playbook do escritório") ||
		!strings.Contains(out.System, "Sempre incluir citação ao CPC.") {
		t.Errorf("playbook not injected into system:\n%s", out.System)
	}
}

// TestTemplateComposer_ComposeDraft_v4_ChunksInjected verifies that RAG chunks
// are injected with numbered labels in the user message.
func TestTemplateComposer_ComposeDraft_v4_ChunksInjected(t *testing.T) {
	out, err := NewTemplateComposer().ComposeDraft(AgentDraftMinuta, DraftContext{
		PieceType: "DEFENSE",
		Chunks:    []string{"chunk A", "chunk B", "chunk C"},
	})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	for _, want := range []string{"Trechos relevantes dos autos:", "1. chunk A", "2. chunk B", "3. chunk C"} {
		if !strings.Contains(out.User, want) {
			t.Errorf("user prompt missing %q\n---\n%s", want, out.User)
		}
	}
}

// ── Fatia 5 — teses/tom/instruções ───────────────────────────────────────────

// TestTemplateComposer_ComposeDraft_ToneEmpty_BackwardCompat is the pin: omitting
// Tone (empty string) MUST produce byte-identical System/User to explicitly passing
// the default tone — the pre-Fatia-5 wording, unchanged. This is the acceptance
// criterion "tone vazio/omitido → default server-side tecnico-formal, wording
// idêntica ao comportamento atual".
func TestTemplateComposer_ComposeDraft_ToneEmpty_BackwardCompat(t *testing.T) {
	c := NewTemplateComposer()
	base := DraftContext{
		PieceType:      "DEFENSE",
		IntimationText: "Fica o réu citado a contestar.",
		Court:          "TJSP",
	}

	empty, err := c.ComposeDraft(AgentDraftMinuta, base)
	if err != nil {
		t.Fatalf("ComposeDraft(tone=\"\") error = %v", err)
	}

	withDefault := base
	withDefault.Tone = "tecnico"
	explicit, err := c.ComposeDraft(AgentDraftMinuta, withDefault)
	if err != nil {
		t.Fatalf("ComposeDraft(tone=tecnico-formal) error = %v", err)
	}

	if empty.System != explicit.System {
		t.Errorf("System differs between Tone=\"\" and Tone=tecnico-formal:\n---empty---\n%s\n---explicit---\n%s",
			empty.System, explicit.System)
	}
	if empty.User != explicit.User {
		t.Errorf("User differs between Tone=\"\" and Tone=tecnico-formal:\n---empty---\n%s\n---explicit---\n%s",
			empty.User, explicit.User)
	}
	// Neither carries a tone directive — the base system prompt IS the
	// tecnico-formal register already (no "TOM:" line injected).
	if strings.Contains(empty.System, "TOM:") {
		t.Errorf("empty-tone system should not inject a TOM: directive\n---\n%s", empty.System)
	}
}

// TestTemplateComposer_ComposeDraft_ToneDirectives verifies that each non-default
// tone injects a DISTINCT directive into the system message.
func TestTemplateComposer_ComposeDraft_ToneDirectives(t *testing.T) {
	c := NewTemplateComposer()
	base := DraftContext{PieceType: "DEFENSE"}

	tones := []string{"objetivo", "enfatico"}
	seen := make(map[string]bool, len(tones))
	for _, tone := range tones {
		ctx := base
		ctx.Tone = tone
		out, err := c.ComposeDraft(AgentDraftMinuta, ctx)
		if err != nil {
			t.Fatalf("ComposeDraft(tone=%s) error = %v", tone, err)
		}
		if !strings.Contains(out.System, "TOM:") {
			t.Errorf("tone=%s: system prompt missing TOM: directive\n---\n%s", tone, out.System)
		}
		if seen[out.System] {
			t.Errorf("tone=%s: system prompt identical to a previously seen tone (want distinct wording)", tone)
		}
		seen[out.System] = true
	}
}

// TestTemplateComposer_ComposeDraft_InvalidTone_NoDirective verifies that an
// unrecognized tone string (should never happen past edge validation, but the
// composer itself must not panic or silently invent a directive) falls back to no
// directive — same as empty/tecnico-formal.
func TestTemplateComposer_ComposeDraft_InvalidTone_NoDirective(t *testing.T) {
	out, err := NewTemplateComposer().ComposeDraft(AgentDraftMinuta, DraftContext{
		PieceType: "DEFENSE",
		Tone:      "not-a-real-tone",
	})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	if strings.Contains(out.System, "TOM:") {
		t.Errorf("unrecognized tone should not inject a TOM: directive\n---\n%s", out.System)
	}
}

// TestTemplateComposer_ComposeDraft_InstructionsAndTheses verifies that
// Instructions and SelectedTheses are injected into the user message ONLY when
// non-empty, each under its own labeled section.
func TestTemplateComposer_ComposeDraft_InstructionsAndTheses(t *testing.T) {
	c := NewTemplateComposer()

	// Both empty → neither section appears.
	empty, err := c.ComposeDraft(AgentDraftMinuta, DraftContext{PieceType: "DEFENSE"})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	for _, absent := range []string{"Instruções específicas", "TESES SELECIONADAS"} {
		if strings.Contains(empty.User, absent) {
			t.Errorf("empty context should omit %q section\n---\n%s", absent, empty.User)
		}
	}

	// v11: without a profile → theses go to the generic "DO DIREITO" section, each
	// carrying its Fundamento/Dispositivo/Apoio nos autos.
	theses := []SelectedThesisCtx{
		{Label: "Prescrição intercorrente", Foundation: "Inércia do exequente por mais de 5 anos", Reference: "art. 924, V, CPC", Excerpt: "sem movimentação útil desde 2018", SourceLabel: "fls. 120"},
		{Label: "Excesso de execução", Foundation: "Cálculo inclui juros indevidos"},
	}
	full, err := c.ComposeDraft(AgentDraftMinuta, DraftContext{
		PieceType:      "DEFENSE",
		Instructions:   "Enfatizar a boa-fé do réu.",
		SelectedTheses: theses,
	})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	for _, want := range []string{
		"Instruções específicas do advogado para esta minuta:",
		"Enfatizar a boa-fé do réu.",
		"TESES SELECIONADAS pelo advogado",
		"seção \"DO DIREITO\"",
		"Prescrição intercorrente",
		"Excesso de execução",
		"Fundamento: Inércia do exequente por mais de 5 anos",
		"Dispositivo: art. 924, V, CPC.",
		"Apoio nos autos (fls. 120): \"sem movimentação útil desde 2018\".",
	} {
		if !strings.Contains(full.User, want) {
			t.Errorf("no-profile user prompt missing %q\n---\n%s", want, full.User)
		}
	}
	if strings.Contains(full.User, "II – DO DIREITO") {
		t.Errorf("v11 must not hardcode the old \"II – DO DIREITO\" destination\n---\n%s", full.User)
	}

	// v11: WITH a profile → theses are routed across the miolo sections (no standalone
	// "Do Direito" avulsa), still carrying Fundamento/Dispositivo/Apoio.
	withProfile, err := c.ComposeDraft(AgentDraftMinuta, DraftContext{
		PieceType:       "DEFENSE",
		PieceProfileKey: "contestacao",
		ProfileSections: []ProfileSectionCtx{
			{Key: "preliminares", Titulo: "Das Preliminares", Ordem: 1, Obrigatoria: "condicional", AceitaTeses: true},
			{Key: "merito", Titulo: "Do Mérito", Ordem: 2, Obrigatoria: "sim", AceitaTeses: true},
		},
		SelectedTheses: theses,
	})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	for _, want := range []string{
		"TESES SELECIONADAS pelo advogado",
		"na seção do MIOLO onde ela se encaixa",
		"Prescrição intercorrente",
		"Fundamento: Inércia do exequente por mais de 5 anos",
		"Dispositivo: art. 924, V, CPC.",
		"Apoio nos autos (fls. 120): \"sem movimentação útil desde 2018\".",
	} {
		if !strings.Contains(withProfile.User, want) {
			t.Errorf("profile user prompt missing %q\n---\n%s", want, withProfile.User)
		}
	}
	// The theses block itself must NOT send them to a standalone "DO DIREITO".
	if strings.Contains(withProfile.User, "desenvolva CADA UMA por extenso e integrada ao argumento, na seção \"DO DIREITO\"") {
		t.Errorf("with profile, theses must route to miolo sections, not \"DO DIREITO\"\n---\n%s", withProfile.User)
	}
}

// TestTemplateComposer_ComposeDraft_v12_MultiAnchor verifies that a selected thesis
// with N anchors lists EVERY autos document that backs it (multi-âncora), not only the
// primary — and that a thesis with no anchors falls back to the singular Excerpt path.
func TestTemplateComposer_ComposeDraft_v12_MultiAnchor(t *testing.T) {
	c := NewTemplateComposer()
	out, err := c.ComposeDraft(AgentDraftMinuta, DraftContext{
		PieceType: "DEFENSE",
		SelectedTheses: []SelectedThesisCtx{
			{
				Label:      "Risco de extinção",
				Foundation: "Múltiplas advertências nos autos",
				Anchors: []ThesisAnchorCtx{
					{Label: "Certidão · pág. 5", Excerpt: "sob pena de extinção"},
					{Label: "Ato ordinatório · pág. 2", Excerpt: "dar andamento sob pena de arquivamento"},
				},
			},
			// No anchors → singular Excerpt path (backward-compat).
			{Label: "Excesso", Excerpt: "juros indevidos", SourceLabel: "fls. 30"},
		},
	})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	for _, want := range []string{
		"Apoio nos autos: Certidão · pág. 5 (\"sob pena de extinção\"); Ato ordinatório · pág. 2 (\"dar andamento sob pena de arquivamento\").",
		"Apoio nos autos (fls. 30): \"juros indevidos\".",
	} {
		if !strings.Contains(out.User, want) {
			t.Errorf("multi-anchor user prompt missing %q\n---\n%s", want, out.User)
		}
	}
	if out.PromptVersion != "draft_minuta/v12" {
		t.Errorf("PromptVersion = %q, want draft_minuta/v12", out.PromptVersion)
	}
}

// TestTemplateComposer_ComposeTheses verifies the suggest_theses agent renders a
// non-empty, versioned prompt with the injected case context.
func TestTemplateComposer_ComposeTheses(t *testing.T) {
	c := NewTemplateComposer()
	out, err := c.ComposeTheses(AgentSuggestTheses, DraftContext{
		PieceType:      "DEFENSE",
		Court:          "TJSP",
		IntimationText: "Fica o réu citado a contestar em 15 dias.",
		Parties: []PartyCtx{
			{Role: "DEFENDANT", Name: "RÉU SA"},
		},
	})
	if err != nil {
		t.Fatalf("ComposeTheses() error = %v", err)
	}
	if out.PromptVersion != "suggest_theses/v4" {
		t.Errorf("PromptVersion = %q, want suggest_theses/v4", out.PromptVersion)
	}
	if strings.TrimSpace(out.System) == "" || strings.TrimSpace(out.User) == "" {
		t.Fatalf("empty system/user: system=%q user=%q", out.System, out.User)
	}
	// v2: campo `evidence` obrigatório + critérios objetivos por confidence.
	for _, want := range []string{"label", "confidence", "reference", "foundation", "evidence", "source_refs"} {
		if !strings.Contains(out.System, want) {
			t.Errorf("system prompt missing %q output field\n---\n%s", want, out.System)
		}
	}
	// v4: dedup + multi-âncora instructions.
	for _, want := range []string{"CONSOLIDAÇÃO", "UMA ÚNICA tese", "várias âncoras"} {
		if !strings.Contains(out.System, want) {
			t.Errorf("system prompt missing v4 dedup/multi-anchor rule %q\n---\n%s", want, out.System)
		}
	}
	for _, want := range []string{"CRITÉRIOS DE CONFIDENCE", "evidence.length"} {
		if !strings.Contains(out.System, want) {
			t.Errorf("system prompt missing v2 rubric %q\n---\n%s", want, out.System)
		}
	}
	for _, want := range []string{"TJSP", "Fica o réu citado a contestar", "RÉU SA"} {
		if !strings.Contains(out.User, want) {
			t.Errorf("user prompt missing %q\n---\n%s", want, out.User)
		}
	}
}

// TestTemplateComposer_ComposeTheses_EmptyContext verifies the degraded-mode message.
func TestTemplateComposer_ComposeTheses_EmptyContext(t *testing.T) {
	out, err := NewTemplateComposer().ComposeTheses(AgentSuggestTheses, DraftContext{})
	if err != nil {
		t.Fatalf("ComposeTheses() error = %v", err)
	}
	if !strings.Contains(out.User, "sem contexto adicional") {
		t.Errorf("empty context should say '(sem contexto adicional)':\n%s", out.User)
	}
}

// TestTemplateComposer_ComposeTheses_UnknownAgent verifies an unknown agent stays a
// typed invalid.
func TestTemplateComposer_ComposeTheses_UnknownAgent(t *testing.T) {
	if _, err := NewTemplateComposer().ComposeTheses("nope", DraftContext{}); err == nil {
		t.Fatal("ComposeTheses(unknown) error = nil, want invalid")
	}
}
