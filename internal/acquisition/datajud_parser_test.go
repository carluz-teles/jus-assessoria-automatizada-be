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

// TestLifecycleFromMovimentos verifies the conservative lifecycle derivation from
// DATAJUD movimentos: terminal code (22) → ARCHIVED, suspension code (25) as
// the latest → SUSPENDED, no signal → ACTIVE, and an empty list → ACTIVE.
// The SUPERSEDED guard (never-downgrade) lives in the SQL layer (UpdateCourtRecordGrade
// CASE) and is NOT covered by a unit test — known-limitation, needs an integration test
// against real Postgres. This function's output (the Lifecycle value) is validated here.
func TestLifecycleFromMovimentos(t *testing.T) {
	t.Parallel()

	mov := func(codigo int, dataHora string) datajudMovimento {
		return datajudMovimento{Codigo: codigo, Nome: "test", DataHora: dataHora}
	}

	tests := []struct {
		name string
		movs []datajudMovimento
		want string
	}{
		{
			name: "empty list → ACTIVE (safe default)",
			movs: nil,
			want: LifecycleActive,
		},
		{
			name: "only unknown codes → ACTIVE",
			movs: []datajudMovimento{
				mov(123, "2026-01-10T00:00:00Z"),
				mov(456, "2026-01-09T00:00:00Z"),
			},
			want: LifecycleActive,
		},
		{
			name: "terminal code 22 as the only movement → ARCHIVED",
			movs: []datajudMovimento{
				mov(22, "2026-03-01T00:00:00Z"),
			},
			want: LifecycleArchived,
		},
		{
			name: "terminal code 22 present but not most recent → ARCHIVED (terminal wins)",
			movs: []datajudMovimento{
				mov(123, "2026-05-01T00:00:00Z"), // more recent procedural step
				mov(22, "2026-01-01T00:00:00Z"),  // terminal
			},
			want: LifecycleArchived,
		},
		{
			name: "suspension code 25 is the most recent movement → SUSPENDED",
			movs: []datajudMovimento{
				mov(25, "2026-04-01T00:00:00Z"), // suspension — most recent
				mov(123, "2026-01-01T00:00:00Z"),
			},
			want: LifecycleSuspended,
		},
		{
			name: "suspension code 25 not the most recent movement → ACTIVE (process resumed)",
			movs: []datajudMovimento{
				mov(123, "2026-05-01T00:00:00Z"), // later procedural step = process resumed
				mov(25, "2026-01-01T00:00:00Z"),  // suspension — but older
			},
			want: LifecycleActive,
		},
		{
			name: "terminal code beats suspension (terminal is sticky)",
			movs: []datajudMovimento{
				mov(25, "2026-05-01T00:00:00Z"), // suspension — most recent
				mov(22, "2026-01-01T00:00:00Z"), // terminal — older but sticky
			},
			want: LifecycleArchived,
		},
		{
			name: "terminal code 196 (extinção da execução) → ARCHIVED",
			movs: []datajudMovimento{
				mov(196, "2026-03-01T00:00:00Z"),
			},
			want: LifecycleArchived,
		},
		{
			name: "suspension code 12065 as most recent → SUSPENDED",
			movs: []datajudMovimento{
				mov(12065, "2026-05-01T00:00:00Z"),
				mov(123, "2026-01-01T00:00:00Z"),
			},
			want: LifecycleSuspended,
		},
		{
			name: "desarquivamento (893) after baixa (22) → ACTIVE (reopened)",
			movs: []datajudMovimento{
				mov(893, "2026-06-01T00:00:00Z"), // reactivation — most recent
				mov(22, "2026-01-01T00:00:00Z"),  // terminal — older, reversed
			},
			want: LifecycleActive,
		},
		{
			name: "levantamento da suspensão (12066) after suspensão (25) → ACTIVE",
			movs: []datajudMovimento{
				mov(12066, "2026-06-01T00:00:00Z"), // reactivation — most recent
				mov(25, "2026-01-01T00:00:00Z"),    // suspension — older, lifted
			},
			want: LifecycleActive,
		},
		{
			name: "unparseable dataHora is skipped; remaining signal respected",
			movs: []datajudMovimento{
				mov(22, "not-a-date"),            // skipped
				mov(123, "2026-01-01T00:00:00Z"), // only parseable: ACTIVE
			},
			want: LifecycleActive,
		},
		{
			name: "all unparseable dataHora → ACTIVE (empty valid list)",
			movs: []datajudMovimento{
				mov(22, "bad"),
				mov(25, "also-bad"),
			},
			want: LifecycleActive,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := lifecycleFromMovimentos(tt.movs)
			if got != tt.want {
				t.Errorf("lifecycleFromMovimentos(...) = %q, want %q", got, tt.want)
			}
		})
	}
}
