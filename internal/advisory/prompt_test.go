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

	if out.PromptVersion != "draft_minuta/v5" {
		t.Errorf("PromptVersion = %q, want draft_minuta/v5", out.PromptVersion)
	}

	// System must contain the gold rule (v4: parties + signing lawyer instruction).
	for _, want := range []string{
		"REGRA DE OURO",
		"NUNCA deixe",
		"placeholder",
		"PARTES do processo",
		"ADVOGADO SIGNATÁRIO",
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
		"Dr. João Silva",
		"OAB/SP nº 12345",
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

// TestTemplateComposer_ComposeDraft_v4_NoSigningLawyer verifies that when no signing
// lawyer is provided the system instructs using the literal marker [Nome do Advogado].
func TestTemplateComposer_ComposeDraft_v4_NoSigningLawyer(t *testing.T) {
	out, err := NewTemplateComposer().ComposeDraft(AgentDraftMinuta, DraftContext{
		PieceType: "DEFENSE",
		// SigningLawyer fields all empty — should NOT appear in user message.
	})
	if err != nil {
		t.Fatalf("ComposeDraft() error = %v", err)
	}
	// User must NOT inject a signing-lawyer line when fields are empty.
	if strings.Contains(out.User, "Advogado signatário:") {
		t.Errorf("user prompt should omit 'Advogado signatário:' when fields empty\n---\n%s", out.User)
	}
	// System must still instruct fallback to markers.
	if !strings.Contains(out.System, "[Nome do Advogado]") {
		t.Errorf("system must mention fallback marker [Nome do Advogado] when lawyer absent\n---\n%s", out.System)
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
	if out.PromptVersion != "draft_minuta/v5" {
		t.Errorf("PromptVersion = %q, want draft_minuta/v3", out.PromptVersion)
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
