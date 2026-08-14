package deadline

import (
	"context"
	"encoding/json"

	"github.com/jusassessoria/platform/internal/deadline/deadlinedb"
	"github.com/jusassessoria/platform/lib/database"
)

// suggestion_store.go is the SuggestionStore adapter (feedback loop, camada 1). Unlike the
// pool-backed READ repo, it WRITES, so it opens its own tenant-scoped tx via the unit of
// work (which SETs app.tenant_id → RLS barrier 2) to insert the one provenance row. It is
// deliberately OFF the confirm's write path: the suggester runs on a GET and the persist is
// best-effort (the use case logs and moves on if it fails), so it must not enroll in — nor
// need — the confirm transaction.

// pgSuggestionStore persists suggestion provenance over the unit of work (pool).
type pgSuggestionStore struct {
	uow database.UnitOfWork
}

var _ SuggestionStore = (*pgSuggestionStore)(nil)

// NewSuggestionStore returns the SuggestionStore over the unit of work. Each call writes one
// row inside its own tenant-scoped tx.
func NewSuggestionStore(uow database.UnitOfWork) SuggestionStore { return &pgSuggestionStore{uow: uow} }

// SaveSuggestion persists one suggestion batch and returns its DB-assigned id. The suggested
// tasks are stored as the exact [{title, kind}, …] jsonb the confirm reads back for the delta.
func (s *pgSuggestionStore) SaveSuggestion(ctx context.Context, tenantID string, rec SuggestionRecord) (string, error) {
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return "", err
	}
	deadlineID, err := parseUUID(rec.DeadlineID)
	if err != nil {
		return "", err
	}
	intID, err := parseUUID(rec.IntimationID)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(rec.Tasks)
	if err != nil {
		return "", database.WrapInfra(err)
	}

	var id string
	err = s.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		newID, err := deadlinedb.New(tx).InsertTaskSuggestion(ctx, deadlinedb.InsertTaskSuggestionParams{
			TenantID:      tenant,
			DeadlineID:    deadlineID,
			IntimationID:  intID,
			PromptVersion: rec.PromptVersion,
			Model:         rec.Model,
			Suggested:     payload,
		})
		if err != nil {
			return database.WrapInfra(err)
		}
		id = newID.String()
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}
