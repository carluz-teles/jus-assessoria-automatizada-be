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
