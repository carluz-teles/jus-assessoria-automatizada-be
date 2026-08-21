package main

import (
	"context"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// Retention windows for the two predicates. Kept as SQL interval literals (not bind
// params) so they read like the policy: 30 days of intimation history, 12 months of
// processo dormancy.
const (
	intervalNoise   = "interval '30 days'"
	intervalDormant = "interval '12 months'"
)

// pruneInTx computes both predicates and deletes (or, when dryRun, only counts via a
// rolled-back transaction) child-first for ONE tenant, inside the caller's RLS-scoped
// tx. It returns the per-table tally.
//
// The plan materializes the two scope sets into temp tables first, so every downstream
// DELETE/COUNT joins against a fixed id set (order-independent, re-entrant):
//
//	del_intim   — Predicate A: noise intimations (COMUNICACAO or > 30d old).
//	del_cr      — Predicate B, computed AFTER A is applied: court_records with no living
//	              signal in the last 12 months (or none at all once A's rows are gone).
//	del_case    — court_cases left with zero court_records once del_cr is removed.
//
// Everything the delete touches is reachable from these three sets. Tables the spec
// pins as untouched (processed_event, outbox, capture_run) are never referenced.
//
// The DELETE order is child-first against NO ACTION FKs; even the ON DELETE CASCADE
// edges (task_item, task_suggestion, party_counsel, draft_attachment) are deleted
// explicitly so the counts are exact and the plan does not rely on cascade side effects.
func pruneInTx(ctx context.Context, tx database.Tx, dryRun bool) (counts, error) {
	if err := buildScopeSets(ctx, tx); err != nil {
		return counts{}, err
	}

	// Child-first deletes. Each step counts the rows it removes; on a dry run the
	// enclosing transaction is rolled back by the caller, so these are effectively
	// "count what WOULD be deleted" (the temp tables and the DELETE ... affected-rows
	// give the same tally as the real run would).
	var c counts
	steps := []struct {
		field *int64
		sql   string
	}{
		// ── Predicate A: the noise intimations and only what hangs off THEM ──────────
		// A noise intimation may anchor a deadline (deadline.notification_id) and tasks
		// derived from it; delete those first, then the intimations. Court records stay
		// (a live processo can carry an old intimation) — Predicate B decides records.
		{&c.TaskItems, `DELETE FROM task_item WHERE task_id IN (
			SELECT id FROM task WHERE intimation_id IN (SELECT id FROM del_intim)
			   OR deadline_id IN (SELECT id FROM deadline WHERE notification_id IN (SELECT id FROM del_intim)))`},
		{&c.Tasks, `DELETE FROM task WHERE intimation_id IN (SELECT id FROM del_intim)
			   OR deadline_id IN (SELECT id FROM deadline WHERE notification_id IN (SELECT id FROM del_intim))`},
		{&c.TaskSuggestions, `DELETE FROM task_suggestion WHERE intimation_id IN (SELECT id FROM del_intim)
			   OR deadline_id IN (SELECT id FROM deadline WHERE notification_id IN (SELECT id FROM del_intim))`},
		{&c.Deadlines, `DELETE FROM deadline WHERE notification_id IN (SELECT id FROM del_intim)`},
		// Drafts authored FROM a noise intimation (draft.intimation_id) are ingestion
		// byproducts; delete their children then the drafts. A draft with only a case_id
		// (user-authored, no intimation link) is left alone.
		{&c.DraftAttachments, `DELETE FROM draft_attachment WHERE draft_id IN (
			SELECT id FROM draft WHERE intimation_id IN (SELECT id FROM del_intim))`},
		{&c.Petitions, `DELETE FROM petition WHERE draft_id IN (
			SELECT id FROM draft WHERE intimation_id IN (SELECT id FROM del_intim))`},
		{&c.Reviews, `DELETE FROM review WHERE draft_id IN (
			SELECT id FROM draft WHERE intimation_id IN (SELECT id FROM del_intim))`},
		{&c.Drafts, `DELETE FROM draft WHERE intimation_id IN (SELECT id FROM del_intim)`},
		{&c.Intimations, `DELETE FROM intimation WHERE id IN (SELECT id FROM del_intim)`},

		// ── Predicate B: the dormant court_records and every child reachable from them ─
		// Tasks/deadlines/drafts tied to a dormant record (by court_record_id, or through
		// an intimation/petition of that record) go first, then the leaf ingestion tables,
		// then the records, then the emptied cases.
		{&c.TaskItems, `DELETE FROM task_item WHERE task_id IN (
			SELECT id FROM task WHERE court_record_id IN (SELECT id FROM del_cr))`},
		{&c.Tasks, `DELETE FROM task WHERE court_record_id IN (SELECT id FROM del_cr)`},
		{&c.TaskSuggestions, `DELETE FROM task_suggestion WHERE deadline_id IN (
			SELECT id FROM deadline WHERE court_record_id IN (SELECT id FROM del_cr))`},
		{&c.Deadlines, `DELETE FROM deadline WHERE court_record_id IN (SELECT id FROM del_cr)`},
		{&c.DraftAttachments, `DELETE FROM draft_attachment WHERE draft_id IN (
			SELECT id FROM draft WHERE intimation_id IN (
				SELECT id FROM intimation WHERE court_record_id IN (SELECT id FROM del_cr)))`},
		{&c.Petitions, `DELETE FROM petition WHERE court_record_id IN (SELECT id FROM del_cr)`},
		{&c.Reviews, `DELETE FROM review WHERE draft_id IN (
			SELECT id FROM draft WHERE intimation_id IN (
				SELECT id FROM intimation WHERE court_record_id IN (SELECT id FROM del_cr)))`},
		{&c.Drafts, `DELETE FROM draft WHERE intimation_id IN (
			SELECT id FROM intimation WHERE court_record_id IN (SELECT id FROM del_cr))`},
		// document → chunk (chunk has no tenant_id, reached via document); delete chunks
		// then documents of the dormant records.
		{nil, `DELETE FROM chunk WHERE document_id IN (
			SELECT id FROM document WHERE court_record_id IN (SELECT id FROM del_cr))`},
		{&c.Documents, `DELETE FROM document WHERE court_record_id IN (SELECT id FROM del_cr)`},
		{&c.DocketEntries, `DELETE FROM docket_entry WHERE court_record_id IN (SELECT id FROM del_cr)`},
		{&c.Intimations, `DELETE FROM intimation WHERE court_record_id IN (SELECT id FROM del_cr)`},
		// party/party_counsel are case-scoped; delete only the counsels+parties of cases
		// that are about to be emptied (all their records dormant → in del_case).
		{&c.PartyCounsels, `DELETE FROM party_counsel WHERE party_id IN (
			SELECT id FROM party WHERE case_id IN (SELECT id FROM del_case))`},
		{&c.Parties, `DELETE FROM party WHERE case_id IN (SELECT id FROM del_case)`},
		{&c.CaseLinks, `DELETE FROM case_link WHERE from_court_record_id IN (SELECT id FROM del_cr)
			   OR to_court_record_id IN (SELECT id FROM del_cr)`},
		// sync_run.court_record_id is nullable NO ACTION — null it (keep the audit row)
		// rather than delete the run, which may reference records outside del_cr.
		{&c.SyncRunsCleared, `UPDATE sync_run SET court_record_id = NULL
			WHERE court_record_id IN (SELECT id FROM del_cr)`},
		{&c.CourtRecords, `DELETE FROM court_record WHERE id IN (SELECT id FROM del_cr)`},
		// draft.case_id is a nullable NO ACTION FK: a SURVIVING user-authored draft
		// (source=processo, no intimation link) that points at a to-be-emptied case would
		// block the court_case delete. Null those links first — keep the user's draft, drop
		// the dead case pointer. Ingestion-authored drafts (intimation_id set) are already gone.
		{&c.CaseDraftLinksCleared, `UPDATE draft SET case_id = NULL
			WHERE case_id IN (SELECT id FROM del_case)`},
		// court_case.merged_into_id is a self NO ACTION FK: a SURVIVING case that was merged
		// into a to-be-deleted one would block the delete. Null those pointers first (the
		// merge lineage of a pruned case is not worth keeping).
		{nil, `UPDATE court_case SET merged_into_id = NULL
			WHERE merged_into_id IN (SELECT id FROM del_case)`},
		{&c.CourtCases, `DELETE FROM court_case WHERE id IN (SELECT id FROM del_case)`},
	}

	for _, s := range steps {
		tag, err := tx.Exec(ctx, s.sql)
		if err != nil {
			return counts{}, apperr.NewInfra("cleanup: exec prune step", err)
		}
		if s.field != nil {
			*s.field += tag.RowsAffected()
		}
	}

	return c, nil
}

// buildScopeSets materializes the three id sets the prune deletes against, as ON
// COMMIT DROP temp tables so a live run and a dry run leave no residue.
//
// Every scope query filters `tenant_id = current_setting('app.tenant_id')::uuid`
// EXPLICITLY (barrier 1), independent of RLS. RLS alone (barrier 2) is NOT enough here:
// the operational role that runs this command may be a superuser / BYPASSRLS role (and
// the tables are not FORCE ROW LEVEL SECURITY), so without the explicit filter the temp
// sets — and therefore every destructive DELETE fed from them — would span ALL tenants.
// current_setting is the value uow.Do SET LOCAL for this tx; the strict (non-missing_ok)
// form makes a missing setting a hard error rather than a silent global scope.
func buildScopeSets(ctx context.Context, tx database.Tx) error {
	stmts := []string{
		// Predicate A: noise intimations (this tenant only).
		`CREATE TEMP TABLE del_intim ON COMMIT DROP AS
			SELECT id FROM intimation
			WHERE tenant_id = current_setting('app.tenant_id')::uuid
			  AND (type = 'COMUNICACAO'
			   OR published_at < (now() - ` + intervalNoise + `)::date)`,

		// Predicate B, AFTER A: a court_record is dormant when its newest LIVING signal
		// — the later of its most recent andamento (docket_entry.occurred_at) and its most
		// recent SURVIVING intimation (published_at, excluding del_intim) — is older than
		// 12 months, OR it has no living signal at all. COALESCE to a far-past epoch makes a
		// record with neither signal fall into the dormant set.
		`CREATE TEMP TABLE del_cr ON COMMIT DROP AS
			SELECT cr.id
			FROM court_record cr
			WHERE cr.tenant_id = current_setting('app.tenant_id')::uuid
			  AND GREATEST(
					COALESCE((SELECT max(de.occurred_at)
						FROM docket_entry de WHERE de.court_record_id = cr.id), 'epoch'::timestamptz),
					COALESCE((SELECT max(i.published_at)
						FROM intimation i
						WHERE i.court_record_id = cr.id
						  AND i.id NOT IN (SELECT id FROM del_intim)), 'epoch'::date)::timestamptz
				) < (now() - ` + intervalDormant + `)`,

		// A case becomes an orphan of THIS cleanup when it had at least one dormant record
		// (so the cleanup is what empties it), has no record left outside del_cr, AND is not
		// still referenced by any SURVIVING intimation. The last guard matters because
		// intimation.case_id (NOT NULL) can diverge from the case of its court_record — a
		// grau-merge can leave a LIVE intimation (on an active record of another case)
		// pointing at this case_id; deleting the case would then violate
		// notification_case_id_fkey. Both intimation.case_id and court_record.case_id are
		// NO ACTION FKs, so a case is only safe to drop when nothing living points at it.
		`CREATE TEMP TABLE del_case ON COMMIT DROP AS
			SELECT cc.id
			FROM court_case cc
			WHERE cc.tenant_id = current_setting('app.tenant_id')::uuid
			  AND EXISTS (
					SELECT 1 FROM court_record cr
					WHERE cr.case_id = cc.id AND cr.id IN (SELECT id FROM del_cr))
			  AND NOT EXISTS (
					SELECT 1 FROM court_record cr
					WHERE cr.case_id = cc.id AND cr.id NOT IN (SELECT id FROM del_cr))
			  AND NOT EXISTS (
					SELECT 1 FROM intimation i
					WHERE i.case_id = cc.id AND i.id NOT IN (SELECT id FROM del_intim))`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return apperr.NewInfra("cleanup: build scope set", err)
		}
	}
	return nil
}
