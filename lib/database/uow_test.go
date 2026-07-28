package database

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/jusassessoria/platform/lib/apperr"
)

// newMockPool builds a pgxmock pool that satisfies beginner. Every Do test drives
// its expectations against it — no real database, so the unit stays sub-ms.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	t.Cleanup(mock.Close)

	return mock
}

// Happy path: Begin → RLS set_config bound to the tenant → fn runs → Commit.
func TestUnitOfWork_Do_CommitsWithTenantScope(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.
		ExpectExec("set_config").
		WithArgs("tenant-42").
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectCommit()

	ran := false
	err := NewUnitOfWork(mock).Do(context.Background(), "tenant-42", func(tx Tx) error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("Do() error = %v, want nil", err)
	}
	if !ran {
		t.Fatal("fn was not run")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// fn error: the transaction rolls back, never commits, and the original error is
// propagated unwrapped so its domain Kind survives to the caller.
func TestUnitOfWork_Do_RollsBackOnError(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.
		ExpectExec("set_config").
		WithArgs("tenant-42").
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectRollback()

	sentinel := apperr.NewInvalid("bad input")
	err := NewUnitOfWork(mock).Do(context.Background(), "tenant-42", func(tx Tx) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Do() error = %v, want %v", err, sentinel)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Empty tenantID: set_config is NOT issued (no ExpectExec) — an accidental call
// would surface as an unexpected-Exec failure — and the work still commits.
func TestUnitOfWork_Do_SkipsRLSWhenNoTenant(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectCommit()

	err := NewUnitOfWork(mock).Do(context.Background(), "", func(tx Tx) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Do() error = %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Panic in fn: the deferred Rollback still fires, then the panic propagates out
// of Do (panic-safe rollback — the connection is not leaked).
func TestUnitOfWork_Do_RollsBackAndRepanicsOnPanic(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectRollback()

	uow := NewUnitOfWork(mock)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Do() did not re-panic")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	}()

	_ = uow.Do(context.Background(), "", func(tx Tx) error {
		panic("boom")
	})
}
