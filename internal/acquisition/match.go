package acquisition

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// matchRepo is the narrow port the match use case drives: the national join (read
// system-wide) plus the same per-tenant write the sync cycle uses.
type matchRepo interface {
	MatchPublicationsByDay(ctx context.Context, tx database.Tx, day time.Time) ([]PublicationMatch, error)
	AcquireTenantWriteLock(ctx context.Context, tx database.Tx, tenantID string) error
	FindOrCreateCourtRecord(ctx context.Context, tx database.Tx, params FindOrCreateCourtRecordParams) (*CourtRecord, bool, error)
	UpsertIntimations(ctx context.Context, tx database.Tx, params []IntimationParams) (int, error)
}

// MatchUseCase turns a day's national publications into per-tenant intimações: it
// reads the (tenant, matched OAB, payload) hits system-wide, groups them by tenant,
// re-parses each tenant's matched payloads through the DJEN parser (feeding its
// watched OABs so the recipient matched-flag is honest), and writes the resulting
// court_records + intimações in that tenant's tx — reusing the sync write path. It
// is bulk-native: no per-OAB fetch, so the shared DJEN budget is untouched.
type MatchUseCase struct {
	repo   matchRepo
	uow    database.UnitOfWork
	parser Parser
	now    func() time.Time
}

// NewMatchUseCase wires the match use case with the wall clock.
func NewMatchUseCase(repo matchRepo, uow database.UnitOfWork, parser Parser) *MatchUseCase {
	return &MatchUseCase{repo: repo, uow: uow, parser: parser, now: time.Now}
}

// MatchDay fans a day's firehose out to the watching tenants.
func (uc *MatchUseCase) MatchDay(ctx context.Context, day time.Time) error {
	var matches []PublicationMatch
	if err := uc.uow.DoSystem(ctx, func(tx database.Tx) error {
		var e error
		matches, e = uc.repo.MatchPublicationsByDay(ctx, tx, day)
		return e
	}); err != nil {
		return err
	}

	for tenantID, b := range groupMatches(matches) {
		if err := uc.writeForTenant(ctx, tenantID, b.keys, b.items); err != nil {
			return err
		}
	}
	return nil
}

type tenantMatches struct {
	keys  map[string]bool
	items []json.RawMessage
	seen  map[string]bool
}

// groupMatches buckets the flat match rows by tenant, collecting each tenant's matched
// OAB keys and the DISTINCT payloads: a publication watched by two of a tenant's OABs
// appears once per OAB, so it is deduped here to be parsed and written once.
func groupMatches(matches []PublicationMatch) map[string]*tenantMatches {
	byTenant := make(map[string]*tenantMatches)
	for _, m := range matches {
		b := byTenant[m.TenantID]
		if b == nil {
			b = &tenantMatches{keys: map[string]bool{}, seen: map[string]bool{}}
			byTenant[m.TenantID] = b
		}
		b.keys[m.OABKey] = true
		if s := string(m.Payload); !b.seen[s] {
			b.seen[s] = true
			b.items = append(b.items, m.Payload)
		}
	}
	return byTenant
}

// writeForTenant re-parses a tenant's matched payloads and writes the resulting
// records + intimações in its tx, reusing the sync helpers. The advisory lock keeps
// concurrent tenant writes deadlock-free; the match is not entitlement-gated (like the
// backfill) and carries no sync_run.
func (uc *MatchUseCase) writeForTenant(ctx context.Context, tenantID string, keys map[string]bool, items []json.RawMessage) error {
	body, err := json.Marshal(djenPayload{OABs: oabEntriesFromKeys(keys), Items: items})
	if err != nil {
		return apperr.NewInfra("match: marshal payload", err)
	}
	parsed, err := uc.parser.Parse(ctx, RawPayload{Source: SourceDJEN, Body: body})
	if err != nil {
		return err
	}

	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		if err := uc.repo.AcquireTenantWriteLock(ctx, tx, tenantID); err != nil {
			return err
		}
		records := make(map[string]*CourtRecord, len(parsed.CourtRecords))
		nextSync := uc.now().Add(defaultSyncInterval)
		for _, pr := range parsed.CourtRecords {
			cr, _, err := uc.repo.FindOrCreateCourtRecord(ctx, tx, FindOrCreateCourtRecordParams{
				TenantID:           tenantID,
				CNJNumber:          pr.CNJNumber,
				Degree:             pr.Degree,
				Court:              pr.Court,
				Class:              pr.Class,
				Subject:            pr.Subject,
				Completeness:       pr.Completeness,
				JudgingBody:        pr.JudgingBody,
				NextSyncAt:         nextSync,
				ActiveProcessLimit: math.MaxInt, // bulk match is ungated, like the backfill
				SyncRunID:          "",          // not from a sync run — national ingestion
			})
			if err != nil {
				return err
			}
			records[recordKey(pr.CNJNumber, pr.Degree)] = cr
		}
		intimParams, err := intimationParamsFor(tenantID, "", parsed.Intimations, records, nil)
		if err != nil {
			return err
		}
		if _, err := uc.repo.UpsertIntimations(ctx, tx, intimParams); err != nil {
			return err
		}
		return nil
	})
}

// oabEntriesFromKeys reverses oabKey ("NUMBER|UF") back to OABEntry so the DJEN parser
// keys its watched set identically and marks the matched recipients.
func oabEntriesFromKeys(keys map[string]bool) []OABEntry {
	entries := make([]OABEntry, 0, len(keys))
	for k := range keys {
		if i := strings.IndexByte(k, '|'); i > 0 {
			entries = append(entries, OABEntry{Number: k[:i], UF: k[i+1:]})
		}
	}
	return entries
}
