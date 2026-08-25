package acquisition

import (
	"context"
	"encoding/json"
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

// TestDATAJUDParserParseBatch maps a MULTI-hit, MULTI-page batch payload into a
// numeroProcesso→process map: every graded hit is keyed by its number; a hit with an empty
// grau is skipped (nothing to grade); a number never returned is simply absent.
func TestDATAJUDParserParseBatch(t *testing.T) {
	t.Parallel()

	observed := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	parser := newTestDATAJUDParser(observed)

	page1 := RawPayload{Source: SourceDATAJUD, Body: []byte(`{"hits":{"hits":[
		{"_source":{"numeroProcesso":"111","grau":"G1","tribunal":"TJSP","movimentos":[{"codigo":26,"nome":"Ato","dataHora":"2024-01-10T12:00:00.000Z"}]}},
		{"_source":{"numeroProcesso":"222","grau":"","tribunal":"TJSP"}}
	]}}`)}
	page2 := RawPayload{Source: SourceDATAJUD, Body: []byte(`{"hits":{"hits":[
		{"_source":{"numeroProcesso":"333","grau":"G2","tribunal":"TJSP"}}
	]}}`)}

	byNumber, skipped, err := parser.ParseBatch(context.Background(), []RawPayload{page1, page2})
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped (real parse errors) = %v, want none (222 empty grau is benign, not a skip)", skipped)
	}
	if len(byNumber) != 2 {
		t.Fatalf("map size = %d, want 2 (111 graded, 333 graded; 222 skipped for empty grau)", len(byNumber))
	}
	if _, ok := byNumber["222"]; ok {
		t.Error("222 has an empty grau and must be absent from the map")
	}
	p111 := byNumber["111"]
	if p111.Record.Degree != "G1" || len(p111.Movimentos) != 1 {
		t.Errorf("111 = degree %q, %d movimentos; want G1, 1", p111.Record.Degree, len(p111.Movimentos))
	}
	if p111.Movimentos[0].ObservedAt != observed {
		t.Errorf("111 movimento observed_at = %s, want the parser clock %s", p111.Movimentos[0].ObservedAt, observed)
	}
	if byNumber["333"].Record.Degree != "G2" {
		t.Errorf("333 degree = %q, want G2", byNumber["333"].Record.Degree)
	}
}

// TestDATAJUDParserParseBatch_NestedAssuntosDoesNotBreakPage proves the QA-found bug is
// fixed: a real DATAJUD page where ONE hit carries a nested-array `assuntos`
// (00012991920158260153, TJSP) no longer fails the whole page. ParseBatch returns NO error;
// the malformed hit is TOLERATED (its nested assuntos flattened) and the other 3 hits parse.
func TestDATAJUDParserParseBatch_NestedAssuntosDoesNotBreakPage(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("testdata/datajud_batch_nested_assuntos.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	byNumber, skipped, err := newTestDATAJUDParser(time.Now()).ParseBatch(
		context.Background(), []RawPayload{{Source: SourceDATAJUD, Body: body}})
	if err != nil {
		t.Fatalf("ParseBatch must not error on a page with one nested-assuntos hit: %v", err)
	}
	// The nested assuntos is FLATTENED (not skipped), so there are ZERO real parse errors →
	// the fecho of an import like this must be OK, not PARTIAL.
	if len(skipped) != 0 {
		t.Errorf("skipped (real parse errors) = %v, want none (nested assuntos is flattened, not a skip)", skipped)
	}

	// The three normal hits must be present, and the nested-assuntos process must NOT have
	// taken the page down with it.
	want := []string{"00001974620238260196", "00002084120248260196", "00003631020258260196"}
	for _, num := range want {
		if _, ok := byNumber[num]; !ok {
			t.Errorf("normal hit %s missing from ParseBatch result", num)
		}
	}
	// The nested-assuntos process is tolerated and PARSED (its nested assuntos flattened).
	if proc, ok := byNumber["00012991920158260153"]; ok {
		if proc.Record.Degree == "" {
			t.Errorf("nested-assuntos hit parsed but degree empty")
		}
	}
	if len(byNumber) < 3 {
		t.Errorf("ParseBatch returned %d processes, want at least the 3 normal hits", len(byNumber))
	}
}

// TestDATAJUDParser_NestedAssuntosFlattened proves the lenient assuntos unmarshaler flattens
// a nested array and ignores non-object elements, exposing the first valid subject.
func TestDATAJUDParser_NestedAssuntosFlattened(t *testing.T) {
	t.Parallel()

	// Mixed shape: an object, then a NESTED ARRAY of objects, then a bare string (ignored).
	src := []byte(`{"assuntos":[{"codigo":1,"nome":"Primeiro"},[{"codigo":2,"nome":"Aninhado"}],"lixo"]}`)
	var got datajudSource
	if err := json.Unmarshal(src, &got); err != nil {
		t.Fatalf("mixed-shape assuntos must not fail unmarshal: %v", err)
	}
	if len(got.Assuntos) != 2 {
		t.Fatalf("assuntos = %d, want 2 (object + flattened nested; bare string ignored)", len(got.Assuntos))
	}
	if firstAssunto(got.Assuntos) != "Primeiro" {
		t.Errorf("firstAssunto = %q, want Primeiro", firstAssunto(got.Assuntos))
	}
}

// TestDATAJUDParser_PerHitResilience proves a page with one hit whose _source is structurally
// bad (a type clash on a scalar field) skips that hit, REPORTS it in `skipped` (a real parse
// error), and still returns the good ones with no fatal error.
func TestDATAJUDParser_PerHitResilience(t *testing.T) {
	t.Parallel()

	// hit 1 valid; hit 2 has nivelSigilo as an object (type clash) → parseHit fails → skip.
	body := []byte(`{"hits":{"hits":[
		{"_source":{"numeroProcesso":"good","grau":"G1","tribunal":"TJSP"}},
		{"_source":{"numeroProcesso":"bad","grau":"G1","nivelSigilo":{"x":1}}}
	]}}`)
	byNumber, skipped, err := newTestDATAJUDParser(time.Now()).ParseBatch(
		context.Background(), []RawPayload{{Source: SourceDATAJUD, Body: body}})
	if err != nil {
		t.Fatalf("one bad hit must not fail the page: %v", err)
	}
	if _, ok := byNumber["good"]; !ok {
		t.Error("good hit missing")
	}
	if _, ok := byNumber["bad"]; ok {
		t.Error("structurally-bad hit must be skipped, not included")
	}
	// The bad hit is a REAL parse error → reported in skipped (drives the fecho to PARTIAL).
	if len(skipped) != 1 || skipped[0] != "bad" {
		t.Errorf("skipped = %v, want exactly [bad] (the unparseable hit)", skipped)
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
