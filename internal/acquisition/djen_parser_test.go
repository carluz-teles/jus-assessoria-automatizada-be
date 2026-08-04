package acquisition

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/calendar"
)

// spySource is an in-memory calendar.HolidaySource: it drives the business-day
// math without a database AND records every (uf, court) it is asked about, so a
// test can assert which UF the parser derived from a siglaTribunal. Keys in
// national are YYYY-MM-DD.
type spySource struct {
	national  map[string]bool
	seenUF    []string
	seenCourt []string
}

func (s *spySource) IsHoliday(_ context.Context, day time.Time, uf, court string) (bool, error) {
	s.seenUF = append(s.seenUF, uf)
	s.seenCourt = append(s.seenCourt, court)
	return s.national[day.Format(time.DateOnly)], nil
}

// wire mirrors the DJEN envelope on the wire, kept independent of the parser's
// own DTOs so a json-tag drift in the parser is caught here rather than hidden.
type wireEnvelope struct {
	Scope wireScope  `json:"scope"`
	Items []wireItem `json:"items"`
}

type wireScope struct {
	OABs []wireOAB `json:"oabs"`
}

type wireOAB struct {
	Number string `json:"number"`
	UF     string `json:"uf"`
}

type wireItem struct {
	Hash                  string         `json:"hash"`
	NumeroProcesso        string         `json:"numero_processo"`
	SiglaTribunal         string         `json:"siglaTribunal"`
	NomeClasse            string         `json:"nomeClasse"`
	NomeOrgao             string         `json:"nomeOrgao"`
	TipoComunicacao       string         `json:"tipoComunicacao"`
	Texto                 string         `json:"texto"`
	Link                  string         `json:"link"`
	DataDisponibilizacao  string         `json:"data_disponibilizacao"`
	DataCancelamento      *string        `json:"data_cancelamento"`
	MotivoCancelamento    *string        `json:"motivo_cancelamento"`
	Destinatarios         []wireParty    `json:"destinatarios"`
	DestinatarioAdvogados []wireLawyerAt `json:"destinatarioadvogados"`
}

type wireParty struct {
	Nome string `json:"nome"`
	Polo string `json:"polo"`
}

type wireLawyerAt struct {
	Advogado wireLawyer `json:"advogado"`
}

type wireLawyer struct {
	Nome      string `json:"nome"`
	NumeroOAB string `json:"numero_oab"`
	UfOAB     string `json:"uf_oab"`
}

// baseItem is a valid, minimal comunicação tests tweak per scenario.
func baseItem() wireItem {
	return wireItem{
		Hash:                 "djen-hash-abc123",
		NumeroProcesso:       "00000010120268260100",
		SiglaTribunal:        "TJSP",
		NomeClasse:           "Procedimento Comum Cível",
		NomeOrgao:            "1ª Vara Cível de São Paulo",
		TipoComunicacao:      "Intimação",
		Texto:                "<p>Fica a parte intimada.</p>",
		Link:                 "https://comunica.pje.jus.br/consulta/abc123",
		DataDisponibilizacao: "2026-08-03", // a Monday
		Destinatarios: []wireParty{
			{Nome: "FULANO DE TAL", Polo: "A"},
		},
		DestinatarioAdvogados: []wireLawyerAt{
			{Advogado: wireLawyer{Nome: "ADV UM", NumeroOAB: "123456", UfOAB: "SP"}},
		},
	}
}

// payload marshals an envelope of the given items (scope defaults to the base
// item's own OAB) into a DJEN RawPayload.
func payload(t *testing.T, scope []wireOAB, items ...wireItem) RawPayload {
	t.Helper()
	body, err := json.Marshal(wireEnvelope{Scope: wireScope{OABs: scope}, Items: items})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return RawPayload{Source: SourceDJEN, Body: body}
}

// newParser builds a parser over a spy source seeded with the given national
// holidays (YYYY-MM-DD).
func newParser(holidays ...string) (*DJENParser, *spySource) {
	nat := make(map[string]bool, len(holidays))
	for _, h := range holidays {
		nat[h] = true
	}
	spy := &spySource{national: nat}
	return NewDJENParser(calendar.New(spy)), spy
}

// dateUTC is a YYYY-MM-DD civil date at midnight UTC.
func dateUTC(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// parseOne parses a single-item envelope and returns the resulting record and
// intimation, failing on any parse error.
func parseOne(t *testing.T, p *DJENParser, scope []wireOAB, item wireItem) (ParsedCourtRecord, ParsedIntimation) {
	t.Helper()
	res, err := p.Parse(payload(t, scope, item))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(res.CourtRecords) != 1 || len(res.Intimations) != 1 {
		t.Fatalf("want 1 record and 1 intimation, got %d records, %d intimations",
			len(res.CourtRecords), len(res.Intimations))
	}
	return res.CourtRecords[0], res.Intimations[0]
}

func TestDJENParser_CanParse(t *testing.T) {
	t.Parallel()
	p, _ := newParser()

	if !p.CanParse(RawPayload{Source: SourceDJEN}) {
		t.Error("CanParse(DJEN) = false, want true")
	}
	if p.CanParse(RawPayload{Source: SourceDATAJUD}) {
		t.Error("CanParse(DATAJUD) = true, want false")
	}
}

// C1/C2/C3 — the three deadline dates derived through lib/calendar.
func TestDJENParser_Parse_DeadlineDates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		madeAvailable     string
		wantPublished     string
		wantDeadlineStart string
	}{
		{
			// C1: disponibilização on a business day (Mon) → published next day.
			name:              "dia util avanca um dia (C1)",
			madeAvailable:     "2026-08-03", // Monday
			wantPublished:     "2026-08-04", // Tuesday
			wantDeadlineStart: "2026-08-05", // Wednesday (C3)
		},
		{
			// C2: disponibilização on a Friday → published skips the weekend.
			name:              "sexta pula o fim de semana (C2)",
			madeAvailable:     "2026-08-07", // Friday
			wantPublished:     "2026-08-10", // Monday
			wantDeadlineStart: "2026-08-11", // Tuesday (C3)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, _ := newParser()

			item := baseItem()
			item.DataDisponibilizacao = tt.madeAvailable
			_, intimation := parseOne(t, p, nil, item)

			if !intimation.MadeAvailableAt.Equal(dateUTC(tt.madeAvailable)) {
				t.Errorf("MadeAvailableAt = %s, want %s",
					intimation.MadeAvailableAt.Format(time.DateOnly), tt.madeAvailable)
			}
			if !intimation.PublishedAt.Equal(dateUTC(tt.wantPublished)) {
				t.Errorf("PublishedAt = %s, want %s",
					intimation.PublishedAt.Format(time.DateOnly), tt.wantPublished)
			}
			if !intimation.DeadlineStartAt.Equal(dateUTC(tt.wantDeadlineStart)) {
				t.Errorf("DeadlineStartAt = %s, want %s",
					intimation.DeadlineStartAt.Format(time.DateOnly), tt.wantDeadlineStart)
			}
		})
	}
}

// C4/C5 — cancellation status derived from data_cancelamento.
func TestDJENParser_Parse_Cancellation(t *testing.T) {
	t.Parallel()

	t.Run("com data_cancelamento vira CANCELLED (C4)", func(t *testing.T) {
		t.Parallel()
		p, _ := newParser()

		cancelledOn := "2026-08-05"
		reason := "Comunicação cancelada por equívoco"
		item := baseItem()
		item.DataCancelamento = &cancelledOn
		item.MotivoCancelamento = &reason

		_, intimation := parseOne(t, p, nil, item)

		if intimation.Status != IntimationStatusCancelled {
			t.Errorf("Status = %q, want %q", intimation.Status, IntimationStatusCancelled)
		}
		if !intimation.CancelledAt.Equal(dateUTC(cancelledOn)) {
			t.Errorf("CancelledAt = %s, want %s", intimation.CancelledAt.Format(time.DateOnly), cancelledOn)
		}
		if intimation.CancelReason != reason {
			t.Errorf("CancelReason = %q, want %q", intimation.CancelReason, reason)
		}
	})

	t.Run("sem data_cancelamento fica ACTIVE (C5)", func(t *testing.T) {
		t.Parallel()
		p, _ := newParser()

		_, intimation := parseOne(t, p, nil, baseItem())

		if intimation.Status != IntimationStatusActive {
			t.Errorf("Status = %q, want %q", intimation.Status, IntimationStatusActive)
		}
		if !intimation.CancelledAt.IsZero() {
			t.Errorf("CancelledAt = %s, want zero", intimation.CancelledAt)
		}
		if intimation.CancelReason != "" {
			t.Errorf("CancelReason = %q, want empty", intimation.CancelReason)
		}
	})
}

// C6/C7/C8 — tipoComunicacao mapping, unknown kind never errors.
func TestDJENParser_Parse_Type(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tipo string
		want string
	}{
		{"intimacao (C6)", "Intimação", IntimationTypeIntimacao},
		{"citacao (C7)", "Citação", IntimationTypeCitacao},
		{"desconhecido vira comunicacao (C8)", "Edital de Leilão", IntimationTypeComunicacao},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, _ := newParser()

			item := baseItem()
			item.TipoComunicacao = tt.tipo
			_, intimation := parseOne(t, p, nil, item)

			if intimation.Type != tt.want {
				t.Errorf("Type = %q, want %q", intimation.Type, tt.want)
			}
		})
	}
}

// C9/C10/C11 — hash carried verbatim, CNJ + UNKNOWN degree, judging body.
func TestDJENParser_Parse_RecordAndIntimationFields(t *testing.T) {
	t.Parallel()
	p, _ := newParser()

	item := baseItem()
	record, intimation := parseOne(t, p, nil, item)

	// C9: hash is the DJEN field, not derived.
	if intimation.Hash != item.Hash {
		t.Errorf("Hash = %q, want %q (verbatim DJEN hash)", intimation.Hash, item.Hash)
	}

	// C10: CNJ from numero_processo; degree UNKNOWN on both record and intimation.
	if record.CNJNumber != item.NumeroProcesso || intimation.CNJNumber != item.NumeroProcesso {
		t.Errorf("CNJNumber = (%q, %q), want %q",
			record.CNJNumber, intimation.CNJNumber, item.NumeroProcesso)
	}
	if record.Degree != DegreeUnknown || intimation.Degree != DegreeUnknown {
		t.Errorf("Degree = (%q, %q), want %q", record.Degree, intimation.Degree, DegreeUnknown)
	}

	// C11: judging body from nomeOrgao; other record fields mapped.
	if record.JudgingBody != item.NomeOrgao {
		t.Errorf("JudgingBody = %q, want %q", record.JudgingBody, item.NomeOrgao)
	}
	if record.Court != item.SiglaTribunal {
		t.Errorf("Court = %q, want %q", record.Court, item.SiglaTribunal)
	}
	if record.Class != item.NomeClasse {
		t.Errorf("Class = %q, want %q", record.Class, item.NomeClasse)
	}
	if record.Completeness != djenCompleteness {
		t.Errorf("Completeness = %v, want %v", record.Completeness, djenCompleteness)
	}

	// Content is the raw HTML; source and source URL from the DJEN item.
	if intimation.Content != item.Texto {
		t.Errorf("Content = %q, want %q", intimation.Content, item.Texto)
	}
	if intimation.Source != SourceDJEN {
		t.Errorf("Source = %q, want %q", intimation.Source, SourceDJEN)
	}
	if intimation.SourceURL != item.Link {
		t.Errorf("SourceURL = %q, want %q", intimation.SourceURL, item.Link)
	}
}

// Recipients — every addressee flattened, matched flag on the in-scope lawyer.
func TestDJENParser_Parse_Recipients(t *testing.T) {
	t.Parallel()
	p, _ := newParser()

	item := baseItem()
	item.Destinatarios = []wireParty{
		{Nome: "AUTORA LTDA", Polo: "A"},
		{Nome: "RÉU S/A", Polo: "P"},
	}
	item.DestinatarioAdvogados = []wireLawyerAt{
		{Advogado: wireLawyer{Nome: "ADV NO ESCOPO", NumeroOAB: "123456", UfOAB: "SP"}},
		{Advogado: wireLawyer{Nome: "ADV FORA", NumeroOAB: "999999", UfOAB: "RJ"}},
	}
	// Scope carries only the first lawyer's OAB.
	scope := []wireOAB{{Number: "123456", UF: "SP"}}

	_, intimation := parseOne(t, p, scope, item)

	var got []djenRecipient
	if err := json.Unmarshal(intimation.Recipients, &got); err != nil {
		t.Fatalf("unmarshal recipients: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("recipients len = %d, want 4 (2 parties + 2 lawyers)", len(got))
	}

	// Parties first, then lawyers, in source order. Parties never matched.
	if got[0].Kind != recipientKindParty || got[0].Name != "AUTORA LTDA" || got[0].Polo != "A" || got[0].Matched {
		t.Errorf("recipient[0] = %+v, want unmatched party AUTORA LTDA polo A", got[0])
	}
	if got[1].Kind != recipientKindParty || got[1].Matched {
		t.Errorf("recipient[1] = %+v, want unmatched party", got[1])
	}

	// The in-scope lawyer is matched; the out-of-scope one is not.
	inScope := got[2]
	if inScope.Kind != recipientKindLawyer || inScope.OABNumber != "123456" || inScope.OABUF != "SP" || !inScope.Matched {
		t.Errorf("recipient[2] = %+v, want matched lawyer OAB 123456/SP", inScope)
	}
	outScope := got[3]
	if outScope.Kind != recipientKindLawyer || outScope.Matched {
		t.Errorf("recipient[3] = %+v, want unmatched lawyer", outScope)
	}
}

// Recipients must serialise to an empty array, never null, when there are none.
func TestDJENParser_Parse_Recipients_EmptyNotNull(t *testing.T) {
	t.Parallel()
	p, _ := newParser()

	item := baseItem()
	item.Destinatarios = nil
	item.DestinatarioAdvogados = nil

	_, intimation := parseOne(t, p, nil, item)

	if string(intimation.Recipients) != "[]" {
		t.Errorf("Recipients = %s, want []", string(intimation.Recipients))
	}
}

// C12 — a known sigla resolves its UF and passes it to the calendar.
func TestDJENParser_Parse_KnownSiglaPassesUF(t *testing.T) {
	t.Parallel()
	p, spy := newParser()

	item := baseItem()
	item.SiglaTribunal = "TJSP"
	parseOne(t, p, nil, item)

	if len(spy.seenUF) == 0 {
		t.Fatal("calendar was never consulted")
	}
	for i, uf := range spy.seenUF {
		if uf != "SP" {
			t.Errorf("seenUF[%d] = %q, want SP", i, uf)
		}
	}
	for i, court := range spy.seenCourt {
		if court != "TJSP" {
			t.Errorf("seenCourt[%d] = %q, want TJSP", i, court)
		}
	}
}

// C13 — an unknown sigla falls back to uf "" (national only) without panic/error.
func TestDJENParser_Parse_UnknownSiglaFallsBackToNational(t *testing.T) {
	t.Parallel()
	p, spy := newParser()

	item := baseItem()
	item.SiglaTribunal = "TJZZ" // not in the map
	record, _ := parseOne(t, p, nil, item)

	if record.Court != "TJZZ" {
		t.Errorf("Court = %q, want TJZZ", record.Court)
	}
	if len(spy.seenUF) == 0 {
		t.Fatal("calendar was never consulted")
	}
	for i, uf := range spy.seenUF {
		if uf != "" {
			t.Errorf("seenUF[%d] = %q, want empty (national only)", i, uf)
		}
	}
}

// DocketEntries is always empty — DJEN carries no andamentos. Also checks a
// multi-item envelope maps one record + one intimation per item.
func TestDJENParser_Parse_MultiItemAndNoDocketEntries(t *testing.T) {
	t.Parallel()
	p, _ := newParser()

	first := baseItem()
	second := baseItem()
	second.Hash = "djen-hash-def456"
	second.NumeroProcesso = "00000020120268260100"

	res, err := p.Parse(payload(t, nil, first, second))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(res.CourtRecords) != 2 || len(res.Intimations) != 2 {
		t.Fatalf("want 2 records/2 intimations, got %d/%d", len(res.CourtRecords), len(res.Intimations))
	}
	if len(res.DocketEntries) != 0 {
		t.Errorf("DocketEntries len = %d, want 0", len(res.DocketEntries))
	}
}

// A malformed envelope is a terminal parse error, not a panic.
func TestDJENParser_Parse_MalformedPayload(t *testing.T) {
	t.Parallel()
	p, _ := newParser()

	if _, err := p.Parse(RawPayload{Source: SourceDJEN, Body: []byte("{not json")}); err == nil {
		t.Fatal("Parse(malformed) = nil error, want error")
	}
}
