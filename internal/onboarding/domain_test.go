package onboarding

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/database"
)

// mockRepo is a hand-written Repository double: each method delegates to a
// func field, so every test injects exactly the behavior it needs.
type mockRepo struct {
	getProgress func(ctx context.Context, tenantID, appUserID string) (Progress, error)
	dismiss     func(ctx context.Context, tx database.Tx, tenantID, appUserID string) error
}

func (m *mockRepo) GetProgress(ctx context.Context, tenantID, appUserID string) (Progress, error) {
	return m.getProgress(ctx, tenantID, appUserID)
}

func (m *mockRepo) Dismiss(ctx context.Context, tx database.Tx, tenantID, appUserID string) error {
	return m.dismiss(ctx, tx, tenantID, appUserID)
}

// fakeUOW runs fn immediately against a nil tx, recording the tenant scope Do
// was called with — enough to assert the write ran under the caller's own
// tenant, without a real database.
type fakeUOW struct {
	scope  string
	called bool
}

func (u *fakeUOW) Do(_ context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.called = true
	u.scope = tenantID
	return fn(nil)
}

func (u *fakeUOW) DoSystem(_ context.Context, fn func(tx database.Tx) error) error {
	return fn(nil)
}

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
	userA   = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
)

func TestUseCase_GetProgress(t *testing.T) {
	dismissed := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		want Progress
	}{
		{
			name: "nothing activated yet: every step false, never dismissed",
			want: Progress{},
		},
		{
			name: "every step activated and the widget dismissed",
			want: Progress{
				Steps: Steps{
					SourcesConnected: true,
					MembersInvited:   true,
					FirstTriagem:     true,
					FirstAnalise:     true,
					FirstPeca:        true,
				},
				DismissedAt: &dismissed,
			},
		},
		{
			name: "mixed progress: some steps true, some false, not dismissed",
			want: Progress{Steps: Steps{SourcesConnected: true, FirstTriagem: true}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &mockRepo{
				getProgress: func(_ context.Context, gotTenant, gotUser string) (Progress, error) {
					if gotTenant != tenantA || gotUser != userA {
						t.Fatalf("scope = (%q, %q), want (%q, %q)", gotTenant, gotUser, tenantA, userA)
					}
					return tt.want, nil
				},
			}

			got, err := NewUseCase(repo, &fakeUOW{}).GetProgress(context.Background(), tenantA, userA)
			if err != nil {
				t.Fatalf("GetProgress() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("GetProgress() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestUseCase_GetProgress_TenantIsolation proves the use case is a pure
// pass-through: it never substitutes or mixes tenants. Real isolation is
// enforced by the repository's SQL (WHERE tenant_id = $1) plus RLS — this
// unit test only proves the use case forwards the CALLER's own tenant id,
// exactly as given, request after request.
func TestUseCase_GetProgress_TenantIsolation(t *testing.T) {
	var gotTenant string
	repo := &mockRepo{
		getProgress: func(_ context.Context, tenantID, _ string) (Progress, error) {
			gotTenant = tenantID
			return Progress{}, nil
		},
	}
	uc := NewUseCase(repo, &fakeUOW{})

	if _, err := uc.GetProgress(context.Background(), tenantA, userA); err != nil {
		t.Fatalf("GetProgress() error = %v", err)
	}
	if gotTenant != tenantA {
		t.Fatalf("tenant scope = %q, want %q", gotTenant, tenantA)
	}

	if _, err := uc.GetProgress(context.Background(), tenantB, userA); err != nil {
		t.Fatalf("GetProgress() error = %v", err)
	}
	if gotTenant != tenantB {
		t.Fatalf("tenant scope = %q, want %q", gotTenant, tenantB)
	}
}

func TestUseCase_GetProgress_RepoError(t *testing.T) {
	boom := errors.New("db down")
	repo := &mockRepo{
		getProgress: func(context.Context, string, string) (Progress, error) { return Progress{}, boom },
	}

	_, err := NewUseCase(repo, &fakeUOW{}).GetProgress(context.Background(), tenantA, userA)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want %v", err, boom)
	}
}

func TestUseCase_Dismiss(t *testing.T) {
	calls := 0
	repo := &mockRepo{
		dismiss: func(_ context.Context, _ database.Tx, gotTenant, gotUser string) error {
			calls++
			if gotTenant != tenantA || gotUser != userA {
				t.Fatalf("scope = (%q, %q), want (%q, %q)", gotTenant, gotUser, tenantA, userA)
			}
			return nil
		},
	}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, uow)

	if err := uc.Dismiss(context.Background(), tenantA, userA); err != nil {
		t.Fatalf("Dismiss() error = %v", err)
	}
	if !uow.called || uow.scope != tenantA {
		t.Fatalf("uow = %+v, want called under tenant %q", uow, tenantA)
	}

	// Idempotent: dismissing a SECOND time must not error — the underlying
	// query is an upsert (ON CONFLICT DO UPDATE) and the use case adds no
	// extra guard that could reject a repeat call.
	if err := uc.Dismiss(context.Background(), tenantA, userA); err != nil {
		t.Fatalf("second Dismiss() error = %v, want nil (idempotent)", err)
	}
	if calls != 2 {
		t.Fatalf("repo.Dismiss called %d times, want 2", calls)
	}
}

func TestUseCase_Dismiss_RepoError(t *testing.T) {
	boom := errors.New("db down")
	repo := &mockRepo{
		dismiss: func(context.Context, database.Tx, string, string) error { return boom },
	}

	err := NewUseCase(repo, &fakeUOW{}).Dismiss(context.Background(), tenantA, userA)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want %v", err, boom)
	}
}
