package acquisition

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/obs"
)

// matchRepo is the narrow port the match use case drives: the national join (read
// system-wide) plus the same per-tenant write the sync cycle uses.
type matchRepo interface {
	MatchPublicationsByDay(ctx context.Context, tx database.Tx, day time.Time) ([]PublicationMatch, error)
	MatchPublicationsForTenantSince(ctx context.Context, tx database.Tx, tenantID string, since time.Time) ([]PublicationMatch, error)
	AcquireTenantWriteLock(ctx context.Context, tx database.Tx, tenantID string) error
	BatchUpsertCourtRecords(ctx context.Context, tx database.Tx, tenantID string, activeLimit int, params []FindOrCreateCourtRecordParams) (outcomes []CourtRecordOutcome, newCount int, err error)
	UpsertIntimations(ctx context.Context, tx database.Tx, params []IntimationParams) (newRows, cancelledRows []IntimationChange, err error)
	WriteDailyCaptureRun(ctx context.Context, tx database.Tx, params DailyCaptureParams) error
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
func (uc *MatchUseCase) MatchDay(ctx context.Context, day time.Time) (err error) {
	// A daily import root: the match tick has no upstream request, so it is its own
	// trace — the span parents the system read + per-tenant writes (otelpgx would emit
	// orphan query spans otherwise) and records how much of the firehose it folded in.
	ctx, span := obs.Start(ctx, "scheduler match_day", trace.WithSpanKind(trace.SpanKindInternal))
	matched := 0
	defer func() {
		span.SetAttributes(attribute.String("day", day.Format(dateLayout)), attribute.Int("matched", matched))
		obs.Record(span, err)
		span.End()
	}()

	var matches []PublicationMatch
	if err = uc.uow.DoSystem(ctx, func(tx database.Tx) error {
		var e error
		matches, e = uc.repo.MatchPublicationsByDay(ctx, tx, day)
		return e
	}); err != nil {
		return err
	}
	matched = len(matches)
	recordMatch(ctx, matched)

	for tenantID, b := range groupMatches(matches) {
		if err = uc.writeForTenant(ctx, tenantID, day, b.keys, b.items); err != nil {
			return err
		}
	}
	return nil
}

// MatchTenantSince catches ONE tenant up against the already-stored firehose from a
// date forward — the onboarding cutover: a new integration's watched OABs are matched
// against the bootstrap history, so the client sees its recent intimações immediately
// with zero per-OAB DJEN calls (the daily MatchDay covers everything from tomorrow).
func (uc *MatchUseCase) MatchTenantSince(ctx context.Context, tenantID string, since time.Time) error {
	var matches []PublicationMatch
	if err := uc.uow.DoSystem(ctx, func(tx database.Tx) error {
		var e error
		matches, e = uc.repo.MatchPublicationsForTenantSince(ctx, tx, tenantID, since)
		return e
	}); err != nil {
		return err
	}
	b := groupMatches(matches)[tenantID]
	if b == nil {
		return nil // nothing in the store matches this tenant's OABs yet
	}
	// The catch-up write lands NOW, so the capture is stamped on today's window (the
	// day the tenant actually sees "+N proc" from the bootstrap history). `since` bounds
	// which stored publications were folded in, not when the effect happened — using it
	// as the window would key the capture on a past day the fan-out never ran on.
	return uc.writeForTenant(ctx, tenantID, uc.now(), b.keys, b.items)
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
func (uc *MatchUseCase) writeForTenant(ctx context.Context, tenantID string, window time.Time, keys map[string]bool, items []json.RawMessage) error {
	body, err := json.Marshal(djenPayload{OABs: oabEntriesFromKeys(keys), Items: items})
	if err != nil {
		return apperr.NewInfra("match: marshal payload", err)
	}
	parsed, err := uc.parser.Parse(ctx, RawPayload{Source: SourceDJEN, Body: body})
	if err != nil {
		return err
	}

	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		startedAt := uc.now()
		if err := uc.repo.AcquireTenantWriteLock(ctx, tx, tenantID); err != nil {
			return err
		}
		nextSync := uc.now().Add(defaultSyncInterval)
		crParams := make([]FindOrCreateCourtRecordParams, len(parsed.CourtRecords))
		for i, pr := range parsed.CourtRecords {
			crParams[i] = FindOrCreateCourtRecordParams{
				TenantID:     tenantID,
				CNJNumber:    pr.CNJNumber,
				Degree:       pr.Degree,
				Court:        pr.Court,
				Class:        pr.Class,
				Subject:      pr.Subject,
				Completeness: pr.Completeness,
				JudgingBody:  pr.JudgingBody,
				NextSyncAt:   nextSync,
				SyncRunID:    "", // not from a sync run — national ingestion
			}
		}
		// One set-based resolve-or-create; bulk match is ungated (math.MaxInt), like the
		// backfill, so nothing comes back Blocked.
		outcomes, newCount, err := uc.repo.BatchUpsertCourtRecords(ctx, tx, tenantID, math.MaxInt, crParams)
		if err != nil {
			return err
		}
		records := make(map[string]*CourtRecord, len(outcomes))
		for i, o := range outcomes {
			pr := parsed.CourtRecords[i]
			records[recordKey(pr.CNJNumber, pr.Degree)] = o.Record
		}
		intimParams, err := intimationParamsFor(tenantID, "", parsed.Intimations, records, nil)
		if err != nil {
			return err
		}
		// National bulk match is a separate producer; it does not emit the
		// intimation.observed/cancelled events the sync slice does (a future slice may),
		// so the cancelled slice is discarded — but the NEW slice count is the honest
		// intimations_new for the capture audit row below.
		newIntim, _, err := uc.repo.UpsertIntimations(ctx, tx, intimParams)
		if err != nil {
			return err
		}
		// MatchDay re-ticks hourly over the same day (at-least-once + lookback window):
		// a bare re-observation re-resolves every record but lands NO new intimation. So
		// skip the audit write when nothing is net-new — otherwise the ON CONFLICT sum
		// inflates the counters and drags finished_at across the whole day (duration lie).
		if newCount == 0 && len(newIntim) == 0 {
			return nil
		}
		// "Processos atualizados" is the honest, re-tick-safe quantity: DISTINCT already-
		// known records that got a NEW intimation this tick (new intimations only land
		// once, so a re-tick contributes 0). New records are court_records_new, not
		// updated — subtract them out (clamped, since a new record's only intimation may
		// have deduped). len(outcomes) would recount every re-observed record → inflation.
		touched := make(map[string]struct{}, len(newIntim))
		for _, in := range newIntim {
			touched[in.CourtRecordID] = struct{}{}
		}
		updated := len(touched) - newCount
		if updated < 0 {
			updated = 0
		}
		// Audit this fan-out's per-tenant effect in the SAME tx.
		return uc.repo.WriteDailyCaptureRun(ctx, tx, DailyCaptureParams{
			TenantID:            tenantID,
			Window:              window,
			StartedAt:           startedAt,
			FinishedAt:          uc.now(),
			CourtRecordsNew:     newCount,
			CourtRecordsUpdated: updated,
			IntimationsNew:      len(newIntim),
		})
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
