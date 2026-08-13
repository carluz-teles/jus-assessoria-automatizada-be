package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition/acquisitiondb"
	"github.com/jusassessoria/platform/lib/database"
)

// repository_batch.go is the SET-BASED write path for a sync/match window. The naive
// path did ~5 DB round-trips PER court record (get + count + insert case + insert
// record + mark-synced) plus 1 per intimation, all serialized under the per-tenant
// advisory lock — so a big OAB window held the lock for seconds and the 12 concurrent
// windows queued behind it. These methods collapse a whole window into a handful of
// jsonb_to_recordset statements, so the lock is held for milliseconds and the windows
// serialize cheaply. The FETCH already ran outside the tx (uTLS, no 429); this makes
// the WRITE stop being the bottleneck.

// CourtRecordOutcome is the per-input-record result of BatchUpsertCourtRecords, parallel
// to the input params slice: Record is the resolved court_record (nil only when Blocked),
// Blocked marks a brand-new record refused by the entitlement ceiling.
type CourtRecordOutcome struct {
	Record  *CourtRecord
	Blocked bool
}

// courtRecordRowJSON is one row of the BatchInsertCourtRecords jsonb payload. Nullable
// columns use *string so a nil marshals to JSON null → SQL NULL (jsonb_to_recordset).
type courtRecordRowJSON struct {
	CaseID       string    `json:"case_id"`
	CNJNumber    string    `json:"cnj_number"`
	Degree       string    `json:"degree"`
	Court        string    `json:"court"`
	Class        *string   `json:"class"`
	Subject      *string   `json:"subject"`
	Completeness float32   `json:"completeness"`
	JudgingBody  *string   `json:"judging_body"`
	SyncRunID    *string   `json:"sync_run_id"`
	NextSyncAt   time.Time `json:"next_sync_at"`
}

type keyRowJSON struct {
	CNJNumber string `json:"cnj_number"`
	Degree    string `json:"degree"`
}

type markSyncedRowJSON struct {
	ID           string    `json:"id"`
	Completeness float32   `json:"completeness"`
	NextSyncAt   time.Time `json:"next_sync_at"`
	JudgingBody  *string   `json:"judging_body"`
}

// BatchUpsertCourtRecords resolves-or-creates every record of a window in a handful of
// statements: ONE find-by-keys, ONE active-count (only if creating), ONE bulk case
// insert, ONE bulk record insert, ONE bulk mark-synced. It returns outcomes parallel to
// params (so the caller maps ids back and skips blocked children) and the count of newly
// created records. activeLimit gates brand-new records: once the tenant's ACTIVE count
// reaches it, the remaining new records are Blocked (the backfill passes math.MaxInt to
// stay ungated). Duplicate keys within the batch resolve to the same record.
func (r *pgRepository) BatchUpsertCourtRecords(ctx context.Context, tx database.Tx, tenantID string, activeLimit int, params []FindOrCreateCourtRecordParams) ([]CourtRecordOutcome, int, error) {
	outcomes := make([]CourtRecordOutcome, len(params))
	if len(params) == 0 {
		return outcomes, 0, nil
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, 0, database.WrapInfra(err)
	}
	q := acquisitiondb.New(tx)

	// 1. Find which of these keys already exist (one round-trip). resolved holds the
	// entity per natural key; a param whose key is here is a reobservation (update path).
	uniqueKeys := make([]keyRowJSON, 0, len(params))
	seenKey := make(map[string]bool, len(params))
	for _, p := range params {
		k := recordKey(p.CNJNumber, p.Degree)
		if seenKey[k] {
			continue
		}
		seenKey[k] = true
		uniqueKeys = append(uniqueKeys, keyRowJSON{CNJNumber: p.CNJNumber, Degree: p.Degree})
	}
	keysJSON, err := json.Marshal(uniqueKeys)
	if err != nil {
		return nil, 0, database.WrapInfra(err)
	}
	existingRows, err := q.GetCourtRecordsByKeys(ctx, acquisitiondb.GetCourtRecordsByKeysParams{TenantID: tid, Keys: keysJSON})
	if err != nil {
		return nil, 0, database.WrapInfra(err)
	}
	resolved := make(map[string]*CourtRecord, len(params))     // key → entity (existing or freshly created)
	caseByKey := make(map[string]uuid.UUID, len(existingRows)) // key → its case id (existing)
	idByKey := make(map[string]uuid.UUID, len(existingRows))   // key → its record id (existing)
	for _, row := range existingRows {
		k := recordKey(row.CnjNumber, row.Degree)
		caseByKey[k] = row.CaseID
		idByKey[k] = row.ID
	}

	// 2. Partition new (first-seen key not in existing) preserving first-seen order; the
	// rest are updates. paramByKey keeps ONE representative param per key (the latest wins
	// for completeness/judging_body, mirroring the last-write of the old loop).
	newKeys := make([]string, 0)
	updateKeys := make([]string, 0)
	paramByKey := make(map[string]FindOrCreateCourtRecordParams, len(params))
	for _, p := range params {
		k := recordKey(p.CNJNumber, p.Degree)
		if _, ok := paramByKey[k]; !ok {
			if _, exists := caseByKey[k]; exists {
				updateKeys = append(updateKeys, k)
			} else {
				newKeys = append(newKeys, k)
			}
		}
		paramByKey[k] = p
	}

	// 3. Entitlement gate: count the tenant's ACTIVE records ONCE, then let only
	// (limit - count) new records through; the rest are blocked. math.MaxInt (backfill)
	// makes remaining huge, so nothing is blocked.
	newCount := 0
	blockedKeys := make(map[string]bool)
	if len(newKeys) > 0 {
		active, cerr := q.CountActiveCourtRecordsByTenant(ctx, tid)
		if cerr != nil {
			return nil, 0, database.WrapInfra(cerr)
		}
		remaining := activeLimit - int(active)
		toCreate := newKeys
		if remaining < 0 {
			remaining = 0
		}
		if len(toCreate) > remaining {
			for _, k := range toCreate[remaining:] {
				blockedKeys[k] = true
			}
			toCreate = toCreate[:remaining]
		}
		if len(toCreate) > 0 {
			created, cerr := r.batchCreateCourtRecords(ctx, q, tid, toCreate, paramByKey, resolved)
			if cerr != nil {
				return nil, 0, cerr
			}
			newCount = created
		}
	}

	// 4. Bulk mark-synced for the reobserved (existing) records; resolve their entities.
	if len(updateKeys) > 0 {
		updates := make([]markSyncedRowJSON, 0, len(updateKeys))
		for _, k := range updateKeys {
			p := paramByKey[k]
			updates = append(updates, markSyncedRowJSON{
				ID:           idByKey[k].String(),
				Completeness: p.Completeness,
				NextSyncAt:   p.NextSyncAt,
				JudgingBody:  strPtrOrNil(p.JudgingBody),
			})
			resolved[k] = newCourtRecordEntity(idByKey[k], caseByKey[k], p)
		}
		updJSON, merr := json.Marshal(updates)
		if merr != nil {
			return nil, 0, database.WrapInfra(merr)
		}
		if uerr := q.BatchMarkCourtRecordsSynced(ctx, updJSON); uerr != nil {
			return nil, 0, database.WrapInfra(uerr)
		}
	}

	// 5. Assemble outcomes parallel to input params.
	for i, p := range params {
		k := recordKey(p.CNJNumber, p.Degree)
		if blockedKeys[k] {
			outcomes[i] = CourtRecordOutcome{Blocked: true}
			continue
		}
		outcomes[i] = CourtRecordOutcome{Record: resolved[k]}
	}
	return outcomes, newCount, nil
}

// batchCreateCourtRecords inserts one fresh case per new key and the records in two bulk
// statements, folding the post-sync fields into the record insert. It fills resolved
// with the created entities and returns how many rows were actually created.
func (r *pgRepository) batchCreateCourtRecords(ctx context.Context, q *acquisitiondb.Queries, tid uuid.UUID, toCreate []string, paramByKey map[string]FindOrCreateCourtRecordParams, resolved map[string]*CourtRecord) (int, error) {
	caseIDs, err := q.BatchInsertCourtCases(ctx, acquisitiondb.BatchInsertCourtCasesParams{TenantID: tid, N: int32(len(toCreate))})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	if len(caseIDs) != len(toCreate) {
		return 0, database.WrapInfra(errShortCaseInsert)
	}
	rows := make([]courtRecordRowJSON, len(toCreate))
	caseByKey := make(map[string]uuid.UUID, len(toCreate))
	for i, k := range toCreate {
		p := paramByKey[k]
		caseByKey[k] = caseIDs[i]
		rows[i] = courtRecordRowJSON{
			CaseID:       caseIDs[i].String(),
			CNJNumber:    p.CNJNumber,
			Degree:       p.Degree,
			Court:        p.Court,
			Class:        strPtrOrNil(p.Class),
			Subject:      strPtrOrNil(p.Subject),
			Completeness: p.Completeness,
			JudgingBody:  strPtrOrNil(p.JudgingBody),
			SyncRunID:    strPtrOrNil(p.SyncRunID),
			NextSyncAt:   p.NextSyncAt,
		}
	}
	recJSON, err := json.Marshal(rows)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	inserted, err := q.BatchInsertCourtRecords(ctx, acquisitiondb.BatchInsertCourtRecordsParams{TenantID: tid, Records: recJSON})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	for _, row := range inserted {
		k := recordKey(row.CnjNumber, row.Degree)
		resolved[k] = newCourtRecordEntity(row.ID, caseByKey[k], paramByKey[k])
	}
	return len(inserted), nil
}

// errShortCaseInsert guards the invariant that BatchInsertCourtCases returns exactly one
// id per requested case; a mismatch means the pairing with new records would be wrong.
var errShortCaseInsert = errors.New("batch insert court cases returned fewer ids than requested")

// intimationRowJSON is one row of the BatchUpsertIntimations jsonb payload. The
// discovery dates (made_available/published/deadline_start) are always present (the
// parser fills them); cancelled_at and the nullable texts collapse to JSON null.
type intimationRowJSON struct {
	CaseID          string          `json:"case_id"`
	CourtRecordID   string          `json:"court_record_id"`
	Hash            string          `json:"hash"`
	MadeAvailableAt string          `json:"made_available_at"`
	PublishedAt     string          `json:"published_at"`
	DeadlineStartAt string          `json:"deadline_start_at"`
	Content         string          `json:"content"`
	Source          string          `json:"source"`
	Type            *string         `json:"type"`
	Status          string          `json:"status"`
	SourceURL       *string         `json:"source_url"`
	CancelledAt     *string         `json:"cancelled_at"`
	CancelReason    *string         `json:"cancel_reason"`
	Recipients      json.RawMessage `json:"recipients"`
	SyncRunID       *string         `json:"sync_run_id"`
}

// UpsertIntimations bulk-upserts a window's intimações in ONE round-trip via a jsonb
// payload, replacing the per-intimation InsertIntimation loop. Same ON CONFLICT semantics
// (a retracted publication re-arrives with the same hash carrying CANCELLED). It returns
// the rows event-worthy for the producer: newRows are the FIRST inserts (xmax = 0 →
// intimation.observed) and cancelledRows are those that transitioned to CANCELLED on
// this upsert (old_status <> CANCELLED, new status = CANCELLED → intimation.cancelled).
// A plain re-observation (deduped, no relevant change) appears in neither. Court is
// carried on every row (joined from court_record) so the producer denormalizes uf.
func (r *pgRepository) UpsertIntimations(ctx context.Context, tx database.Tx, params []IntimationParams) (newRows, cancelledRows []IntimationChange, err error) {
	if len(params) == 0 {
		return nil, nil, nil
	}
	tid, err := uuid.Parse(params[0].TenantID)
	if err != nil {
		return nil, nil, database.WrapInfra(err)
	}
	rows := make([]intimationRowJSON, len(params))
	for i, p := range params {
		rows[i] = intimationRowJSON{
			CaseID:          p.CaseID,
			CourtRecordID:   p.CourtRecordID,
			Hash:            p.Hash,
			MadeAvailableAt: dateStr(p.MadeAvailableAt),
			PublishedAt:     dateStr(p.PublishedAt),
			DeadlineStartAt: dateStr(p.DeadlineStartAt),
			Content:         p.Content,
			Source:          p.Source,
			Type:            strPtrOrNil(p.Type),
			Status:          p.Status,
			SourceURL:       strPtrOrNil(p.SourceURL),
			CancelledAt:     dateStrOrNil(p.CancelledAt),
			CancelReason:    strPtrOrNil(p.CancelReason),
			Recipients:      rawArrayOrEmpty(p.Recipients),
			SyncRunID:       strPtrOrNil(p.SyncRunID),
		}
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return nil, nil, database.WrapInfra(err)
	}
	upserted, err := acquisitiondb.New(tx).BatchUpsertIntimations(ctx, acquisitiondb.BatchUpsertIntimationsParams{
		TenantID: tid, Intimations: payload,
	})
	if err != nil {
		return nil, nil, database.WrapInfra(err)
	}
	for _, row := range upserted {
		change := IntimationChange{
			ID:              row.ID.String(),
			CourtRecordID:   row.CourtRecordID.String(),
			CaseID:          row.CaseID.String(),
			Type:            derefString(row.Type),
			Court:           row.Court,
			DeadlineStartAt: dateOrEmpty(row.DeadlineStartAt),
			CancelReason:    derefString(row.CancelReason),
		}
		switch {
		case row.Inserted:
			// First insert: observed only if still active. A row that arrives
			// dead-on-arrival (fresh INSERT already carrying CANCELLED — e.g. the DJEN
			// retracts a publication inside the same window we first ingest it) emits
			// NOTHING. There is no deadline to open (skip observed) and none to revoke
			// (no prior ACTIVE state → not a transition; gating on row.Inserted here
			// also keeps it out of the cancelled case below, whose old_status is '').
			if row.Status != IntimationStatusCancelled {
				newRows = append(newRows, change)
			}
		case row.OldStatus != IntimationStatusCancelled && row.Status == IntimationStatusCancelled:
			cancelledRows = append(cancelledRows, change)
		}
	}
	return newRows, cancelledRows, nil
}

// partyRowJSON is one row of the BatchUpsertParties jsonb payload.
type partyRowJSON struct {
	CaseID string `json:"case_id"`
	Role   string `json:"role"`
	Name   string `json:"name"`
}

// partyCounselRowJSON is one row of the BatchUpsertPartyCounsels jsonb payload.
type partyCounselRowJSON struct {
	PartyID string `json:"party_id"`
	Name    string `json:"name"`
	OAB     string `json:"oab"`
	UF      string `json:"uf"`
}

// UpsertParties materializes a window's partes (autor/réu/terceiro + advogados) in the
// caller's tx: ONE bulk party upsert (idempotent on tenant+case+role+name, returning
// each row's id whether inserted or pre-existing), then ONE bulk counsel upsert keyed to
// those ids (idempotent on tenant+party+oab+uf). A re-observation of the same
// communication re-lands the exact same rows — no duplicates, no dedup breakage. Parties
// with the same (case, role, name) within the batch collapse to one row; their counsels
// all attach to that one id.
func (r *pgRepository) UpsertParties(ctx context.Context, tx database.Tx, params []PartyParams) error {
	if len(params) == 0 {
		return nil
	}
	tid, err := uuid.Parse(params[0].TenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	q := acquisitiondb.New(tx)

	// 1. Bulk-upsert the parties (dedup identical (case, role, name) within the batch so
	// jsonb_to_recordset never feeds ON CONFLICT the same row twice in one statement,
	// which Postgres rejects — "affect row a second time").
	rows := make([]partyRowJSON, 0, len(params))
	seen := make(map[string]bool, len(params))
	for _, p := range params {
		k := p.CaseID + "\x00" + p.Role + "\x00" + p.Name
		if seen[k] {
			continue
		}
		seen[k] = true
		rows = append(rows, partyRowJSON{CaseID: p.CaseID, Role: p.Role, Name: p.Name})
	}
	partyJSON, err := json.Marshal(rows)
	if err != nil {
		return database.WrapInfra(err)
	}
	upserted, err := q.BatchUpsertParties(ctx, acquisitiondb.BatchUpsertPartiesParams{TenantID: tid, Parties: partyJSON})
	if err != nil {
		return database.WrapInfra(err)
	}

	// 2. Map each returned party back to its id by natural key, then attach every input
	// party's counsels to that id. Dedup identical (party, oab, uf) within the batch for
	// the same reason as above.
	idByKey := make(map[string]uuid.UUID, len(upserted))
	for _, row := range upserted {
		idByKey[row.CaseID.String()+"\x00"+row.Role+"\x00"+row.Name] = row.ID
	}
	counselRows := make([]partyCounselRowJSON, 0)
	seenCounsel := make(map[string]bool)
	for _, p := range params {
		partyID, ok := idByKey[p.CaseID+"\x00"+p.Role+"\x00"+p.Name]
		if !ok {
			continue // defensive: the upsert returns every key, so this cannot happen
		}
		for _, c := range p.Counsels {
			k := partyID.String() + "\x00" + oabKey(c.OAB, c.UF)
			if seenCounsel[k] {
				continue
			}
			seenCounsel[k] = true
			counselRows = append(counselRows, partyCounselRowJSON{
				PartyID: partyID.String(),
				Name:    c.Name,
				OAB:     c.OAB,
				UF:      c.UF,
			})
		}
	}
	if len(counselRows) == 0 {
		return nil
	}
	counselJSON, err := json.Marshal(counselRows)
	if err != nil {
		return database.WrapInfra(err)
	}
	if err := q.BatchUpsertPartyCounsels(ctx, acquisitiondb.BatchUpsertPartyCounselsParams{TenantID: tid, Counsels: counselJSON}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// rawArrayOrEmpty embeds the parser's recipients JSON array as-is (jsonb_to_recordset
// maps a JSON string array to text[]); an empty/absent value becomes [] (→ empty text[],
// never NULL), matching recipientsOrEmpty's empty-array-on-nil behavior.
func rawArrayOrEmpty(rm json.RawMessage) json.RawMessage {
	if len(rm) == 0 {
		return json.RawMessage("[]")
	}
	return rm
}

// docketEntryRowJSON is one row of the BatchInsertDocketEntries jsonb payload. tpu_code
// (0 → null) and complements (empty → null) mirror the per-row nullInt32/complementsOrNil.
type docketEntryRowJSON struct {
	CourtRecordID string          `json:"court_record_id"`
	Hash          string          `json:"hash"`
	OccurredAt    time.Time       `json:"occurred_at"`
	ObservedAt    time.Time       `json:"observed_at"`
	Source        string          `json:"source"`
	Fidelity      int             `json:"fidelity"`
	TPUCode       *int            `json:"tpu_code"`
	Complements   json.RawMessage `json:"complements"`
	Text          string          `json:"text"`
}

// UpsertDocketEntries bulk-inserts a window's andamentos in ONE round-trip via a jsonb
// payload (replacing the per-entry InsertDocketEntry loop), ON CONFLICT DO NOTHING. Only
// the ACTUALLY new entries are returned (in input order, deduped keys dropped), so the
// enrichment/sync use case emits an observed event only for those.
func (r *pgRepository) UpsertDocketEntries(ctx context.Context, tx database.Tx, params []DocketEntryParams) ([]DocketEntry, error) {
	if len(params) == 0 {
		return nil, nil
	}
	rows := make([]docketEntryRowJSON, len(params))
	for i, p := range params {
		rows[i] = docketEntryRowJSON{
			CourtRecordID: p.CourtRecordID,
			Hash:          p.Hash,
			OccurredAt:    p.OccurredAt,
			ObservedAt:    p.ObservedAt,
			Source:        p.Source,
			Fidelity:      p.Fidelity,
			TPUCode:       intPtrOrNil(p.TPUCode),
			Complements:   rawOrNull(p.Complements),
			Text:          p.Text,
		}
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	inserted, err := acquisitiondb.New(tx).BatchInsertDocketEntries(ctx, payload)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	// Map the returned NEW rows (DO NOTHING omits conflicts) back to their params, in
	// input order; a duplicate key within the batch is consumed once (deduped after).
	insertedIDByKey := make(map[string]uuid.UUID, len(inserted))
	for _, row := range inserted {
		insertedIDByKey[row.CourtRecordID.String()+"|"+row.Hash] = row.ID
	}
	newEntries := make([]DocketEntry, 0, len(inserted))
	for _, p := range params {
		k := p.CourtRecordID + "|" + p.Hash
		id, ok := insertedIDByKey[k]
		if !ok {
			continue // conflicted → deduped
		}
		delete(insertedIDByKey, k)
		newEntries = append(newEntries, DocketEntry{
			ID:            id.String(),
			CourtRecordID: p.CourtRecordID,
			Hash:          p.Hash,
			OccurredAt:    p.OccurredAt,
			ObservedAt:    p.ObservedAt,
			Source:        p.Source,
			Fidelity:      p.Fidelity,
			Text:          p.Text,
		})
	}
	return newEntries, nil
}

// intPtrOrNil maps 0 to nil (JSON null → SQL NULL), mirroring nullInt32.
func intPtrOrNil(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// rawOrNull maps an empty raw message to nil (JSON null → SQL NULL), mirroring complementsOrNil.
func rawOrNull(rm json.RawMessage) json.RawMessage {
	if len(rm) == 0 {
		return nil
	}
	return rm
}

// dateStr formats a date for the jsonb payload (date column parses YYYY-MM-DD).
func dateStr(t time.Time) string { return t.Format("2006-01-02") }

// dateStrOrNil is dateStr but yields nil (JSON null → SQL NULL) for a zero time.
func dateStrOrNil(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.Format("2006-01-02")
	return &s
}

// strPtrOrNil maps an empty string to nil (JSON null → SQL NULL), preserving the
// NULL-when-absent semantics the per-row nullString helpers gave.
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
