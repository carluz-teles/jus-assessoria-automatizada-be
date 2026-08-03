package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jusassessoria/platform/lib/calendar"
)

// djen_parser.go is the REAL DJEN parser behind the Parser port: it turns the
// aggregated JSON the DJENConnector produces (one envelope wrapping every
// comunicação fetched for a tenant's OABs) into a connector-agnostic
// ParsedResult. Each comunicação yields BOTH a placeholder court record (degree
// UNKNOWN — DJEN never discloses the instance; a later DATAJUD sync reveals it)
// and one intimation carrying the deadline dates.
//
// The three deadline dates are derived here via lib/calendar so the downstream
// deadline slice consumes ready civil dates: made_available_at is the DJEN
// disponibilização, published_at is the next business day after it, and
// deadline_start_at is the next business day after publication (CPC art. 224).
// DJEN carries no andamentos, so DocketEntries is always empty.

// djenCompleteness is the source confidence written on every DJEN court record.
// DJEN discloses only court/class/órgão and no degree, so the record is a low-
// confidence placeholder until a DATAJUD enrichment fills it in.
const djenCompleteness = 0.3

// DJEN communication kinds (tipoComunicacao) as the Comunica API spells them,
// mapped to the app's intimation type constants. Anything else is a generic
// COMUNICACAO — an unknown kind is never an error.
const (
	djenTipoIntimacao = "Intimação"
	djenTipoCitacao   = "Citação"
)

// tribunalUF maps a DJEN siglaTribunal to the two-letter UF whose STATE holiday
// calendar governs that court's deadlines. State courts (TJxx) map to their UF;
// the superior courts map to "" (only the NATIONAL calendar applies to them).
// A sigla ABSENT from this map is unknown: the parser falls back to uf "" (still
// honouring national holidays + the forensic recess) and logs a warning, never
// aborting. Federal/labour regionals (TRFx/TRTx) span multiple UFs and are
// intentionally omitted — they resolve to the national calendar until a court-
// scoped holiday feed is seeded per tribunal.
var tribunalUF = map[string]string{
	"TJAC": "AC", "TJAL": "AL", "TJAP": "AP", "TJAM": "AM",
	"TJBA": "BA", "TJCE": "CE", "TJDFT": "DF", "TJES": "ES",
	"TJGO": "GO", "TJMA": "MA", "TJMT": "MT", "TJMS": "MS",
	"TJMG": "MG", "TJPA": "PA", "TJPB": "PB", "TJPR": "PR",
	"TJPE": "PE", "TJPI": "PI", "TJRJ": "RJ", "TJRN": "RN",
	"TJRS": "RS", "TJRO": "RO", "TJRR": "RR", "TJSC": "SC",
	"TJSP": "SP", "TJSE": "SE", "TJTO": "TO",
	// Superior courts: national calendar only, so no UF — present with "" so a
	// legitimate superior-court sigla does NOT trip the unknown-sigla warning.
	"STF": "", "STJ": "", "TST": "", "TSE": "", "STM": "",
}

// DJENParser satisfies Parser for the DJEN source. It holds the shared calendar
// used to derive the deadline dates; it carries no per-payload state, so a single
// instance is safe to reuse across syncs.
type DJENParser struct {
	cal *calendar.Calendar
}

var _ Parser = (*DJENParser)(nil)

// NewDJENParser builds the DJEN parser over the business-day calendar it needs to
// derive published_at / deadline_start_at.
func NewDJENParser(cal *calendar.Calendar) *DJENParser {
	return &DJENParser{cal: cal}
}

// CanParse claims every payload tagged with the DJEN source; the orchestrator
// routes DJEN fetches here.
func (p *DJENParser) CanParse(payload RawPayload) bool {
	return payload.Source == SourceDJEN
}

// Parse decodes the aggregated DJEN envelope and maps each comunicação to a
// (court record, intimation) pair. A malformed envelope or a date the calendar
// cannot resolve is terminal — the sync use case records the run FAILED and
// archives the task, since a broken payload never parses on retry.
//
// The Parser port carries no context.Context (it is a pure format adapter fixed
// by the connector seam), so the calendar calls run on a background context.
func (p *DJENParser) Parse(payload RawPayload) (ParsedResult, error) {
	var env djenEnvelope
	if err := json.Unmarshal(payload.Body, &env); err != nil {
		return ParsedResult{}, fmt.Errorf("djen parser: unmarshal payload: %w", err)
	}

	ctx := context.Background()
	scope := newOABSet(env.Scope.OABs)

	result := ParsedResult{
		CourtRecords:  make([]ParsedCourtRecord, 0, len(env.Items)),
		DocketEntries: []ParsedDocketEntry{}, // DJEN delivers no andamentos
		Intimations:   make([]ParsedIntimation, 0, len(env.Items)),
	}

	for i, item := range env.Items {
		record, intimation, err := p.parseItem(ctx, item, scope)
		if err != nil {
			return ParsedResult{}, fmt.Errorf("djen parser: item %d: %w", i, err)
		}
		result.CourtRecords = append(result.CourtRecords, record)
		result.Intimations = append(result.Intimations, intimation)
	}

	return result, nil
}

// parseItem maps one comunicação into its court record and intimation, deriving
// the deadline dates through the calendar for the item's court/UF.
func (p *DJENParser) parseItem(ctx context.Context, item djenItem, scope oabSet) (ParsedCourtRecord, ParsedIntimation, error) {
	court := item.SiglaTribunal
	uf := courtUF(ctx, court)

	madeAvailable, err := parseDJENDate(item.DataDisponibilizacao)
	if err != nil {
		return ParsedCourtRecord{}, ParsedIntimation{}, fmt.Errorf("data_disponibilizacao: %w", err)
	}

	published, err := p.cal.NextBusinessDay(ctx, madeAvailable, uf, court)
	if err != nil {
		return ParsedCourtRecord{}, ParsedIntimation{}, fmt.Errorf("published_at: %w", err)
	}

	deadlineStart, err := p.cal.NextBusinessDay(ctx, published, uf, court)
	if err != nil {
		return ParsedCourtRecord{}, ParsedIntimation{}, fmt.Errorf("deadline_start_at: %w", err)
	}

	status := IntimationStatusActive
	var cancelledAt time.Time
	if item.DataCancelamento != nil {
		status = IntimationStatusCancelled
		cancelledAt, err = parseDJENDate(*item.DataCancelamento)
		if err != nil {
			return ParsedCourtRecord{}, ParsedIntimation{}, fmt.Errorf("data_cancelamento: %w", err)
		}
	}

	recipients, err := buildRecipients(item, scope)
	if err != nil {
		return ParsedCourtRecord{}, ParsedIntimation{}, fmt.Errorf("recipients: %w", err)
	}

	cnj := item.NumeroProcesso

	record := ParsedCourtRecord{
		CNJNumber:    cnj,
		Degree:       DegreeUnknown,
		Court:        court,
		Class:        item.NomeClasse,
		JudgingBody:  item.NomeOrgao,
		Completeness: djenCompleteness,
	}

	intimation := ParsedIntimation{
		CNJNumber:       cnj,
		Degree:          DegreeUnknown,
		Hash:            item.Hash,
		MadeAvailableAt: madeAvailable,
		PublishedAt:     published,
		DeadlineStartAt: deadlineStart,
		Content:         item.Texto,
		Source:          SourceDJEN,
		Type:            mapIntimationType(item.TipoComunicacao),
		Status:          status,
		SourceURL:       item.Link,
		CancelledAt:     cancelledAt,
		CancelReason:    deref(item.MotivoCancelamento),
		Recipients:      recipients,
	}

	return record, intimation, nil
}

// courtUF resolves the UF whose state calendar applies to a DJEN sigla. An
// unknown sigla is not fatal: it falls back to "" (national calendar only) and
// warns so the missing mapping surfaces without breaking the sync.
func courtUF(ctx context.Context, sigla string) string {
	uf, ok := tribunalUF[sigla]
	if !ok {
		slog.WarnContext(ctx, "djen parser: unknown court sigla, using national calendar only",
			"sigla", sigla)
		return ""
	}
	return uf
}

// mapIntimationType maps the DJEN tipoComunicacao to an intimation type. An
// unrecognised kind is a generic COMUNICACAO, never an error.
func mapIntimationType(tipo string) string {
	switch tipo {
	case djenTipoIntimacao:
		return IntimationTypeIntimacao
	case djenTipoCitacao:
		return IntimationTypeCitacao
	default:
		return IntimationTypeComunicacao
	}
}

// parseDJENDate reads a DJEN civil date (YYYY-MM-DD) as midnight UTC, the form
// lib/calendar expects. DJEN's dates are date-only, so there is no time-of-day
// or zone to normalise.
func parseDJENDate(s string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse date %q: %w", s, err)
	}
	return t, nil
}

// buildRecipients flattens every addressee — parties (destinatarios) and lawyers
// (destinatarioadvogados) — into the jsonb list carried verbatim to the
// intimation's recipients column. A lawyer is flagged matched when their
// (OAB number, UF) is one the tenant's integration scope queried; parties are
// never matched.
func buildRecipients(item djenItem, scope oabSet) (json.RawMessage, error) {
	out := make([]djenRecipient, 0, len(item.Destinatarios)+len(item.DestinatarioAdvogados))

	for _, d := range item.Destinatarios {
		out = append(out, djenRecipient{
			Kind: recipientKindParty,
			Name: d.Nome,
			Polo: d.Polo,
		})
	}

	for _, da := range item.DestinatarioAdvogados {
		adv := da.Advogado
		out = append(out, djenRecipient{
			Kind:      recipientKindLawyer,
			Name:      adv.Nome,
			OABNumber: adv.NumeroOAB,
			OABUF:     adv.UfOAB,
			Matched:   scope.has(adv.NumeroOAB, adv.UfOAB),
		})
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal recipients: %w", err)
	}
	return raw, nil
}

// deref returns the pointed-to string, or "" when the pointer is nil — DJEN
// sends null for the optional cancellation fields.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// oabKey is the (number, UF) identity a scope match keys on — the pair, not a
// combined string, mirroring how a fetch queries the DJEN per (number, UF).
type oabKey struct {
	number string
	uf     string
}

// oabSet is the tenant's queried OAB registrations, for O(1) matched lookups.
type oabSet map[oabKey]bool

// newOABSet indexes the envelope's scope OABs for matching.
func newOABSet(entries []djenOAB) oabSet {
	set := make(oabSet, len(entries))
	for _, e := range entries {
		set[oabKey{number: e.Number, uf: e.UF}] = true
	}
	return set
}

// has reports whether (number, uf) is in the scope.
func (s oabSet) has(number, uf string) bool {
	return s[oabKey{number: number, uf: uf}]
}

// Recipient kinds written into the recipients jsonb.
const (
	recipientKindParty  = "PARTY"
	recipientKindLawyer = "LAWYER"
)

// djenRecipient is one flattened addressee in the recipients jsonb. Polo is set
// for parties; OABNumber/OABUF for lawyers; Matched is true only for a lawyer
// whose OAB the tenant's scope queried.
type djenRecipient struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Polo      string `json:"polo,omitempty"`
	OABNumber string `json:"oabNumber,omitempty"`
	OABUF     string `json:"oabUf,omitempty"`
	Matched   bool   `json:"matched"`
}

// djenEnvelope is the aggregated payload the DJENConnector produces: every
// comunicação fetched for the tenant's OABs, plus the scope those OABs form so
// the parser can flag matched lawyers. Its json tags are the wire contract
// between the connector (sub-slice B) and this parser.
type djenEnvelope struct {
	Scope djenScope  `json:"scope"`
	Items []djenItem `json:"items"`
}

// djenScope carries the OAB registrations the fetch queried.
type djenScope struct {
	OABs []djenOAB `json:"oabs"`
}

// djenOAB is one queried OAB registration (bare number + UF seccional).
type djenOAB struct {
	Number string `json:"number"`
	UF     string `json:"uf"`
}

// djenItem is one comunicação as the CNJ Comunica API reports it. numero_processo
// arrives already without the CNJ mask.
type djenItem struct {
	Hash                  string                     `json:"hash"`
	NumeroProcesso        string                     `json:"numero_processo"`
	SiglaTribunal         string                     `json:"siglaTribunal"`
	NomeClasse            string                     `json:"nomeClasse"`
	NomeOrgao             string                     `json:"nomeOrgao"`
	TipoComunicacao       string                     `json:"tipoComunicacao"`
	Texto                 string                     `json:"texto"`
	Link                  string                     `json:"link"`
	DataDisponibilizacao  string                     `json:"data_disponibilizacao"`
	DataCancelamento      *string                    `json:"data_cancelamento"`
	MotivoCancelamento    *string                    `json:"motivo_cancelamento"`
	Destinatarios         []djenDestinatario         `json:"destinatarios"`
	DestinatarioAdvogados []djenDestinatarioAdvogado `json:"destinatarioadvogados"`
}

// djenDestinatario is a party addressee.
type djenDestinatario struct {
	Nome string `json:"nome"`
	Polo string `json:"polo"`
}

// djenDestinatarioAdvogado wraps the lawyer addressee, matching the Comunica
// API's nesting of the advogado object.
type djenDestinatarioAdvogado struct {
	Advogado djenAdvogado `json:"advogado"`
}

// djenAdvogado is a lawyer's identity and OAB registration.
type djenAdvogado struct {
	Nome      string `json:"nome"`
	NumeroOAB string `json:"numero_oab"`
	UfOAB     string `json:"uf_oab"`
}
