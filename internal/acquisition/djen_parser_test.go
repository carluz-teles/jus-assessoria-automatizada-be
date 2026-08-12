package acquisition

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// fakeCalendar advances one calendar day per call, deterministically — enough to
// assert the parser derives published_at/deadline_start_at THROUGH the calendar
// without a real HolidaySource.
type fakeCalendar struct{}

func (fakeCalendar) NextBusinessDay(_ context.Context, day time.Time, _, _ string) (time.Time, error) {
	return day.AddDate(0, 0, 1), nil
}

// djenRawFromFixture rebuilds the connector's djenPayload from the real captured
// Comunica API page (testdata) plus the tenant's watched OABs, so the parser sees
// exactly what the connector would emit.
func djenRawFromFixture(t *testing.T, watched []OABEntry) RawPayload {
	t.Helper()

	b, err := os.ReadFile("testdata/djen_comunicacao_page.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var env djenResponse
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal fixture envelope: %v", err)
	}
	if len(env.Items) == 0 {
		t.Fatal("fixture has no items")
	}
	body, err := json.Marshal(djenPayload{OABs: watched, Items: env.Items})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return RawPayload{Source: SourceDJEN, ConnectorID: djenConnectorID, Body: body}
}

// TestDJENParserParse maps the real captured page (queried by OAB 41101/RS) and
// checks the end-to-end contract: UNKNOWN degree, DJEN source, calendar-derived
// dates, deduped court records, and the matched-OAB flag on recipients.
func TestDJENParserParse(t *testing.T) {
	t.Parallel()

	watched := []OABEntry{{Number: "41101", UF: "RS"}}
	raw := djenRawFromFixture(t, watched)

	parser := NewDJENParser(fakeCalendar{})
	res, err := parser.Parse(context.Background(), raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(res.Intimations) == 0 {
		t.Fatal("no intimations parsed")
	}
	if len(res.DocketEntries) != 0 {
		t.Errorf("DJEN yields no docket entries, got %d", len(res.DocketEntries))
	}
	if len(res.CourtRecords) == 0 || len(res.CourtRecords) > len(res.Intimations) {
		t.Errorf("court records = %d, want between 1 and %d (deduped by CNJ)", len(res.CourtRecords), len(res.Intimations))
	}

	for _, cr := range res.CourtRecords {
		if cr.Degree != DegreeUnknown {
			t.Errorf("court record %s degree = %q, want UNKNOWN", cr.CNJNumber, cr.Degree)
		}
		if cr.Court == "" || cr.JudgingBody == "" {
			t.Errorf("court record %s missing court/judging_body: court=%q judging_body=%q", cr.CNJNumber, cr.Court, cr.JudgingBody)
		}
	}

	sawMatched := false
	for _, in := range res.Intimations {
		if in.Degree != DegreeUnknown || in.Source != SourceDJEN {
			t.Errorf("intimation %s: degree=%q source=%q, want UNKNOWN/DJEN", in.Hash, in.Degree, in.Source)
		}
		if in.Hash == "" || in.CNJNumber == "" || in.Content == "" {
			t.Errorf("intimation %s: missing hash/cnj/content", in.Hash)
		}
		if in.Type != IntimationTypeIntimacao && in.Type != IntimationTypeCitacao && in.Type != IntimationTypeComunicacao {
			t.Errorf("intimation %s: type %q not in enum", in.Hash, in.Type)
		}
		// published = availability + 1 business day; deadline = published + 1 (fake cal).
		if !in.PublishedAt.Equal(in.MadeAvailableAt.AddDate(0, 0, 1)) {
			t.Errorf("intimation %s: published_at %s not availability+1 (%s)", in.Hash, in.PublishedAt, in.MadeAvailableAt)
		}
		if !in.DeadlineStartAt.Equal(in.PublishedAt.AddDate(0, 0, 1)) {
			t.Errorf("intimation %s: deadline_start_at %s not published+1 (%s)", in.Hash, in.DeadlineStartAt, in.PublishedAt)
		}

		var recips []djenRecipient
		if err := json.Unmarshal(in.Recipients, &recips); err != nil {
			t.Fatalf("intimation %s: recipients not a JSON array: %v", in.Hash, err)
		}
		for _, r := range recips {
			isWatched := r.OABNumber == "41101" && r.OABUF == "RS"
			if r.Matched != isWatched {
				t.Errorf("intimation %s: recipient %s/%s matched=%v, want %v", in.Hash, r.OABNumber, r.OABUF, r.Matched, isWatched)
			}
			if r.Matched {
				sawMatched = true
			}
		}
	}
	if !sawMatched {
		t.Error("no recipient flagged matched, but the page was queried by a watched OAB")
	}
}

// TestDJENParserCancellation covers the retraction path with a synthetic item
// (live cancelled items are rare): a cancelled communication maps to status
// CANCELLED with its date and reason preserved.
func TestDJENParserCancellation(t *testing.T) {
	t.Parallel()

	item := `{
		"hash":"cancelled-1","numero_processo":"5000001","tipoComunicacao":"Intimação",
		"nomeOrgao":"1ª Vara","siglaTribunal":"TJSP","nomeClasse":"Cumprimento",
		"texto":"...","link":"https://x/y","data_disponibilizacao":"2024-01-10",
		"ativo":false,"data_cancelamento":"2024-01-12","motivo_cancelamento":"retificação",
		"destinatarioadvogados":[]
	}`
	body, err := json.Marshal(djenPayload{
		OABs:  []OABEntry{{Number: "1", UF: "SP"}},
		Items: []json.RawMessage{json.RawMessage(item)},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	res, err := NewDJENParser(fakeCalendar{}).Parse(context.Background(), RawPayload{Source: SourceDJEN, Body: body})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Intimations) != 1 {
		t.Fatalf("intimations = %d, want 1", len(res.Intimations))
	}
	in := res.Intimations[0]
	if in.Status != IntimationStatusCancelled {
		t.Errorf("status = %q, want CANCELLED", in.Status)
	}
	if in.CancelReason != "retificação" {
		t.Errorf("cancel_reason = %q, want retificação", in.CancelReason)
	}
	want := time.Date(2024, 1, 12, 0, 0, 0, 0, time.UTC)
	if !in.CancelledAt.Equal(want) {
		t.Errorf("cancelled_at = %s, want %s", in.CancelledAt, want)
	}
	if in.SourceURL != "https://x/y" {
		t.Errorf("source_url = %q, want the link", in.SourceURL)
	}
}

func TestDJENType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"Intimação", IntimationTypeIntimacao},
		{"INTIMACAO", IntimationTypeIntimacao},
		{"Citação", IntimationTypeCitacao},
		{"Edital", IntimationTypeComunicacao},
		{"Comunicação", IntimationTypeComunicacao},
		{"", IntimationTypeComunicacao},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := djenType(tt.in); got != tt.want {
				t.Errorf("djenType(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestUFFromTribunal moved to pkg/tribunal (TestUF) with the registry relocation —
// UF derivation is reference data shared by acquisition and deadline, so it lives in
// /pkg now (Regra nº1: one source of truth, two consumers).
