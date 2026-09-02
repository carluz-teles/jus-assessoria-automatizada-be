package draft

import (
	"reflect"
	"testing"
)

// helper: build a StructuredContent from section titles (roman/paragraphs kept simple).
func sc(titles ...string) *StructuredContent {
	secs := make([]StructuredSection, 0, len(titles))
	for i, t := range titles {
		secs = append(secs, StructuredSection{
			ID:         "s",
			Title:      t,
			Paragraphs: []string{"corpo da seção " + t},
		})
		_ = i
	}
	return &StructuredContent{Sections: secs}
}

func thesis(id, label string) SuggestedThesis { return SuggestedThesis{ID: id, Label: label} }

func TestMatchThesisSegments(t *testing.T) {
	tests := []struct {
		name   string
		theses []SuggestedThesis
		sc     *StructuredContent
		// wantByThesis maps thesis id → expected section heading (Title). Absence
		// means the thesis must NOT get a segment.
		wantByThesis map[string]string
	}{
		{
			name:         "exact match, accent/case folded",
			theses:       []SuggestedThesis{thesis("t1", "Extinção do processo por inércia do exequente")},
			sc:           sc("DA EXTINÇÃO DO PROCESSO POR INÉRCIA DO EXEQUENTE"),
			wantByThesis: map[string]string{"t1": "DA EXTINÇÃO DO PROCESSO POR INÉRCIA DO EXEQUENTE"},
		},
		{
			name: "each thesis to its own section (1:1, no double-assign)",
			theses: []SuggestedThesis{
				thesis("t1", "Extinção do processo por inércia"),
				thesis("t2", "Pesquisa de endereço do executado"),
			},
			sc: sc("DA EXTINÇÃO DO PROCESSO", "DA PESQUISA DE ENDEREÇO DO EXECUTADO", "DOS PEDIDOS"),
			wantByThesis: map[string]string{
				"t1": "DA EXTINÇÃO DO PROCESSO",
				"t2": "DA PESQUISA DE ENDEREÇO DO EXECUTADO",
			},
		},
		{
			name:         "below threshold → no segment",
			theses:       []SuggestedThesis{thesis("t1", "Pesquisa de endereço do executado")},
			sc:           sc("DOS PEDIDOS", "DAS PROVAS"),
			wantByThesis: map[string]string{}, // nothing matched
		},
		{
			name: "generic section not double-assigned to two theses",
			theses: []SuggestedThesis{
				thesis("t1", "Impugnação específica dos fatos"),
				thesis("t2", "Impugnação ao valor da causa"),
			},
			// One section covers both labels partially; only the best-covered thesis wins it.
			sc:           sc("DA IMPUGNAÇÃO ESPECÍFICA DOS FATOS"),
			wantByThesis: map[string]string{"t1": "DA IMPUGNAÇÃO ESPECÍFICA DOS FATOS"},
		},
		{
			name:         "empty theses",
			theses:       nil,
			sc:           sc("DOS FATOS"),
			wantByThesis: map[string]string{},
		},
		{
			name:         "nil structured content",
			theses:       []SuggestedThesis{thesis("t1", "Extinção do processo")},
			sc:           nil,
			wantByThesis: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchThesisSegments(tt.theses, tt.sc)

			gotByThesis := map[string]string{}
			for _, m := range got {
				// Heading may carry "ROMAN — Title"; our helper sections have no roman,
				// so Heading == Title. Assert on Title via the segment heading.
				gotByThesis[m.ThesisID] = m.Segment.Heading
				if m.Segment.Conteudo == "" {
					t.Errorf("thesis %s: empty conteudo", m.ThesisID)
				}
			}
			if !reflect.DeepEqual(gotByThesis, tt.wantByThesis) {
				t.Errorf("match mismatch:\n got=%v\nwant=%v", gotByThesis, tt.wantByThesis)
			}
		})
	}
}

// TestMatchThesisSegments_BodyMatch reproduces the real generation shape where the
// theses live as paragraphs under GENERIC profile headings (the label words appear
// only in the section body, not the title). The match must still find each thesis.
func TestMatchThesisSegments_BodyMatch(t *testing.T) {
	theses := []SuggestedThesis{
		thesis("t1", "Extinção do processo por inércia do exequente"),
		thesis("t2", "Necessidade de pesquisa de endereço do executado"),
		thesis("t3", "Intimação para credenciamento no sistema eproc"),
	}
	content := &StructuredContent{Sections: []StructuredSection{
		{Roman: "I", Title: "DAS PRELIMINARES", Paragraphs: []string{
			"Diante da inércia do exequente em promover o andamento do feito, impõe-se a extinção do processo sem resolução de mérito, art. 485, III, do CPC.",
		}},
		{Roman: "II", Title: "DA IMPUGNAÇÃO ESPECÍFICA DOS FATOS", Paragraphs: []string{
			"Não houve o cumprimento da providência de pesquisa de endereço do executado pelos sistemas conveniados.",
		}},
		{Roman: "III", Title: "DO MÉRITO", Paragraphs: []string{
			"A intimação para que a advogada proceda ao credenciamento no sistema eproc é formalidade que não interfere no mérito.",
		}},
		{Roman: "IV", Title: "DOS PEDIDOS", Paragraphs: []string{"Requer a procedência."}},
	}}

	got := matchThesisSegments(theses, content)
	byThesis := map[string]string{}
	for _, m := range got {
		byThesis[m.ThesisID] = m.Segment.Heading
	}
	want := map[string]string{
		"t1": "I — DAS PRELIMINARES",
		"t2": "II — DA IMPUGNAÇÃO ESPECÍFICA DOS FATOS",
		"t3": "III — DO MÉRITO",
	}
	if !reflect.DeepEqual(byThesis, want) {
		t.Errorf("body-match mismatch:\n got=%v\nwant=%v", byThesis, want)
	}
}

func TestMatchThesisSegments_PositionOrderedByThesis(t *testing.T) {
	// Sections discovered out of thesis order must still emit segments ordered by
	// thesis position with sequential Position values.
	theses := []SuggestedThesis{
		thesis("t1", "Extinção do processo"),
		thesis("t2", "Pesquisa de endereço do executado"),
	}
	content := sc("DA PESQUISA DE ENDEREÇO DO EXECUTADO", "DA EXTINÇÃO DO PROCESSO")
	got := matchThesisSegments(theses, content)

	if len(got) != 2 {
		t.Fatalf("want 2 segments, got %d", len(got))
	}
	if got[0].ThesisID != "t1" || got[1].ThesisID != "t2" {
		t.Errorf("segments not ordered by thesis position: got %s,%s", got[0].ThesisID, got[1].ThesisID)
	}
	if got[0].Segment.Position != 0 || got[1].Segment.Position != 1 {
		t.Errorf("positions not sequential: got %d,%d", got[0].Segment.Position, got[1].Segment.Position)
	}
}

func TestSectionHeading(t *testing.T) {
	tests := []struct {
		name string
		in   StructuredSection
		want string
	}{
		{"roman + title", StructuredSection{Roman: "I", Title: "DOS FATOS"}, "I — DOS FATOS"},
		{"title only", StructuredSection{Title: "PRELIMINARES"}, "PRELIMINARES"},
		{"roman only", StructuredSection{Roman: "II"}, "II"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sectionHeading(tt.in); got != tt.want {
				t.Errorf("sectionHeading() = %q, want %q", got, tt.want)
			}
		})
	}
}
