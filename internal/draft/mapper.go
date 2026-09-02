package draft

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/draft/draftdb"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// isNoRows reports whether err is pgx.ErrNoRows (the repo's not-found sentinel).
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// isUniqueViolation reports whether err is a Postgres 23505 (unique/partial
// unique violation) — used to map to a typed conflict AppError.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

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

// pgTimestamptzPtr converts a pgtype.Timestamptz to *time.Time — nil on NULL,
// *time.Time on non-NULL. Used pra timestamps opcionais (workflow steps 0060:
// sent_to_signing_at, signed_at, filed_at) — a UI distingue "não aconteceu" (nil)
// de "aconteceu em X" (non-nil).
func pgTimestamptzPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time.UTC()
	return &t
}

// pgDateToTime converts a pgtype.Date to time.Time, zero on NULL.
func pgDateToTime(d pgtype.Date) time.Time {
	if !d.Valid {
		return time.Time{}
	}
	return d.Time
}

// nonNilStrings normalizes a possibly-nil slice (a party array with no rows comes
// back as nil from array_agg) to an empty slice, so the read model never carries nil
// and the JSON serializes as [] rather than null.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// draftFromInsertRow maps the InsertDraft RETURNING row to a *Draft entity.
func draftFromInsertRow(r draftdb.InsertDraftRow) *Draft {
	return &Draft{
		ID:                r.ID.String(),
		TenantID:          r.TenantID.String(),
		CaseID:            pgUUIDToString(r.CaseID),
		IntimationID:      pgUUIDToString(r.IntimationID),
		PieceType:         r.PieceType,
		Title:             r.Title,
		Content:           derefString(r.Content),
		Status:            r.Status,
		SagaState:         r.SagaState,
		CreatedAt:         timestamptzToTime(r.CreatedAt),
		UpdatedAt:         timestamptzToTime(r.UpdatedAt),
		StructuredContent: structuredContentFromJSON(r.StructuredContent),
		Authorship:        r.Authorship,
		TaskID:            pgUUIDToString(r.TaskID),
		PieceProfileKey:   derefString(r.PieceProfileKey),
	}
}

// draftFromGetByTaskIDRow maps the GetDraftByTaskID row to a *Draft entity — the
// idempotent-fetch counterpart of draftFromGetByIntimationRow for the task-sourced
// path (migration 0088).
func draftFromGetByTaskIDRow(r draftdb.GetDraftByTaskIDRow) *Draft {
	return &Draft{
		ID:                r.ID.String(),
		TenantID:          r.TenantID.String(),
		CaseID:            pgUUIDToString(r.CaseID),
		IntimationID:      pgUUIDToString(r.IntimationID),
		PieceType:         r.PieceType,
		Title:             r.Title,
		Content:           derefString(r.Content),
		Status:            r.Status,
		SagaState:         r.SagaState,
		CreatedAt:         timestamptzToTime(r.CreatedAt),
		UpdatedAt:         timestamptzToTime(r.UpdatedAt),
		StructuredContent: structuredContentFromJSON(r.StructuredContent),
		Authorship:        r.Authorship,
		TaskID:            pgUUIDToString(r.TaskID),
		PieceProfileKey:   derefString(r.PieceProfileKey),
	}
}

// draftFromGetByIDRow maps the GetDraftByID row to a *Draft entity.
func draftFromGetByIDRow(r draftdb.GetDraftByIDRow) *Draft {
	return &Draft{
		ID:                  r.ID.String(),
		TenantID:            r.TenantID.String(),
		CaseID:              pgUUIDToString(r.CaseID),
		IntimationID:        pgUUIDToString(r.IntimationID),
		PieceType:           r.PieceType,
		PieceProfileKey:     derefString(r.PieceProfileKey),
		Title:               r.Title,
		Content:             derefString(r.Content),
		Status:              r.Status,
		SagaState:           r.SagaState,
		CreatedAt:           timestamptzToTime(r.CreatedAt),
		UpdatedAt:           timestamptzToTime(r.UpdatedAt),
		Tone:                r.Tone,
		Instructions:        derefString(r.Instructions),
		SelectedTheses:      derefStringSlice(r.SelectedTheses),
		StructuredContent:   structuredContentFromJSON(r.StructuredContent),
		Authorship:          r.Authorship,
		FilingNumber:        derefString(r.FilingNumber),
		SupersededAt:        pgTimestamptzPtr(r.SupersededAt),
		SupersededByDraftID: pgUUIDToString(r.SupersededByDraftID),
	}
}

// structuredContentFromJSON decodes the draft.structured_content jsonb column
// into a *StructuredContent. Returns nil when the column is NULL/empty (drafts
// pre-migration 0056 or that haven't been generated yet); the read model falls
// back to the plain-text parser on the fly. A decode fault also returns nil
// (best-effort — a corrupt row should not crash the read model).
func structuredContentFromJSON(raw []byte) *StructuredContent {
	if len(raw) == 0 {
		return nil
	}
	var out StructuredContent
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	// Normalize nil slices to empty so JSON output stays [] (not null) — the FE
	// depends on the shape.
	if out.Sections == nil {
		out.Sections = []StructuredSection{}
	}
	if out.Preamble.Paragraphs == nil {
		out.Preamble.Paragraphs = []string{}
	}
	for i := range out.Sections {
		if out.Sections[i].Paragraphs == nil {
			out.Sections[i].Paragraphs = []string{}
		}
	}
	return &out
}

// structuredContentToJSON marshals a *StructuredContent to jsonb bytes for
// persistence. Returns nil for a nil pointer (writing SQL NULL). An encoding
// error is a programmer fault — logged & swallowed to nil to keep the caller
// path resilient (a caller with a bad struct shouldn't crash the whole write).
func structuredContentToJSON(sc *StructuredContent) []byte {
	if sc == nil {
		return nil
	}
	b, err := json.Marshal(sc)
	if err != nil {
		return nil
	}
	return b
}

// derefStringSlice normalizes a possibly-nil string slice to a non-nil empty
// slice — a text[] NOT NULL DEFAULT '{}' column scans as [] in practice, but
// this keeps the entity's invariant (never nil) explicit at the boundary.
func derefStringSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
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
		ID:                r.ID.String(),
		TenantID:          r.TenantID.String(),
		CaseID:            pgUUIDToString(r.CaseID),
		IntimationID:      pgUUIDToString(r.IntimationID),
		PieceType:         r.PieceType,
		Title:             r.Title,
		Content:           derefString(r.Content),
		Status:            r.Status,
		SagaState:         r.SagaState,
		CreatedAt:         timestamptzToTime(r.CreatedAt),
		UpdatedAt:         timestamptzToTime(r.UpdatedAt),
		StructuredContent: structuredContentFromJSON(r.StructuredContent),
		Authorship:        r.Authorship,
	}
}

// draftFromAuthorshipRow maps the UpdateDraftAuthorship RETURNING row to a *Draft.
func draftFromAuthorshipRow(r draftdb.UpdateDraftAuthorshipRow) *Draft {
	return &Draft{
		ID:                r.ID.String(),
		TenantID:          r.TenantID.String(),
		CaseID:            pgUUIDToString(r.CaseID),
		IntimationID:      pgUUIDToString(r.IntimationID),
		PieceType:         r.PieceType,
		Title:             r.Title,
		Content:           derefString(r.Content),
		Status:            r.Status,
		SagaState:         r.SagaState,
		CreatedAt:         timestamptzToTime(r.CreatedAt),
		UpdatedAt:         timestamptzToTime(r.UpdatedAt),
		StructuredContent: structuredContentFromJSON(r.StructuredContent),
		Authorship:        r.Authorship,
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

// unmarshalCoverageSummary decodes jsonb bytes into a CoverageSummary. The review
// coverage jsonb has more fields than the trimmed summary exposed in list endpoints;
// this extracts only the three fields the client needs. A decode fault returns nil.
func unmarshalCoverageSummary(b []byte) *CoverageSummary {
	c := unmarshalCoverage(b)
	return &CoverageSummary{
		Grounded:         c.Grounded,
		ChunksUsed:       c.ChunksUsed,
		SuggestionsTotal: c.SuggestionsTotal,
	}
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
		counsels := decodeCounsels(r.Counsels)
		out = append(out, PartyInfo{
			Role:     r.Role,
			Name:     r.Name,
			IsClient: r.IsClient,
			Counsel:  firstCounselLabelFrom(counsels),
			Counsels: counsels,
		})
	}
	return out
}

// decodeCounsels parses the jsonb_agg-as-text produced by GetPartiesForDraft.
// A decode failure returns an empty slice (best-effort — a corrupt row should
// not crash the peça pipeline). Never returns nil.
func decodeCounsels(counselsText string) []PartyCounselInfo {
	if counselsText == "" || counselsText == "[]" {
		return []PartyCounselInfo{}
	}
	var recs []counselInfoRaw
	if err := json.Unmarshal([]byte(counselsText), &recs); err != nil {
		return []PartyCounselInfo{}
	}
	out := make([]PartyCounselInfo, 0, len(recs))
	for _, r := range recs {
		out = append(out, PartyCounselInfo{Name: r.Name, OAB: r.OAB, UF: r.UF})
	}
	return out
}

// firstCounselLabelFrom formats the first counsel as "Name (OAB/UF nº oab)".
// Returns "" when the slice is empty or the first entry has no name/OAB.
func firstCounselLabelFrom(counsels []PartyCounselInfo) string {
	if len(counsels) == 0 {
		return ""
	}
	c := counsels[0]
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

// ── Fatia 4 mappers ────────────────────────────────────────────────────────

// draftFromSignRow maps a SignDraft RETURNING row to a *Draft entity.
func draftFromSignRow(r draftdb.SignDraftRow) *Draft {
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

// petitionFromRow maps a Petition db row to a *Petition entity.
func petitionFromRow(r draftdb.Petition) *Petition {
	var result *Petition
	if r.ObservedResult != nil {
		result = &Petition{ObservedResult: *r.ObservedResult}
	}
	p := &Petition{
		ID:            r.ID.String(),
		DraftID:       r.DraftID.String(),
		CourtRecordID: r.CourtRecordID.String(),
		FiledAt:       timestamptzToTime(r.FiledAt),
		Receipt:       unmarshalReceipt(r.Receipt),
	}
	if result != nil {
		p.ObservedResult = result.ObservedResult
	}
	return p
}

// unmarshalReceipt decodes jsonb bytes into map[string]any.
func unmarshalReceipt(b []byte) map[string]any {
	var out map[string]any
	if len(b) == 0 {
		return map[string]any{}
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	if out == nil {
		return map[string]any{}
	}
	return out
}

// draftListItemFromProcessRow maps a ListDraftsByProcessRow to a DraftListItem.
func draftListItemFromProcessRow(r draftdb.ListDraftsByProcessRow) DraftListItem {
	item := DraftListItem{
		ID:        r.ID.String(),
		PieceType: r.PieceType,
		Title:     r.Title,
		Status:    r.Status,
		SagaState: r.SagaState,
		CreatedAt: timestamptzToTime(r.CreatedAt),
	}
	if r.FiledAt.Valid {
		t := timestamptzToTime(r.FiledAt)
		item.FiledAt = &t
	}
	if r.ObservedResult != nil {
		item.ObservedResult = r.ObservedResult
	}
	if len(r.ReviewCoverage) > 0 {
		item.CoverageSummary = unmarshalCoverageSummary(r.ReviewCoverage)
	}
	return item
}

// draftListItemFromAllRow maps a ListDraftsAllRow to a DraftListItem.
func draftListItemFromAllRow(r draftdb.ListDraftsAllRow) DraftListItem {
	item := DraftListItem{
		ID:              r.ID.String(),
		PieceType:       r.PieceType,
		Title:           r.Title,
		Status:          r.Status,
		SagaState:       r.SagaState,
		CreatedAt:       timestamptzToTime(r.CreatedAt),
		CNJNumber:       r.CnjNumber,
		ResponsibleName: r.ResponsibleName,
	}
	if r.SentToSigningAt.Valid {
		t := timestamptzToTime(r.SentToSigningAt)
		item.SentToSigningAt = &t
	}
	if r.SignedAt.Valid {
		t := timestamptzToTime(r.SignedAt)
		item.SignedAt = &t
	}
	// filed_at pode vir do draft (novo) OU do petition (legacy). O primeiro
	// que estiver preenchido ganha.
	if r.DraftFiledAt.Valid {
		t := timestamptzToTime(r.DraftFiledAt)
		item.FiledAt = &t
	} else if r.FiledAt.Valid {
		t := timestamptzToTime(r.FiledAt)
		item.FiledAt = &t
	}
	if r.DeadlineEndDate.Valid {
		t := r.DeadlineEndDate.Time
		item.DeadlineEndDate = &t
		// days_left calculado no Go pra evitar o CASE...NULL::int no SQL
		// (sqlc infere non-null e o scan quebra pra rows sem deadline).
		today := time.Now().UTC().Truncate(24 * time.Hour)
		end := t.UTC().Truncate(24 * time.Hour)
		days := int32(end.Sub(today) / (24 * time.Hour))
		item.DeadlineDaysLeft = &days
	}
	if r.ObservedResult != nil {
		item.ObservedResult = r.ObservedResult
	}
	if len(r.ReviewCoverage) > 0 {
		item.CoverageSummary = unmarshalCoverageSummary(r.ReviewCoverage)
	}
	return item
}

// suggestedThesisFromRow maps a persisted suggested_thesis row (driver types) to
// the SuggestedThesis entity — the mapper boundary where pgtype.* dies: uuid →
// string, nullable source_document_id → "" on NULL, int32 → int, and the text[]
// evidence normalized to a non-nil slice.
func suggestedThesisFromRow(r draftdb.SuggestedThesis) *SuggestedThesis {
	return &SuggestedThesis{
		ID:               r.ID.String(),
		DraftID:          pgUUIDToString(r.DraftID),
		IntimationID:     pgUUIDToString(r.IntimationID),
		Label:            r.Label,
		Confidence:       r.Confidence,
		Reference:        r.Reference,
		Foundation:       r.Foundation,
		Evidence:         nonNilStrings(r.Evidence),
		SourceRef:        int(r.SourceRef),
		SourceDocumentID: pgUUIDToString(r.SourceDocumentID),
		SourcePage:       int(r.SourcePage),
		SourceExcerpt:    r.SourceExcerpt,
		SourceLabel:      r.SourceLabel,
		Grounded:         r.Grounded,
		State:            r.State,
		Position:         int(r.Position),
	}
}
