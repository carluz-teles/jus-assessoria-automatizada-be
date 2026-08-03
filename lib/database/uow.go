package database

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Tx is the transaction handle a repository receives inside a unit of work. It
// is the minimal read/write subset of pgx.Tx — repositories bind their sqlc
// queries to it and never see the pool, the commit, or the rollback: the use
// case owns the transaction boundary (docs 4b.2), the repo only participates.
type Tx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// UnitOfWork runs fn inside a single database transaction. The entity write and
// its outbox row commit together or not at all (transactional outbox). tenantID,
// when non-empty, is bound to the transaction-local RLS setting so every query
// fn issues is filtered to that tenant (second isolation barrier, docs 4d.4).
//
// DoSystem is the cross-tenant counterpart for system processes (the re-poll
// scheduler): it sets app.system='on' instead of a tenant, so RLS policies with a
// system escape hatch (court_record, migration 0016) expose every tenant's rows.
// Use it ONLY for trusted system work that legitimately spans tenants — never on a
// request path.
type UnitOfWork interface {
	Do(ctx context.Context, tenantID string, fn func(tx Tx) error) error
	DoSystem(ctx context.Context, fn func(tx Tx) error) error
}

// beginner is the slice of the pool the unit of work actually needs. Depending
// on this instead of *pgxpool.Pool keeps Do unit-testable without a real
// database — both *pgxpool.Pool and pgxmock's mock pool satisfy it.
type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type unitOfWork struct {
	pool beginner
}

var _ UnitOfWork = (*unitOfWork)(nil)

// NewUnitOfWork returns a UnitOfWork backed by pool. Inject *pgxpool.Pool in
// production; inject a pgxmock pool in tests.
func NewUnitOfWork(pool beginner) UnitOfWork {
	return &unitOfWork{pool: pool}
}

// Do opens a transaction, applies the RLS tenant scope, runs fn, and commits.
// On any fn error the transaction is rolled back and the error is propagated
// unwrapped — domain errors from fn must reach the caller with their own Kind,
// not disguised as infra. Only failures owned by the boundary (Begin, RLS,
// Commit) become infra errors.
//
// The deferred Rollback is a no-op once Commit has run, and — because it is a
// defer — it also fires if fn panics, releasing the connection before the panic
// unwinds past Do (panic-safe rollback; the panic then propagates normally).
func (u *unitOfWork) Do(ctx context.Context, tenantID string, fn func(tx Tx) error) error {
	tx, err := u.pool.Begin(ctx)
	if err != nil {
		return WrapInfra(err)
	}
	defer tx.Rollback(ctx)

	if tenantID != "" {
		// set_config(...,is_local=true) is the SET LOCAL equivalent that accepts a
		// bind parameter; `SET LOCAL app.tenant_id = $1` does not, so it is avoided.
		const setTenant = "SELECT set_config('app.tenant_id', $1, true)"
		if _, err := tx.Exec(ctx, setTenant, tenantID); err != nil {
			return WrapInfra(err)
		}
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return WrapInfra(err)
	}

	return nil
}

// DoSystem opens a transaction, marks it system-scoped (app.system='on'), runs fn,
// and commits — the same lifecycle and error contract as Do, minus the tenant
// binding. It is the read/enqueue path for the re-poll scheduler, which spans every
// tenant's court records.
func (u *unitOfWork) DoSystem(ctx context.Context, fn func(tx Tx) error) error {
	tx, err := u.pool.Begin(ctx)
	if err != nil {
		return WrapInfra(err)
	}
	defer tx.Rollback(ctx)

	const setSystem = "SELECT set_config('app.system', 'on', true)"
	if _, err := tx.Exec(ctx, setSystem); err != nil {
		return WrapInfra(err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return WrapInfra(err)
	}

	return nil
}
