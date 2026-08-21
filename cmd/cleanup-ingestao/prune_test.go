package main

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jusassessoria/platform/lib/database"
)

// recordingTx captures every SQL statement Exec receives and returns a fixed
// affected-row count, so a test can assert the prune's statement SET, ORDER, and
// tenant/idempotency contracts without a real database.
type recordingTx struct {
	stmts    []string
	affected int64
	execErr  error
}

func (r *recordingTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	r.stmts = append(r.stmts, sql)
	if r.execErr != nil {
		return pgconn.CommandTag{}, r.execErr
	}
	return pgconn.NewCommandTag("DELETE " + strconv.FormatInt(r.affected, 10)), nil
}

func (r *recordingTx) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (r *recordingTx) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }

// stubUoW runs fn against the same recordingTx and remembers the tenant scope and
// whether fn returned an error (a dry run returns errRollback → the real UoW would
// roll back). It mirrors database.UnitOfWork.Do's contract closely enough for the
// dry-run/live-run assertions.
type stubUoW struct {
	tx        *recordingTx
	scopes    []string
	fnErr     error
	committed bool
}

func (u *stubUoW) Do(ctx context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.scopes = append(u.scopes, tenantID)
	err := fn(u.tx)
	u.fnErr = err
	if err != nil {
		return err // the real UoW rolls back and propagates
	}
	u.committed = true
	return nil
}

func (u *stubUoW) DoSystem(context.Context, func(tx database.Tx) error) error { return nil }

// firstIndexMatching returns the index of the first statement containing sub, or -1.
func firstIndexMatching(stmts []string, sub string) int {
	for i, s := range stmts {
		if strings.Contains(s, sub) {
			return i
		}
	}
	return -1
}

// lastIndexMatching returns the index of the last statement matching re, or -1.
func lastIndexMatching(stmts []string, re *regexp.Regexp) int {
	idx := -1
	for i, s := range stmts {
		if re.MatchString(s) {
			idx = i
		}
	}
	return idx
}

// TestPruneInTx_ChildFirstOrder asserts the delete plan removes each child before the
// parent it FK-references. If any pair is out of order, a real run would hit a foreign
// key violation, so this is the load-bearing safety test for the plan.
func TestPruneInTx_ChildFirstOrder(t *testing.T) {
	t.Parallel()

	tx := &recordingTx{affected: 1}
	if _, err := pruneInTx(context.Background(), tx, false); err != nil {
		t.Fatalf("pruneInTx: %v", err)
	}

	// (child DELETE, parent DELETE): the child must appear before the parent.
	pairs := []struct{ child, parent string }{
		{"DELETE FROM task_item", "DELETE FROM task "},
		{"DELETE FROM task ", "DELETE FROM deadline"},
		{"DELETE FROM task_suggestion", "DELETE FROM deadline"},
		{"DELETE FROM deadline", "DELETE FROM intimation"},
		{"DELETE FROM draft_attachment", "DELETE FROM draft "},
		{"DELETE FROM petition", "DELETE FROM draft "},
		{"DELETE FROM review", "DELETE FROM draft "},
		{"DELETE FROM chunk", "DELETE FROM document"},
		{"DELETE FROM document", "DELETE FROM court_record"},
		{"DELETE FROM docket_entry", "DELETE FROM court_record"},
		{"DELETE FROM intimation", "DELETE FROM court_record"},
		{"DELETE FROM party_counsel", "DELETE FROM party "},
		{"DELETE FROM party ", "DELETE FROM court_case"},
		{"DELETE FROM case_link", "DELETE FROM court_record"},
		{"DELETE FROM court_record", "DELETE FROM court_case"},
	}
	for _, p := range pairs {
		ci := firstIndexMatching(tx.stmts, p.child)
		pi := lastIndexMatching(tx.stmts, regexp.MustCompile(regexp.QuoteMeta(p.parent)))
		if ci < 0 {
			t.Errorf("missing child delete %q", p.child)
			continue
		}
		if pi < 0 {
			t.Errorf("missing parent delete %q", p.parent)
			continue
		}
		if ci > pi {
			t.Errorf("order violation: %q (idx %d) must precede %q (idx %d)", p.child, ci, p.parent, pi)
		}
	}
}

// TestPruneInTx_ScopeSetsFilterTenant is the regression guard for the CRITICAL barrier-1
// gap: the three scope sets (del_intim/del_cr/del_case) must each filter tenant_id
// EXPLICITLY, not rely on RLS. The operational role may be BYPASSRLS / the tables are not
// FORCE RLS, so without this filter the destructive deletes would span every tenant.
func TestPruneInTx_ScopeSetsFilterTenant(t *testing.T) {
	t.Parallel()

	tx := &recordingTx{affected: 1}
	if _, err := pruneInTx(context.Background(), tx, false); err != nil {
		t.Fatalf("pruneInTx: %v", err)
	}

	const tenantFilter = "current_setting('app.tenant_id')"
	for _, scope := range []string{"del_intim", "del_cr", "del_case"} {
		create := "CREATE TEMP TABLE " + scope
		idx := firstIndexMatching(tx.stmts, create)
		if idx < 0 {
			t.Errorf("missing scope set %q", scope)
			continue
		}
		if !strings.Contains(tx.stmts[idx], tenantFilter) {
			t.Errorf("scope set %q does not filter tenant_id (barrier 1 missing): %q", scope, tx.stmts[idx])
		}
	}
}

// TestPruneInTx_NeverTouchesProtectedTables guards the invariant that the cleanup never
// writes the idempotency/audit tables the spec pins as off-limits.
func TestPruneInTx_NeverTouchesProtectedTables(t *testing.T) {
	t.Parallel()

	tx := &recordingTx{affected: 1}
	if _, err := pruneInTx(context.Background(), tx, false); err != nil {
		t.Fatalf("pruneInTx: %v", err)
	}

	forbidden := []string{"processed_event", "outbox", "capture_run"}
	for _, table := range forbidden {
		for _, s := range tx.stmts {
			if strings.Contains(s, table) {
				t.Errorf("statement touches protected table %q: %s", table, s)
			}
		}
	}
}

// TestPruneInTx_SyncRunClearedNotDeleted asserts sync_run is de-referenced (SET NULL),
// never deleted — its audit row must survive.
func TestPruneInTx_SyncRunClearedNotDeleted(t *testing.T) {
	t.Parallel()

	tx := &recordingTx{affected: 1}
	if _, err := pruneInTx(context.Background(), tx, false); err != nil {
		t.Fatalf("pruneInTx: %v", err)
	}
	for _, s := range tx.stmts {
		if strings.Contains(s, "DELETE FROM sync_run") {
			t.Errorf("sync_run must be SET NULL, not deleted: %s", s)
		}
	}
	if firstIndexMatching(tx.stmts, "UPDATE sync_run SET court_record_id = NULL") < 0 {
		t.Error("expected a sync_run SET NULL update")
	}
}

// TestPruneInTx_ExecErrorPropagates asserts an Exec failure surfaces as an infra error
// (typed via lib/apperr), not swallowed.
func TestPruneInTx_ExecErrorPropagates(t *testing.T) {
	t.Parallel()

	tx := &recordingTx{affected: 1, execErr: errors.New("boom")}
	if _, err := pruneInTx(context.Background(), tx, false); err == nil {
		t.Fatal("expected an error when Exec fails")
	}
}

// TestCleanTenant_DryRunRollsBack asserts the dry run scopes the tenant, runs the prune,
// but returns errRollback from fn so the unit of work rolls back — nothing commits — and
// the counts still come back to the caller.
func TestCleanTenant_DryRunRollsBack(t *testing.T) {
	t.Parallel()

	tx := &recordingTx{affected: 2}
	uow := &stubUoW{tx: tx}

	c, err := cleanTenant(context.Background(), uow, "tenant-1", true)
	if err != nil {
		t.Fatalf("cleanTenant dry-run error = %v", err)
	}
	if uow.committed {
		t.Error("dry run must NOT commit")
	}
	if !errors.Is(uow.fnErr, errRollback) {
		t.Errorf("fn error = %v, want errRollback (the rollback signal)", uow.fnErr)
	}
	if len(uow.scopes) != 1 || uow.scopes[0] != "tenant-1" {
		t.Errorf("tenant scopes = %v, want [tenant-1]", uow.scopes)
	}
	// Counts are still reported (each DELETE step reported 2 affected rows).
	if c.CourtRecords != 2 || c.Intimations == 0 {
		t.Errorf("dry-run still reports counts: court_records=%d intimations=%d", c.CourtRecords, c.Intimations)
	}
}

// TestCleanTenant_LiveRunCommits asserts a live run lets fn return nil so the unit of
// work commits.
func TestCleanTenant_LiveRunCommits(t *testing.T) {
	t.Parallel()

	tx := &recordingTx{affected: 1}
	uow := &stubUoW{tx: tx}

	if _, err := cleanTenant(context.Background(), uow, "tenant-1", false); err != nil {
		t.Fatalf("cleanTenant live error = %v", err)
	}
	if !uow.committed {
		t.Error("live run must commit")
	}
	if uow.fnErr != nil {
		t.Errorf("fn error = %v, want nil on a live run", uow.fnErr)
	}
}
