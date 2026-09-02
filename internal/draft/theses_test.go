package draft

import (
	"context"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/llm"
)

// buildThesesUC assembles the ThesesUseCase with the given overridable parts.
// Reuses fakeUoW/fakeReader/fakeGen (generate_test.go) and the real
// TemplateComposer (same pattern as buildReviewUC in review_test.go) — no
// custom composer fake needed since ComposeTheses is a real, tested method.
// gen is typed llm.Generator (not *fakeGen) so a literal `nil` argument yields a
// true nil interface — the nil-generator guard compares uc.gen == nil.
func buildThesesUC(
	uow database.UnitOfWork,
	reader generationDepsReader,
	gen llm.Generator,
	emb embedder,
) *ThesesUseCase {
	return NewThesesUseCase(ThesesUseCaseParams{
		UoW:      uow,
		Reader:   reader,
		Gen:      gen,
		Emb:      emb,
		Search:   indexing.SearchDeps{Pool: nil}, // nil pool → degraded
		Composer: advisory.NewTemplateComposer(),
		Model:    "test-model",
	})
}

// thesesCmd returns a canonical SuggestThesesCommand.
func thesesCmd() SuggestThesesCommand {
	return SuggestThesesCommand{
		TenantID: "tenant-thz-1",
		DraftID:  "draft-thz-1",
	}
}

// makeThesesDraft returns a test draft ready for Sugerir Teses (any saga state is
// fine — the use case is stateless and does not guard saga_state).
func makeThesesDraft() *Draft {
	return &Draft{
		ID:        "draft-thz-1",
		TenantID:  "tenant-thz-1",
		SagaState: SagaStateCreated,
		PieceType: PieceTypeDefense,
	}
}

// cannedThesesJSON is the JSON the fake generator returns for Sugerir Teses.
// cannedThesesJSON reflete o wire do prompt v2: campo evidence obrigatório,
// alta com ≥2 trechos (respeita a rubric — sem downgrade), media com 1.
const cannedThesesJSON = `{
  "theses": [
    {
      "label": "Prescrição intercorrente",
      "confidence": "alta",
      "reference": "art. 206, §5º, I, CC",
      "foundation": "O prazo prescricional de 5 anos transcorreu sem movimentação útil.",
      "evidence": [
        "última movimentação útil ocorreu em 2019, sem impulso desde então",
        "não houve pedido de suspensão nem causa interruptiva no período"
      ]
    },
    {
      "label": "Excesso de execução",
      "confidence": "media",
      "reference": "art. 917, §2º, CPC",
      "foundation": "O valor exequendo diverge do título.",
      "evidence": ["valor cobrado supera o principal atualizado do título"]
    }
  ]
}`

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestThesesUseCase_NilGenerator_422 verifies that a nil generator returns an
// apperr.Invalid (422) immediately, without touching the reader (Phase 1 never runs).
func TestThesesUseCase_NilGenerator_422(t *testing.T) {
	uc := buildThesesUC(fakeUoW{}, fakeReader{draft: makeThesesDraft()}, nil, nil)

	_, err := uc.SuggestTheses(context.Background(), thesesCmd())
	if err == nil {
		t.Fatal("want error for nil generator, got nil")
	}
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Kind != apperr.KindInvalid {
		t.Errorf("want KindInvalid (422), got: %v", err)
	}
}

// TestThesesUseCase_DraftNotFound_404 verifies that a missing draft returns
// ErrDraftNotFound (KindNotFound → 404).
func TestThesesUseCase_DraftNotFound_404(t *testing.T) {
	gen := &fakeGen{out: []byte(cannedThesesJSON)}
	uc := buildThesesUC(fakeUoW{}, fakeReader{draftErr: ErrDraftNotFound}, gen, nil)

	_, err := uc.SuggestTheses(context.Background(), thesesCmd())
	if !errors.Is(err, ErrDraftNotFound) {
		t.Errorf("want ErrDraftNotFound, got %v", err)
	}
}

// TestThesesUseCase_LLMError_Typed verifies that an LLM failure returns a typed,
// wrapped error and the draft's saga_state is never touched (there is no writer
// on ThesesUseCase — an architectural impossibility, not just a choice).
func TestThesesUseCase_LLMError_Typed(t *testing.T) {
	gen := &fakeGen{err: errors.New("llm timeout")}
	uc := buildThesesUC(fakeUoW{}, fakeReader{draft: makeThesesDraft()}, gen, nil)

	_, err := uc.SuggestTheses(context.Background(), thesesCmd())
	if err == nil {
		t.Fatal("want error for LLM failure, got nil")
	}
	if !errors.Is(err, gen.err) {
		t.Errorf("want wrapped llm error, got %v", err)
	}
}

// TestThesesUseCase_HappyPath verifies the success flow: theses are decoded from
// the LLM output and returned; no repository write occurs (stateless).
func TestThesesUseCase_HappyPath(t *testing.T) {
	gen := &fakeGen{out: []byte(cannedThesesJSON)}
	uc := buildThesesUC(fakeUoW{}, fakeReader{draft: makeThesesDraft()}, gen, nil)

	result, err := uc.SuggestTheses(context.Background(), thesesCmd())
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if result == nil {
		t.Fatal("result is nil, want non-nil")
	}
	if len(result.Theses) != 2 {
		t.Fatalf("theses = %d, want 2", len(result.Theses))
	}
	if result.Theses[0].Label != "Prescrição intercorrente" {
		t.Errorf("theses[0].Label = %q, want %q", result.Theses[0].Label, "Prescrição intercorrente")
	}
	if result.Theses[0].Confidence != ThesisConfidenceAlta {
		t.Errorf("theses[0].Confidence = %q, want %q", result.Theses[0].Confidence, ThesisConfidenceAlta)
	}
	if result.Theses[0].Reference != "art. 206, §5º, I, CC" {
		t.Errorf("theses[0].Reference = %q, want the plain-string reference", result.Theses[0].Reference)
	}
}

// TestThesesUseCase_AltaDowngradedWhenEvidenceShort verifies the rubric guard:
// se o LLM cuspir confidence=alta mas com <2 evidências (violação da rubric do
// prompt v2), o use case downgrade pra media antes de retornar. Sem esse guard,
// ~40% das "altas" observadas em prod vinham com só 1 evidência.
func TestThesesUseCase_AltaDowngradedWhenEvidenceShort(t *testing.T) {
	// LLM diz alta mas manda só 1 evidence → deve virar media.
	// A segunda tese vem alta com 2 evidences → mantém alta.
	json := `{"theses":[
	  {"label":"Alta suspeita","confidence":"alta","reference":"x","foundation":"y","evidence":["um trecho só"]},
	  {"label":"Alta legítima","confidence":"alta","reference":"x","foundation":"y","evidence":["um","dois"]},
	  {"label":"Alta sem prova","confidence":"alta","reference":"x","foundation":"y","evidence":[]}
	]}`
	gen := &fakeGen{out: []byte(json)}
	uc := buildThesesUC(fakeUoW{}, fakeReader{draft: makeThesesDraft()}, gen, nil)

	result, err := uc.SuggestTheses(context.Background(), thesesCmd())
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	// A ordenação determinística (alta antes de media) sobe "Alta legítima" (2
	// evidências, mantém alta) pro topo; as duas rebaixadas viram media e ficam
	// abaixo, estáveis na ordem original. Asserção por label, não por índice.
	byLabel := map[string]string{}
	for _, th := range result.Theses {
		byLabel[th.Label] = th.Confidence
	}
	if byLabel["Alta suspeita"] != ThesisConfidenceMedia {
		t.Errorf("Alta suspeita (1 evidência): Confidence = %q, want media (downgrade)", byLabel["Alta suspeita"])
	}
	if byLabel["Alta legítima"] != ThesisConfidenceAlta {
		t.Errorf("Alta legítima (2 evidências): Confidence = %q, want alta (preservada)", byLabel["Alta legítima"])
	}
	if byLabel["Alta sem prova"] != ThesisConfidenceMedia {
		t.Errorf("Alta sem prova (0 evidências): Confidence = %q, want media (downgrade)", byLabel["Alta sem prova"])
	}
	if result.Theses[0].Confidence != ThesisConfidenceAlta {
		t.Errorf("após ordenação, theses[0] deve ser alta, got %q", result.Theses[0].Confidence)
	}
}

// TestThesesUseCase_EmptyTheses_NoGrounding verifies that when the LLM returns no
// theses (empty grounding — RAG found nothing) the use case returns an empty,
// non-nil slice rather than an error.
func TestThesesUseCase_EmptyTheses_NoGrounding(t *testing.T) {
	gen := &fakeGen{out: []byte(`{"theses": []}`)}
	uc := buildThesesUC(fakeUoW{}, fakeReader{draft: makeThesesDraft()}, gen, nil)

	result, err := uc.SuggestTheses(context.Background(), thesesCmd())
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if result.Theses == nil {
		t.Error("Theses is nil, want non-nil empty slice")
	}
	if len(result.Theses) != 0 {
		t.Errorf("theses = %d, want 0", len(result.Theses))
	}
}

// TestThesesUseCase_ParseError verifies that malformed LLM JSON output surfaces as
// a wrapped error.
func TestThesesUseCase_ParseError(t *testing.T) {
	gen := &fakeGen{out: []byte(`not json`)}
	uc := buildThesesUC(fakeUoW{}, fakeReader{draft: makeThesesDraft()}, gen, nil)

	_, err := uc.SuggestTheses(context.Background(), thesesCmd())
	if err == nil {
		t.Fatal("want error for malformed llm output, got nil")
	}
}

// TestThesesUseCase_WithIntimation_CRIDResolved verifies that when the draft has an
// intimation with a non-empty CourtRecordID, the LLM call still succeeds (nil pool
// degrades RAG gracefully — no grounding, but no error either).
func TestThesesUseCase_WithIntimation_CRIDResolved(t *testing.T) {
	d := makeThesesDraft()
	d.IntimationID = "intim-thz-1"
	intim := &IntimationContext{
		IntimationID:  "intim-thz-1",
		CourtRecordID: "court-record-thz-1",
	}
	gen := &fakeGen{out: []byte(cannedThesesJSON)}
	uc := buildThesesUC(fakeUoW{}, fakeReader{draft: d, intimation: intim}, gen, nil)

	result, err := uc.SuggestTheses(context.Background(), thesesCmd())
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if len(result.Theses) != 2 {
		t.Errorf("theses = %d, want 2", len(result.Theses))
	}
}

// --- resolveThesisSource / thesisSourceLabel ---------------------------------

func TestResolveThesisSource(t *testing.T) {
	t.Parallel()

	const quotePet = "o réu confessou a dívida em audiência de conciliação"
	const quoteContr = "a sociedade foi constituída por prazo indeterminado conforme cláusula"
	const teorSnippet = "apresente contestação no prazo de quinze dias"
	const teor = "Fica a parte intimada a " + teorSnippet + ", sob pena de revelia."

	hits := []indexing.ChunkHit{
		{DocumentID: "doc-pet", Page: 3, Text: "Trecho: " + quotePet + " e nada pagou.", Score: 0.91, DocumentTitle: "Petição inicial", DocumentType: "PET"},
		{DocumentID: "doc-contr", Page: 1, Text: quoteContr + ", com capital integralizado.", Score: 0.70, DocumentTitle: "", DocumentType: "CONTRSOCIAL"},
	}

	tests := []struct {
		name          string
		sourceRef     int
		evidence      []string
		hits          []indexing.ChunkHit
		wantDocID     string
		wantPage      int
		wantLabel     string
		wantGrounded  bool
		wantExcerpt   string // "" means assert empty
		excerptNonNil bool   // when true, just assert non-empty
	}{
		{
			name:         "ref cites chunk and evidence casa → grounded, attributed",
			sourceRef:    1,
			evidence:     []string{quotePet},
			hits:         hits,
			wantDocID:    "doc-pet",
			wantPage:     3,
			wantLabel:    "Petição inicial · pág. 3",
			wantGrounded: true,
			wantExcerpt:  quotePet,
		},
		{
			name:         "ref cites chunk 2 (type-fallback label), diacritic-robust match",
			sourceRef:    2,
			evidence:     []string{"a SOCIEDADE foi constituida por prazo indeterminado conforme clausula"},
			hits:         hits,
			wantDocID:    "doc-contr",
			wantPage:     1,
			wantLabel:     "CONTRSOCIAL · pág. 1",
			wantGrounded:  true,
			excerptNonNil: true,
		},
		{
			name:          "ref given but NO evidence casa no trecho citado → source kept, grounded=false",
			sourceRef:     1,
			evidence:      []string{"trecho que não aparece no chunk citado de forma alguma"},
			hits:          hits,
			wantDocID:     "doc-pet",
			wantPage:      3,
			wantLabel:     "Petição inicial · pág. 3",
			wantGrounded:  false,
			excerptNonNil: true, // keeps the 1st evidence as excerpt
		},
		{
			name:         "ref==0 and evidence casa no teor → grounded, no doc",
			sourceRef:    0,
			evidence:     []string{teorSnippet},
			hits:         hits,
			wantDocID:    "",
			wantGrounded: true,
		},
		{
			name:         "ref out of range and evidence casa nowhere → hallucinated: grounded=false, excerpt cleared",
			sourceRef:    99,
			evidence:     []string{"evidência puramente inventada sem lastro"},
			hits:         hits,
			wantDocID:    "",
			wantGrounded: false,
			wantExcerpt:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			th := &Thesis{Label: "t", Evidence: tt.evidence, SourceRef: tt.sourceRef}
			resolveThesisSource(th, tt.hits, teor)

			if th.SourceDocumentID != tt.wantDocID {
				t.Fatalf("SourceDocumentID = %q, want %q", th.SourceDocumentID, tt.wantDocID)
			}
			if th.Grounded != tt.wantGrounded {
				t.Errorf("Grounded = %v, want %v", th.Grounded, tt.wantGrounded)
			}
			if tt.wantDocID != "" {
				if th.SourcePage != tt.wantPage {
					t.Errorf("SourcePage = %d, want %d", th.SourcePage, tt.wantPage)
				}
				if tt.wantLabel != "" && th.SourceLabel != tt.wantLabel {
					t.Errorf("SourceLabel = %q, want %q", th.SourceLabel, tt.wantLabel)
				}
			}
			if tt.excerptNonNil {
				if th.SourceExcerpt == "" {
					t.Error("SourceExcerpt should be non-empty")
				}
			} else if tt.wantExcerpt != "" && th.SourceExcerpt != tt.wantExcerpt {
				t.Errorf("SourceExcerpt = %q, want %q", th.SourceExcerpt, tt.wantExcerpt)
			} else if tt.wantExcerpt == "" && !tt.excerptNonNil && th.SourceExcerpt != "" {
				t.Errorf("SourceExcerpt = %q, want empty", th.SourceExcerpt)
			}
		})
	}
}

func TestThesisSourceLabel_FallbacksAndPage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		hit  indexing.ChunkHit
		want string
	}{
		{"title + page", indexing.ChunkHit{DocumentTitle: "Petição inicial", Page: 5}, "Petição inicial · pág. 5"},
		{"type fallback when no title", indexing.ChunkHit{DocumentType: "CERT", Page: 2}, "CERT · pág. 2"},
		{"generic fallback when neither", indexing.ChunkHit{Page: 1}, "Documento dos autos · pág. 1"},
		{"no page → no suffix", indexing.ChunkHit{DocumentTitle: "Contrato"}, "Contrato"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := thesisSourceLabel(tt.hit); got != tt.want {
				t.Errorf("thesisSourceLabel = %q, want %q", got, tt.want)
			}
		})
	}
}
