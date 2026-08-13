package acquisition

import (
	"context"
	"os"
	"testing"
	"time"
)

// fixedNow returns a parser whose observed_at clock is deterministic.
func newTestDATAJUDParser(now time.Time) *DATAJUDParser {
	return &DATAJUDParser{now: func() time.Time { return now }}
}

// TestDATAJUDParserParse maps the real captured ES document and checks the graded
// court record (grau revealed) and the movimentos → docket entries.
func TestDATAJUDParserParse(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("testdata/datajud_search.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	observed := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	parser := newTestDATAJUDParser(observed)
	res, err := parser.Parse(context.Background(), RawPayload{Source: SourceDATAJUD, Body: body})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(res.CourtRecords) != 1 {
		t.Fatalf("court records = %d, want 1", len(res.CourtRecords))
	}
	if len(res.Intimations) != 0 {
		t.Errorf("DATAJUD yields no intimations, got %d", len(res.Intimations))
	}
	cr := res.CourtRecords[0]
	if cr.Degree != "G1" {
		t.Errorf("degree = %q, want G1 (grau revealed)", cr.Degree)
	}
	if cr.Court != "TJRS" || cr.Class == "" || cr.Subject == "" || cr.JudgingBody == "" {
		t.Errorf("court record under-populated: court=%q class=%q subject=%q judging_body=%q", cr.Court, cr.Class, cr.Subject, cr.JudgingBody)
	}
	if cr.Secrecy != SecrecyPublic {
		t.Errorf("secrecy = %q, want PUBLIC (nivelSigilo 0)", cr.Secrecy)
	}
	wantFiled := time.Date(2016, 11, 25, 0, 0, 0, 0, time.UTC)
	if !cr.FiledAt.Equal(wantFiled) {
		t.Errorf("filed_at = %s, want %s", cr.FiledAt, wantFiled)
	}

	if len(res.DocketEntries) == 0 {
		t.Fatal("no docket entries from movimentos")
	}
	for _, de := range res.DocketEntries {
		if de.Source != SourceDATAJUD || de.Degree != "G1" {
			t.Errorf("docket %q: source=%q degree=%q, want DATAJUD/G1", de.Hash, de.Source, de.Degree)
		}
		if len(de.Hash) != 64 { // sha256 hex
			t.Errorf("docket hash %q is not a sha256 hex", de.Hash)
		}
		if !de.ObservedAt.Equal(observed) {
			t.Errorf("docket observed_at = %s, want the parser clock %s", de.ObservedAt, observed)
		}
		if de.OccurredAt.IsZero() || de.Text == "" || de.TPUCode == 0 {
			t.Errorf("docket %q under-populated: occurred=%s text=%q tpu=%d", de.Hash, de.OccurredAt, de.Text, de.TPUCode)
		}
	}
}

// TestDATAJUDParserHashStable proves the derived movimento hash is deterministic
// (so a re-fetch dedups) and empty hits map to an empty result.
func TestDATAJUDParserHashStable(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("testdata/datajud_search.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	p := newTestDATAJUDParser(time.Unix(0, 0))
	a, _ := p.Parse(context.Background(), RawPayload{Source: SourceDATAJUD, Body: body})
	b, _ := p.Parse(context.Background(), RawPayload{Source: SourceDATAJUD, Body: body})
	if len(a.DocketEntries) == 0 || len(a.DocketEntries) != len(b.DocketEntries) {
		t.Fatalf("docket counts differ: %d vs %d", len(a.DocketEntries), len(b.DocketEntries))
	}
	for i := range a.DocketEntries {
		if a.DocketEntries[i].Hash != b.DocketEntries[i].Hash {
			t.Errorf("hash %d not stable: %q vs %q", i, a.DocketEntries[i].Hash, b.DocketEntries[i].Hash)
		}
	}

	empty, err := p.Parse(context.Background(), RawPayload{Source: SourceDATAJUD, Body: []byte(`{"hits":{"hits":[]}}`)})
	if err != nil {
		t.Fatalf("Parse empty: %v", err)
	}
	if len(empty.CourtRecords) != 0 || len(empty.DocketEntries) != 0 {
		t.Errorf("empty hits should yield empty result, got %d records / %d docket", len(empty.CourtRecords), len(empty.DocketEntries))
	}
}

// TestDATAJUDMovimentoHashGradeIndependent proves #4: the movimento hash no longer
// depends on the grau, so the SAME movimento seen at different grades (G1 vs G2)
// derives the SAME hash — with FIX B's single record per CNJ, a grade reveal/change
// must not re-duplicate an andamento. It still distinguishes a different movimento.
func TestDATAJUDMovimentoHashGradeIndependent(t *testing.T) {
	t.Parallel()

	body := func(grau string) []byte {
		return []byte(`{"hits":{"hits":[{"_source":{` +
			`"numeroProcesso":"50007978720168210156","tribunal":"TJRS","grau":"` + grau + `",` +
			`"movimentos":[{"codigo":123,"nome":"Juntada de Petição","dataHora":"2026-01-02T10:00:00Z"}]}}]}}`)
	}
	p := newTestDATAJUDParser(time.Unix(0, 0))

	g1, err := p.Parse(context.Background(), RawPayload{Source: SourceDATAJUD, Body: body("G1")})
	if err != nil {
		t.Fatalf("Parse G1: %v", err)
	}
	g2, err := p.Parse(context.Background(), RawPayload{Source: SourceDATAJUD, Body: body("G2")})
	if err != nil {
		t.Fatalf("Parse G2: %v", err)
	}
	if len(g1.DocketEntries) != 1 || len(g2.DocketEntries) != 1 {
		t.Fatalf("want one movimento each, got G1=%d G2=%d", len(g1.DocketEntries), len(g2.DocketEntries))
	}
	// The records grade differently, but the movimento hash is identical.
	if g1.CourtRecords[0].Degree != "G1" || g2.CourtRecords[0].Degree != "G2" {
		t.Fatalf("record grades not distinct: G1=%q G2=%q", g1.CourtRecords[0].Degree, g2.CourtRecords[0].Degree)
	}
	if g1.DocketEntries[0].Hash != g2.DocketEntries[0].Hash {
		t.Errorf("grade changed the movimento hash: %q (G1) vs %q (G2)", g1.DocketEntries[0].Hash, g2.DocketEntries[0].Hash)
	}

	// A genuinely different movimento (different codigo) must still hash differently.
	mov := datajudMovimento{Codigo: 123, Nome: "Juntada de Petição", DataHora: "2026-01-02T10:00:00Z"}
	other := datajudMovimento{Codigo: 456, Nome: "Conclusão", DataHora: "2026-01-02T10:00:00Z"}
	if datajudMovimentoHash("TJRS", "50007978720168210156", mov) == datajudMovimentoHash("TJRS", "50007978720168210156", other) {
		t.Error("distinct movimentos collided to the same hash")
	}
}

func TestSecrecyFromNivel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		nivel int
		want  string
	}{
		{0, SecrecyPublic},
		{1, SecrecyRestricted},
		{4, SecrecyRestricted},
		{5, SecrecySecret},
	}
	for _, tt := range tests {
		if got := secrecyFromNivel(tt.nivel); got != tt.want {
			t.Errorf("secrecyFromNivel(%d) = %q, want %q", tt.nivel, got, tt.want)
		}
	}
}
