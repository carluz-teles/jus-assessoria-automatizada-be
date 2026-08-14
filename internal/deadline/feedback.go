package deadline

import (
	"context"
	"strings"
)

// feedback.go is the AI feedback loop's domain core (erd-ai-advisory §"feedback loop",
// camadas 1-2). Camada 1 (proveniência): SuggestionRecord + SuggestionStore capture WHAT
// the LLM suggested, with WHICH prompt_version and model, when the F2 opens. Camada 2
// (delta): computeSuggestionDelta diffs that stored suggestion against what the human
// actually confirmed — kept / removed / added — the raw signal for "how trustworthy was the
// IA" and, eventually, for refining the versioned prompt. The delta is pure (no I/O): the
// confirm loads the record inside its tx and emits it as deadline.suggestion_feedback.

// SuggestionRecord is one persisted batch of AI-suggested tasks with its provenance. It is
// written on the read path (the suggester, after the LLM answers) and read back at confirm
// to compute the delta. PromptVersion + Model are the provenance (which instruction-set,
// which LLM); Tasks is the exact suggested set.
type SuggestionRecord struct {
	DeadlineID    string
	IntimationID  string
	PromptVersion string
	Model         string
	Tasks         []SuggestedTask
}

// SuggestionStore persists suggestion provenance (feedback loop, camada 1). It is a
// separate port from the write Repository because the suggester runs on the READ path
// (GET /v1/prazos/:id/suggested-tasks) and the persist is best-effort: a failure here must
// never break the F2 (the lawyer still gets the pre-fill). Its implementation owns its own
// unit of work (a single tenant-scoped insert), like the pool-backed read repo.
type SuggestionStore interface {
	SaveSuggestion(ctx context.Context, tenantID string, rec SuggestionRecord) (string, error)
}

// SuggestionDelta is the diff between what the IA suggested and what the human confirmed
// (feedback loop, camada 2). Titles carry the ORIGINAL text (not the normalized match key)
// so the aggregate is legible. Kept = suggested tasks that survived verbatim; Removed =
// suggested tasks the human dropped; Added = tasks the human wrote that the IA never
// proposed. Edited (a suggestion reworded rather than kept/dropped) is intentionally NOT a
// separate bucket in v1: without stable per-suggestion ids a reworded title is
// indistinguishable from a remove+add, so we report the honest sets and leave finer
// alignment to a later slice that threads suggestion-item ids through the F2.
type SuggestionDelta struct {
	PromptVersion  string
	Model          string
	SuggestedCount int
	ConfirmedCount int
	Kept           []string
	Removed        []string
	Added          []string
}

// computeSuggestionDelta diffs the stored suggestion against the confirmed tasks by
// normalized title (lowercased, whitespace-collapsed) — the coarse but honest v1 match.
// It is pure: the confirm calls it inside its tx and hands the result to the outbox. A nil
// record (no suggestion for this prazo) is handled by the CALLER (it simply does not emit).
func computeSuggestionDelta(rec SuggestionRecord, confirmed []ConfirmTaskInput) SuggestionDelta {
	confirmedKeys := make(map[string]bool, len(confirmed))
	for _, t := range confirmed {
		confirmedKeys[normalizeTitle(t.Title)] = true
	}
	suggestedKeys := make(map[string]bool, len(rec.Tasks))
	for _, t := range rec.Tasks {
		suggestedKeys[normalizeTitle(t.Title)] = true
	}

	delta := SuggestionDelta{
		PromptVersion:  rec.PromptVersion,
		Model:          rec.Model,
		SuggestedCount: len(rec.Tasks),
		ConfirmedCount: len(confirmed),
		Kept:           []string{},
		Removed:        []string{},
		Added:          []string{},
	}

	// A suggested task was kept if its normalized title appears among the confirmed;
	// otherwise the human removed it. Dedup by normalized key so a model that repeats a
	// title does not double-count.
	seenSuggested := make(map[string]bool, len(rec.Tasks))
	for _, t := range rec.Tasks {
		key := normalizeTitle(t.Title)
		if seenSuggested[key] {
			continue
		}
		seenSuggested[key] = true
		if confirmedKeys[key] {
			delta.Kept = append(delta.Kept, t.Title)
		} else {
			delta.Removed = append(delta.Removed, t.Title)
		}
	}

	// A confirmed task the IA never suggested is one the human added — the signal for a gap
	// in the prompt (something the model consistently misses).
	seenConfirmed := make(map[string]bool, len(confirmed))
	for _, t := range confirmed {
		key := normalizeTitle(t.Title)
		if seenConfirmed[key] {
			continue
		}
		seenConfirmed[key] = true
		if !suggestedKeys[key] {
			delta.Added = append(delta.Added, t.Title)
		}
	}

	return delta
}

// normalizeTitle is the match key for the delta: lowercase + collapse internal whitespace
// + trim, so "Redigir  contestação " and "redigir contestação" match. Deliberately shallow
// (no accent/stemming): a coarse key is enough to tell kept from removed, and over-matching
// would blur genuinely distinct tasks.
func normalizeTitle(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
