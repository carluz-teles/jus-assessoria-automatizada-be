package acquisition

import (
	"encoding/json"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition/acquisitiondb"
	"github.com/jusassessoria/platform/lib/database"
)

// mapper.go is the boundary where driver types die: uuid.UUID, pgtype.* and the
// raw jsonb bytes are absorbed here so the entity stays pure. The repository
// returns *Integration, never the sqlc row.

// integrationToEntity decodes one sqlc row into an Integration. The scope jsonb
// is unmarshalled here; a malformed blob is an infra fault (the DB should only
// hold what we wrote), surfaced as a typed infra error.
func integrationToEntity(r acquisitiondb.Integration) (*Integration, error) {
	scope, err := decodeScope(r.Scope)
	if err != nil {
		return nil, err
	}

	ent := &Integration{
		ID:        r.ID.String(),
		TenantID:  r.TenantID.String(),
		Source:    r.Source,
		Scope:     scope,
		Status:    r.Status,
		CreatedAt: r.CreatedAt.Time,
		UpdatedAt: r.UpdatedAt.Time,
	}
	if r.CredentialRef != nil {
		ent.CredentialRef = *r.CredentialRef
	}
	return ent, nil
}

// syncRunToEntity decodes a sync_run row read back by FindSyncRunByEventID into a
// SyncRun. The nullable driver types (court_record_id, finished_at) collapse to
// the entity's zero values when absent (an open run has no finished_at, an OAB
// discovery run no court_record_id).
func syncRunToEntity(r acquisitiondb.FindSyncRunByEventIDRow) *SyncRun {
	run := &SyncRun{
		ID:               r.ID.String(),
		TenantID:         r.TenantID.String(),
		IntegrationID:    r.IntegrationID.String(),
		ConnectorID:      r.ConnectorID,
		ConnectorVersion: r.ConnectorVersion,
		Status:           r.Status,
		ItemsNew:         int(r.ItemsNew),
		ItemsDeduped:     int(r.ItemsDeduped),
		StartedAt:        r.StartedAt.Time,
		FinishedAt:       r.FinishedAt.Time,
	}
	if r.CourtRecordID.Valid {
		run.CourtRecordID = uuid.UUID(r.CourtRecordID.Bytes).String()
	}
	return run
}

// decodeScope unmarshals the jsonb scope column. An empty/NULL blob yields the
// zero Scope rather than an error.
func decodeScope(raw []byte) (Scope, error) {
	var scope Scope
	if len(raw) == 0 {
		return scope, nil
	}
	if err := json.Unmarshal(raw, &scope); err != nil {
		return Scope{}, database.WrapInfra(err)
	}
	return scope, nil
}

// encodeScope marshals a Scope to the jsonb bytes the upsert stores. A struct of
// two string slices cannot fail to marshal in practice, but the error is still
// surfaced as infra rather than dropped.
func encodeScope(scope Scope) ([]byte, error) {
	raw, err := json.Marshal(scope)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return raw, nil
}
