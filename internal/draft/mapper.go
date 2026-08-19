package draft

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/draft/draftdb"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// mapper.go is the boundary where driver types die (docs §4b.3): uuid.UUID,
// pgtype.* are absorbed here so the entity, use case and read models stay pure Go.
// The repo returns *Draft / DraftDetailView, never the sqlc row.

// parseUUID parses a string into a uuid.UUID, returning an InvalidInput error on
// failure. This is the barrier: a caller-supplied id that is not a valid UUID is a
// 400 (not a 500), so we map the parse failure to KindInvalid here rather than
// letting it bubble as an infra error.
func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, apperr.NewInvalid("invalid id: " + s)
	}
	return id, nil
}

// optUUID returns a pgtype.UUID for a possibly-empty id. An empty string maps to
// SQL NULL (the intimation_id / case_id when source != intimation / processo).
func optUUID(s string) pgtype.UUID {
	if s == "" {
		return pgtype.UUID{Valid: false}
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{Valid: false}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

// pgUUIDToString collapses a pgtype.UUID to a plain string, "" for SQL NULL.
func pgUUIDToString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// derefString collapses a nullable text column (*string) to a plain string.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// textToNull is the inverse: an empty string writes as SQL NULL.
func textToNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// timestamptzToTime converts a pgtype.Timestamptz to time.Time, zero on NULL.
func timestamptzToTime(ts pgtype.Timestamptz) time.Time {
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time
}

// pgDateToTime converts a pgtype.Date to time.Time, zero on NULL.
func pgDateToTime(d pgtype.Date) time.Time {
	if !d.Valid {
		return time.Time{}
	}
	return d.Time
}

// draftFromInsertRow maps the InsertDraft RETURNING row to a *Draft entity.
func draftFromInsertRow(r draftdb.InsertDraftRow) *Draft {
	return &Draft{
		ID:           r.ID.String(),
		TenantID:     r.TenantID.String(),
		CaseID:       pgUUIDToString(r.CaseID),
		IntimationID: pgUUIDToString(r.IntimationID),
		PieceType:    r.PieceType,
		Title:        r.Title,
		Content:      derefString(r.Content),
		Status:       r.Status,
		SagaState:    r.SagaState,
		CreatedAt:    timestamptzToTime(r.CreatedAt),
		UpdatedAt:    timestamptzToTime(r.UpdatedAt),
	}
}

// draftFromGetByIDRow maps the GetDraftByID row to a *Draft entity.
func draftFromGetByIDRow(r draftdb.GetDraftByIDRow) *Draft {
	return &Draft{
		ID:           r.ID.String(),
		TenantID:     r.TenantID.String(),
		CaseID:       pgUUIDToString(r.CaseID),
		IntimationID: pgUUIDToString(r.IntimationID),
		PieceType:    r.PieceType,
		Title:        r.Title,
		Content:      derefString(r.Content),
		Status:       r.Status,
		SagaState:    r.SagaState,
		CreatedAt:    timestamptzToTime(r.CreatedAt),
		UpdatedAt:    timestamptzToTime(r.UpdatedAt),
	}
}

// ── Attachment mappers (Fatia 2) ─────────────────────────────────────────────

// attachmentFromRow maps a DraftAttachment db row to an *Attachment entity. The
// name and document metadata are NOT on the DraftAttachment model (they live on
// the document table) — this function is only for write results (INSERT/UPDATE).
// Read model rows (GetDraftAttachmentsRow) are mapped by attachmentFromReadRow.
func attachmentFromRow(r draftdb.DraftAttachment) *Attachment {
	return &Attachment{
		ID:         r.ID.String(),
		TenantID:   r.TenantID.String(),
		DraftID:    r.DraftID.String(),
		DocumentID: r.DocumentID.String(),
		Category:   AttachmentCategory(r.Category),
		Position:   int(r.Position),
		CreatedAt:  timestamptzToTime(r.CreatedAt),
	}
}

// attachmentsFromRows maps a slice of GetDraftAttachmentsRow (the read-model query
// that JOINs document to carry name/mime_type/size_bytes) to []Attachment.
func attachmentsFromRows(rows []draftdb.GetDraftAttachmentsRow) []Attachment {
	out := make([]Attachment, 0, len(rows))
	for _, r := range rows {
		name := derefString(r.Name)
		if name == "" {
			name = derefString(r.OriginalFilename)
		}
		out = append(out, Attachment{
			ID:         r.ID.String(),
			DocumentID: r.DocumentID.String(),
			Name:       name,
			Category:   AttachmentCategory(r.Category),
			MimeType:   derefString(r.MimeType),
			SizeBytes:  derefInt64(r.SizeBytes),
			Status:     r.Status,
			Position:   int(r.Position),
			CreatedAt:  timestamptzToTime(r.CreatedAt),
		})
	}
	return out
}

// derefInt64 collapses a nullable int8 column (*int64) to a plain int64.
func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// draftFromGetByIntimationRow maps the GetDraftByIntimationID row to a *Draft entity.
func draftFromGetByIntimationRow(r draftdb.GetDraftByIntimationIDRow) *Draft {
	return &Draft{
		ID:           r.ID.String(),
		TenantID:     r.TenantID.String(),
		CaseID:       pgUUIDToString(r.CaseID),
		IntimationID: pgUUIDToString(r.IntimationID),
		PieceType:    r.PieceType,
		Title:        r.Title,
		Content:      derefString(r.Content),
		Status:       r.Status,
		SagaState:    r.SagaState,
		CreatedAt:    timestamptzToTime(r.CreatedAt),
		UpdatedAt:    timestamptzToTime(r.UpdatedAt),
	}
}

// ── AI generation mappers (Fatia 3) ──────────────────────────────────────────

// draftFromUpdateSagaStateRow maps the UpdateSagaState RETURNING row to a *Draft.
func draftFromUpdateSagaStateRow(r draftdb.UpdateSagaStateRow) *Draft {
	return &Draft{
		ID:           r.ID.String(),
		TenantID:     r.TenantID.String(),
		CaseID:       pgUUIDToString(r.CaseID),
		IntimationID: pgUUIDToString(r.IntimationID),
		PieceType:    r.PieceType,
		Title:        r.Title,
		Content:      derefString(r.Content),
		Status:       r.Status,
		SagaState:    r.SagaState,
		CreatedAt:    timestamptzToTime(r.CreatedAt),
		UpdatedAt:    timestamptzToTime(r.UpdatedAt),
	}
}

// reviewFromInsertRow maps an InsertReview RETURNING row to a *Review entity.
func reviewFromInsertRow(r draftdb.InsertReviewRow) *Review {
	return &Review{
		ID:           r.ID.String(),
		DraftID:      r.DraftID.String(),
		Findings:     unmarshalFindings(r.Findings),
		Coverage:     unmarshalCoverage(r.Coverage),
		ModelVersion: r.ModelVersion,
		RulesVersion: r.RulesVersion,
		Status:       r.Status,
		GeneratedAt:  timestamptzToTime(r.GeneratedAt),
		CreatedAt:    timestamptzToTime(r.CreatedAt),
	}
}

// reviewFromGetLatestRow maps a GetLatestReview row to a *Review entity.
func reviewFromGetLatestRow(r draftdb.GetLatestReviewRow) *Review {
	return &Review{
		ID:           r.ID.String(),
		DraftID:      r.DraftID.String(),
		Findings:     unmarshalFindings(r.Findings),
		Coverage:     unmarshalCoverage(r.Coverage),
		ModelVersion: r.ModelVersion,
		RulesVersion: r.RulesVersion,
		Status:       r.Status,
		GeneratedAt:  timestamptzToTime(r.GeneratedAt),
		CreatedAt:    timestamptzToTime(r.CreatedAt),
	}
}

// marshalJSON serializes a value to []byte (jsonb). An encoding error is an infra
// fault — it indicates a programmer mistake (a non-serializable type), not a user
// error.
func marshalJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return b, nil
}

// unmarshalFindings decodes jsonb bytes into []Finding. A decode fault returns an
// empty slice (best-effort — a corrupt review row should not crash the read model).
func unmarshalFindings(b []byte) []Finding {
	var out []Finding
	if len(b) == 0 {
		return []Finding{}
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return []Finding{}
	}
	if out == nil {
		return []Finding{}
	}
	return out
}

// unmarshalCoverage decodes jsonb bytes into a Coverage struct. A decode fault
// returns a zero-value Coverage (best-effort — same rationale as unmarshalFindings).
func unmarshalCoverage(b []byte) Coverage {
	var out Coverage
	if len(b) == 0 {
		return Coverage{DocumentsCited: []string{}}
	}
	_ = json.Unmarshal(b, &out)
	if out.DocumentsCited == nil {
		out.DocumentsCited = []string{}
	}
	return out
}

// timeToTimestamptz converts a time.Time to a pgtype.Timestamptz.
func timeToTimestamptz(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{Valid: false}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// ── Parties + signing-lawyer mappers ─────────────────────────────────────────

// djenRecipientRaw is the JSON shape of one element in intimation.recipients.
// It mirrors the djenRecipient struct in the acquisition slice without importing it
// (same principle as reading court_record directly — slice independence).
type djenRecipientRaw struct {
	Name    string `json:"name"`
	OAB     string `json:"oab"`
	UF      string `json:"uf"`
	Matched bool   `json:"matched"`
}

// signingLawyerFromRecipients parses intimation.recipients jsonb and returns the
// first recipient with matched=true (our OAB, the signing lawyer). Returns a
// zero-value SigningLawyer when recipients is empty, nil, or has no matched entry.
func signingLawyerFromRecipients(raw []byte) SigningLawyer {
	if len(raw) == 0 {
		return SigningLawyer{}
	}
	var recs []djenRecipientRaw
	if err := json.Unmarshal(raw, &recs); err != nil {
		return SigningLawyer{}
	}
	for _, r := range recs {
		if r.Matched {
			return SigningLawyer{Name: r.Name, OAB: r.OAB, UF: r.UF}
		}
	}
	return SigningLawyer{}
}

// counselInfoRaw is the JSON shape of one element in the counsels jsonb_agg text.
type counselInfoRaw struct {
	Name string `json:"name"`
	OAB  string `json:"oab"`
	UF   string `json:"uf"`
}

// partiesFromRows maps GetPartiesForDraft rows to []PartyInfo. The counsels column
// is a jsonb_agg encoded as text; a decode failure returns an empty counsel string
// (best-effort — a corrupt row should not crash the generation pipeline).
func partiesFromRows(rows []draftdb.GetPartiesForDraftRow) []PartyInfo {
	out := make([]PartyInfo, 0, len(rows))
	for _, r := range rows {
		counsel := firstCounselLabel(r.Counsels)
		out = append(out, PartyInfo{
			Role:    r.Role,
			Name:    r.Name,
			Counsel: counsel,
		})
	}
	return out
}

// firstCounselLabel returns a short label for the first (alphabetically) advogado
// aggregated under a party, formatted as "Name (OAB/UF nº oab)". Returns "" when
// the counsels array is empty or cannot be decoded.
func firstCounselLabel(counselsText string) string {
	if counselsText == "" || counselsText == "[]" {
		return ""
	}
	var recs []counselInfoRaw
	if err := json.Unmarshal([]byte(counselsText), &recs); err != nil || len(recs) == 0 {
		return ""
	}
	c := recs[0]
	if c.Name == "" && c.OAB == "" {
		return ""
	}
	if c.OAB == "" {
		return c.Name
	}
	uf := c.UF
	if uf == "" {
		uf = "??"
	}
	return fmt.Sprintf("%s (OAB/%s nº %s)", c.Name, uf, c.OAB)
}

// ── Chat mappers (Fatia 3b) ──────────────────────────────────────────────────

// chatMessageFromRow maps a draftdb.ChatMessage row to a *ChatMessage entity.
// citations jsonb is unmarshalled from []byte; a decode fault returns an empty slice
// (best-effort — a corrupt row should not crash the read model).
func chatMessageFromRow(r draftdb.ChatMessage) *ChatMessage {
	return &ChatMessage{
		ID:           r.ID.String(),
		DraftID:      r.DraftID.String(),
		Role:         r.Role,
		Content:      r.Content,
		Citations:    unmarshalCitations(r.Citations),
		Grounded:     r.Grounded,
		ModelVersion: derefString(r.ModelVersion),
		CreatedAt:    timestamptzToTime(r.CreatedAt),
	}
}

// chatMessagesFromRows maps a slice of draftdb.ChatMessage rows to []ChatMessage.
func chatMessagesFromRows(rows []draftdb.ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(rows))
	for _, r := range rows {
		out = append(out, *chatMessageFromRow(r))
	}
	return out
}

// unmarshalCitations decodes jsonb bytes into []Citation. A decode fault or empty
// input returns an empty slice (never nil — serializes as [] in JSON, not null).
func unmarshalCitations(b []byte) []Citation {
	if len(b) == 0 {
		return []Citation{}
	}
	var out []Citation
	if err := json.Unmarshal(b, &out); err != nil {
		return []Citation{}
	}
	if out == nil {
		return []Citation{}
	}
	return out
}
