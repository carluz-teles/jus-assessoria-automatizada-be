package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/pkg/tribunal"
)

// djen_parser.go turns a DJEN RawPayload (the djenPayload the connector emits)
// into the connector-agnostic ParsedResult the sync cycle upserts. DJEN discovers
// processes but never their degree, so every court record lands degree=UNKNOWN
// (the DATAJUD reconciliation grades it later); DJEN carries intimações, not
// andamentos, so no docket entries are produced here. The publication/deadline
// dates are DERIVED from the availability date through the judicial calendar
// (CPC art. 224), which is why Parse takes a context.

// djenDateLayout is the ISO date the Comunica API returns in data_disponibilizacao
// / data_cancelamento (e.g. "2026-07-31").
const djenDateLayout = "2006-01-02"

// djenCompleteness is the confidence a DJEN-sourced court record is fully
// populated: DJEN gives court/órgão/classe but not degree, parties, or claim
// value, so it is deliberately low — DATAJUD enrichment raises it later.
const djenCompleteness = 0.3

// businessCalendar is the narrow slice of lib/calendar the parser needs to derive
// the publication and deadline-start dates. Defining it here (consumer side) lets
// the parser unit test inject a fake without a HolidaySource; *calendar.Calendar
// satisfies it.
type businessCalendar interface {
	NextBusinessDay(ctx context.Context, day time.Time, uf, court string) (time.Time, error)
}

// DJENParser maps DJEN payloads. It holds the judicial calendar used to derive the
// CPC-224 dates; build it through NewDJENParser.
type DJENParser struct {
	calendar businessCalendar
}

// NewDJENParser wires the parser with the calendar it derives dates from.
func NewDJENParser(cal businessCalendar) *DJENParser {
	return &DJENParser{calendar: cal}
}

var _ Parser = (*DJENParser)(nil)

// djenComunicacao is the subset of a Comunica API item the parser maps. The
// nullable columns (link, data_cancelamento, motivo_cancelamento) are pointers so
// an absent value stays distinct from an empty one.
type djenComunicacao struct {
	Hash                 string             `json:"hash"`
	NumeroProcesso       string             `json:"numero_processo"`
	TipoComunicacao      string             `json:"tipoComunicacao"`
	NomeOrgao            string             `json:"nomeOrgao"`
	SiglaTribunal        string             `json:"siglaTribunal"`
	NomeClasse           string             `json:"nomeClasse"`
	Texto                string             `json:"texto"`
	Link                 *string            `json:"link"`
	DataDisponibilizacao string             `json:"data_disponibilizacao"`
	DataCancelamento     *string            `json:"data_cancelamento"`
	MotivoCancelamento   *string            `json:"motivo_cancelamento"`
	Ativo                bool               `json:"ativo"`
	Advogados            []djenAdvogadoLink `json:"destinatarioadvogados"`
	// Destinatarios are the parties this communication addressed: {nome, polo}, polo
	// "A" (ativo = autor) / "P" (passivo = réu). Mapped to the party materialization
	// (parseParties); it was HISTORICALLY discarded (recipients only carried advogados).
	Destinatarios []djenDestinatario `json:"destinatarios"`
}

type djenDestinatario struct {
	Nome string `json:"nome"`
	Polo string `json:"polo"`
}

type djenAdvogadoLink struct {
	Advogado djenAdvogado `json:"advogado"`
}

type djenAdvogado struct {
	Nome      string `json:"nome"`
	NumeroOAB string `json:"numero_oab"`
	UFOAB     string `json:"uf_oab"`
}

// djenRecipient is one addressee persisted in the intimation.recipients jsonb: the
// advogado's identity plus whether their OAB is one the tenant asked us to watch
// (Matched). The counting/notification slices read Matched to know which of a
// process's advogados is the client.
type djenRecipient struct {
	Name      string `json:"name"`
	OABNumber string `json:"oab_number"`
	OABUF     string `json:"oab_uf"`
	Matched   bool   `json:"matched"`
}

// CanParse claims any DJEN payload (matched by source, not connector id, so a
// real and a replayed DJEN payload both route here).
func (*DJENParser) CanParse(p RawPayload) bool { return p.Source == SourceDJEN }

// Parse maps every communication in the payload. It is resilient per item: an
// item whose required availability date does not parse is logged and skipped
// rather than failing the whole window (which the sync cycle would archive). Court
// records are deduped by CNJ (all UNKNOWN degree), so N intimations on one process
// yield one court record.
func (p *DJENParser) Parse(ctx context.Context, raw RawPayload) (ParsedResult, error) {
	var payload djenPayload
	if err := json.Unmarshal(raw.Body, &payload); err != nil {
		// A payload that does not even decode is a terminal parse fault (never valid
		// on retry) — surfaced to the sync cycle, which archives the task.
		return ParsedResult{}, fmt.Errorf("djen: decode payload: %w", err)
	}

	watched := watchedOABSet(payload.OABs)

	records := make(map[string]ParsedCourtRecord)
	ordered := make([]ParsedCourtRecord, 0)
	intimations := make([]ParsedIntimation, 0, len(payload.Items))
	parties := make([]ParsedParty, 0, len(payload.Items))

	for _, rawItem := range payload.Items {
		var item djenComunicacao
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return ParsedResult{}, fmt.Errorf("djen: decode item: %w", err)
		}

		// Lean-ingestion gate: a generic COMUNICACAO (Edital and anything that is not
		// an INTIMACAO/CITACAO) opens no prazo and is dropped at the source — no
		// intimation AND no court record (the record is appended below only for a kept
		// item, so a COMUNICACAO-only processo yields zero court_records, not a phantom
		// one). This covers backfill, sync and the national match: all three write
		// through parser.Parse. A defensive no-op in deadline.OnIntimationObserved is
		// the belt-and-suspenders guard against a future producer.
		if djenType(item.TipoComunicacao) == IntimationTypeComunicacao {
			continue
		}

		intim, ok := p.parseIntimation(ctx, item, watched)
		if !ok {
			continue
		}
		intimations = append(intimations, intim)
		// The parties (destinatarios + their advogados) of this communication. Emitted
		// only for a kept item so a skipped (unparseable) one never seeds a party whose
		// record was never produced. Deduped downstream by the DB's unique constraints.
		parties = append(parties, parseParties(item)...)

		key := recordKey(item.NumeroProcesso, DegreeUnknown)
		if _, seen := records[key]; !seen {
			cr := ParsedCourtRecord{
				CNJNumber:    item.NumeroProcesso,
				Degree:       DegreeUnknown,
				Court:        item.SiglaTribunal,
				Class:        item.NomeClasse,
				JudgingBody:  item.NomeOrgao,
				Completeness: djenCompleteness,
			}
			records[key] = cr
			ordered = append(ordered, cr)
		}
	}

	return ParsedResult{
		CourtRecords: ordered,
		Intimations:  intimations,
		Parties:      parties,
	}, nil
}

// parseParties maps one communication's destinatários to ParsedParty, attaching the
// communication's advogados to the right party. The DJEN does NOT link an advogado to
// a specific destinatário, so we infer: the advogados of a communication represent the
// destinatários of that SAME communication. When every destinatário shares one polo,
// the advogados are that polo's counsel and attach to each of those parties. When the
// destinatários are mixed (more than one polo) — so the polo is not inferible — we
// attach the advogados to the DEFENDANT (the adverse party) by default, matching the
// most common DJEN shape where the réu is the one being intimated with counsel; this
// is a documented approximation, not a fact. An item with no destinatário but with
// advogados falls back to a single DEFENDANT party carrying them, so the counsel is
// never dropped. Duplicate parties across communications are deduped by the DB.
func parseParties(item djenComunicacao) []ParsedParty {
	counsels := parsedCounsels(item.Advogados)

	// Group the destinatários by role, preserving first-seen order per role and
	// deduping the same (role, name) within this communication.
	roles := make([]string, 0, len(item.Destinatarios))
	byRole := make(map[string][]string) // role → ordered unique names
	seen := make(map[string]bool)
	for _, d := range item.Destinatarios {
		if d.Nome == "" {
			continue
		}
		role := partyRoleFromPolo(d.Polo)
		k := role + "\x00" + d.Nome
		if seen[k] {
			continue
		}
		seen[k] = true
		if _, ok := byRole[role]; !ok {
			roles = append(roles, role)
		}
		byRole[role] = append(byRole[role], d.Nome)
	}

	// No named party but advogados present: keep the counsel on a DEFENDANT placeholder
	// rather than lose it (the adverse party is the safe default — see the doc above).
	if len(roles) == 0 {
		if len(counsels) == 0 {
			return nil
		}
		return []ParsedParty{{
			CNJNumber: item.NumeroProcesso,
			Degree:    DegreeUnknown,
			Role:      PartyRoleDefendant,
			Counsels:  counsels,
		}}
	}

	// The advogados attach to the destinatários' party when the polo is inferible (a
	// single polo across the communication); otherwise to the DEFENDANT by default.
	counselRole := roles[0]
	if len(roles) > 1 {
		counselRole = PartyRoleDefendant
	}

	out := make([]ParsedParty, 0, len(item.Destinatarios))
	for _, role := range roles {
		for _, name := range byRole[role] {
			party := ParsedParty{
				CNJNumber: item.NumeroProcesso,
				Degree:    DegreeUnknown,
				Role:      role,
				Name:      name,
			}
			if role == counselRole {
				party.Counsels = counsels
			}
			out = append(out, party)
		}
	}
	return out
}

// parsedCounsels maps the DJEN destinatarioadvogados to ParsedCounsel, dropping an
// entry with no OAB (the dedup key) and deduping by (oab, uf). Never nil-guards: an
// empty result is an empty slice.
func parsedCounsels(advs []djenAdvogadoLink) []ParsedCounsel {
	out := make([]ParsedCounsel, 0, len(advs))
	seen := make(map[string]bool, len(advs))
	for _, a := range advs {
		if a.Advogado.NumeroOAB == "" {
			continue
		}
		k := oabKey(a.Advogado.NumeroOAB, a.Advogado.UFOAB)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, ParsedCounsel{
			Name: a.Advogado.Nome,
			OAB:  a.Advogado.NumeroOAB,
			UF:   a.Advogado.UFOAB,
		})
	}
	return out
}

// partyRoleFromPolo maps a DJEN destinatário polo to the party role enum: "A" (ativo =
// autor) → PLAINTIFF, "P" (passivo = réu) → DEFENDANT, anything else → THIRD_PARTY.
// An unexpected polo value is logged at Warn level so DJEN schema changes (new polo
// codes, typos) surface in logs and can be added to the switch rather than silently
// creating phantom THIRD_PARTY entries. The THIRD_PARTY fallback is preserved so the
// party is never dropped.
func partyRoleFromPolo(polo string) string {
	normalized := strings.ToUpper(strings.TrimSpace(polo))
	switch normalized {
	case "A":
		return PartyRolePlaintiff
	case "P":
		return PartyRoleDefendant
	default:
		if normalized != "" {
			// A non-empty, unrecognized polo is a data quality signal: the DJEN may have
			// introduced a new polo code or there is a typo in the payload. Log it so it
			// can be added to the switch above.
			slog.Warn("djen: unrecognized polo value; defaulting to THIRD_PARTY",
				"polo", polo)
		}
		return PartyRoleThirdParty
	}
}

// parseIntimation maps one communication to a ParsedIntimation, deriving the
// publication and deadline-start dates through the calendar. It returns ok=false
// when the item must be skipped (unparseable availability date), having logged the
// reason.
func (p *DJENParser) parseIntimation(ctx context.Context, item djenComunicacao, watched map[string]bool) (ParsedIntimation, bool) {
	madeAvailable, err := time.Parse(djenDateLayout, item.DataDisponibilizacao)
	if err != nil {
		slog.WarnContext(ctx, "djen: skipping item with unparseable data_disponibilizacao",
			"hash", item.Hash,
			"numero_processo", item.NumeroProcesso,
			"data_disponibilizacao", item.DataDisponibilizacao,
		)
		return ParsedIntimation{}, false
	}

	uf, court := tribunal.UF(item.SiglaTribunal), item.SiglaTribunal

	// published_at = first business day after availability; deadline_start_at =
	// first business day after publication (CPC 224). A calendar fault is not fatal
	// to the item: fall back to the raw availability date so the intimation is still
	// captured (the deadline slice can recompute), and log it.
	published := madeAvailable
	if next, cerr := p.calendar.NextBusinessDay(ctx, madeAvailable, uf, court); cerr == nil {
		published = next
	} else {
		slog.WarnContext(ctx, "djen: calendar NextBusinessDay failed for published_at; using availability date",
			"hash", item.Hash, "err", cerr)
	}
	deadlineStart := published
	if next, cerr := p.calendar.NextBusinessDay(ctx, published, uf, court); cerr == nil {
		deadlineStart = next
	} else {
		slog.WarnContext(ctx, "djen: calendar NextBusinessDay failed for deadline_start_at; using published date",
			"hash", item.Hash, "err", cerr)
	}

	cancelled := (item.DataCancelamento != nil && *item.DataCancelamento != "") || !item.Ativo
	status := IntimationStatusActive
	if cancelled {
		status = IntimationStatusCancelled
	}

	return ParsedIntimation{
		CNJNumber:       item.NumeroProcesso,
		Degree:          DegreeUnknown,
		Hash:            item.Hash,
		MadeAvailableAt: madeAvailable,
		PublishedAt:     published,
		DeadlineStartAt: deadlineStart,
		Content:         item.Texto,
		Source:          SourceDJEN,
		Type:            djenType(item.TipoComunicacao),
		Status:          status,
		SourceURL:       deref(item.Link),
		CancelledAt:     parseDJENDate(deref(item.DataCancelamento)),
		CancelReason:    deref(item.MotivoCancelamento),
		Recipients:      djenRecipients(item.Advogados, watched),
	}, true
}

// watchedOABSet indexes the tenant's queried OABs for the matched flag. The key is
// number + "|" + upper(UF), matching how advogado OABs are keyed.
func watchedOABSet(oabs []OABEntry) map[string]bool {
	set := make(map[string]bool, len(oabs))
	for _, o := range oabs {
		set[oabKey(o.Number, o.UF)] = true
	}
	return set
}

func oabKey(number, uf string) string {
	return number + "|" + strings.ToUpper(strings.TrimSpace(uf))
}

// publicationParamsFromItems maps raw DJEN diário items (from FetchDiario) to the
// national publication store's write params: the dedup hash, the court + process, the
// disponibilização date, the normalized recipient OAB keys for the local match, and
// the raw item as payload — which the same djen parser later consumes for a matched
// tenant. An item without its hash (the dedup key) is unusable and skipped.
func publicationParamsFromItems(items []json.RawMessage) ([]PublicationParams, error) {
	out := make([]PublicationParams, 0, len(items))
	for _, raw := range items {
		var item djenComunicacao
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, apperr.NewInfra("djen: decode diário item", err)
		}
		if item.Hash == "" {
			continue
		}
		out = append(out, PublicationParams{
			Hash:            item.Hash,
			Court:           item.SiglaTribunal,
			CNJNumber:       item.NumeroProcesso,
			MadeAvailableAt: parseDJENDate(item.DataDisponibilizacao),
			RecipientOABs:   recipientOABKeys(item.Advogados),
			Payload:         raw,
		})
	}
	return out, nil
}

// recipientOABKeys extracts the unique normalized "NUMBER|UF" keys of an item's
// advogado recipients — the set the local OAB match indexes. It uses the SAME oabKey
// normalization as the per-OAB matched flag (watchedOABSet), so a tenant's watched OAB
// keys identically here and the match lines up. Always a non-nil slice (never null).
func recipientOABKeys(advs []djenAdvogadoLink) []string {
	keys := make([]string, 0, len(advs))
	seen := make(map[string]bool, len(advs))
	for _, a := range advs {
		if a.Advogado.NumeroOAB == "" {
			continue
		}
		k := oabKey(a.Advogado.NumeroOAB, a.Advogado.UFOAB)
		if seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	return keys
}

// djenRecipients maps every advogado addressee to a recipient, flagging the ones
// whose OAB the tenant watches. The result is always a JSON array (never null), so
// the recipients column keeps its '[]' shape when there are no advogados.
func djenRecipients(advs []djenAdvogadoLink, watched map[string]bool) json.RawMessage {
	recipients := make([]djenRecipient, 0, len(advs))
	for _, a := range advs {
		recipients = append(recipients, djenRecipient{
			Name:      a.Advogado.Nome,
			OABNumber: a.Advogado.NumeroOAB,
			OABUF:     a.Advogado.UFOAB,
			Matched:   watched[oabKey(a.Advogado.NumeroOAB, a.Advogado.UFOAB)],
		})
	}
	raw, err := json.Marshal(recipients)
	if err != nil {
		// A slice of plain strings/bools cannot fail to marshal; fall back to the empty
		// array rather than propagate an impossible error.
		return json.RawMessage("[]")
	}
	return raw
}

// djenType normalizes a DJEN tipoComunicacao (e.g. "Intimação", "Citação",
// "Edital", "Comunicação") to the intimation type enum. Anything that is not an
// intimação or citação (Edital included) counts as a generic COMUNICACAO, so the
// deadline slice's counting rule sees only the three kinds it knows.
//
// Matching strategy (conservative):
//   - Contains("INTIMA"): catches "Intimação", "REINTIMAÇÃO", "Intimação Eletrônica".
//     Contains is appropriate here because "INTIMA" is a distinctive root that does
//     not appear as a substring of unrelated types.
//   - HasPrefix("CITA"): catches "Citação", "Citação Eletrônica" but NOT a future
//     type like "NOTIFICAÇÃO DE CITAÇÃO" that contains "CITA" only mid-string.
//     HasPrefix is safer than Contains for this root.
//
// An unrecognized tipo is logged at Warn so new DJEN communication types surface
// in observability and can be explicitly mapped. Behavior is unchanged: COMUNICACAO.
func djenType(tipo string) string {
	t := strings.ToUpper(strings.TrimSpace(tipo))
	switch {
	case strings.Contains(t, "INTIMA"):
		return IntimationTypeIntimacao
	case strings.HasPrefix(t, "CITA"):
		return IntimationTypeCitacao
	default:
		if tipo != "" {
			// An unrecognized tipo defaults to COMUNICACAO (the safe generic bucket).
			// Log it so new DJEN communication types can be mapped explicitly.
			slog.Warn("djen: unrecognized tipoComunicacao; defaulting to COMUNICACAO",
				"tipo_comunicacao", tipo)
		}
		return IntimationTypeComunicacao
	}
}

// parseDJENDate parses an optional ISO date, returning the zero time when it is
// absent or malformed (a cancellation date we cannot read is dropped, not fatal —
// the status still reflects the cancellation).
func parseDJENDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(djenDateLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// deref returns the pointed-to string, or "" when the pointer is nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
