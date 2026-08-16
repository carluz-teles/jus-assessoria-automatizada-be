package notifications

import (
	"context"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/lib/database"
)

// mockReadRepo is a hand-written readRepo double: each method delegates to a func
// field, so every test injects exactly the behavior it needs. It counts the receipt
// writes so a test asserts an invisible aviso never reaches the insert. Unset fields
// fail loudly (nil call) if a test reaches a path it did not expect.
type mockReadRepo struct {
	list        func(ctx context.Context, q ListNotificationsQuery) ([]NotificationView, error)
	countUnread func(ctx context.Context, tenantID, userID string) (int, error)
	visible     func(ctx context.Context, tx database.Tx, notificationID, tenantID, userID string) (bool, error)
	markRead    func(ctx context.Context, tx database.Tx, notificationID, tenantID, userID string) error
	markAll     func(ctx context.Context, tx database.Tx, tenantID, userID string) error

	markReadCalls int
	markAllCalls  int
}

func (m *mockReadRepo) ListNotifications(ctx context.Context, q ListNotificationsQuery) ([]NotificationView, error) {
	return m.list(ctx, q)
}

func (m *mockReadRepo) CountUnread(ctx context.Context, tenantID, userID string) (int, error) {
	return m.countUnread(ctx, tenantID, userID)
}

func (m *mockReadRepo) NotificationVisibleTo(ctx context.Context, tx database.Tx, notificationID, tenantID, userID string) (bool, error) {
	return m.visible(ctx, tx, notificationID, tenantID, userID)
}

func (m *mockReadRepo) MarkRead(ctx context.Context, tx database.Tx, notificationID, tenantID, userID string) error {
	m.markReadCalls++
	return m.markRead(ctx, tx, notificationID, tenantID, userID)
}

func (m *mockReadRepo) MarkAllRead(ctx context.Context, tx database.Tx, tenantID, userID string) error {
	m.markAllCalls++
	return m.markAll(ctx, tx, tenantID, userID)
}

// AC9(a): List over-fetches one row so it can report hasMore without a COUNT — a
// full page (>limit rows) trims to limit and signals a next page; a short/exact page
// is the last one. The envelope's type options are always present (the closed set).
func TestReadUseCase_List(t *testing.T) {
	tests := []struct {
		name        string
		limit       int
		returned    int
		wantItems   int
		wantHasMore bool
	}{
		{name: "full page signals a next page", limit: 2, returned: 3, wantItems: 2, wantHasMore: true},
		{name: "partial page is the last page", limit: 2, returned: 1, wantItems: 1, wantHasMore: false},
		{name: "exactly one page is the last page", limit: 2, returned: 2, wantItems: 2, wantHasMore: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotLimit int
			repo := &mockReadRepo{
				list: func(_ context.Context, q ListNotificationsQuery) ([]NotificationView, error) {
					gotLimit = q.Limit
					return make([]NotificationView, tt.returned), nil
				},
			}
			uc := NewReadUseCase(repo, &fakeUOW{})

			res, err := uc.List(context.Background(), ListNotificationsQuery{Limit: tt.limit})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if gotLimit != tt.limit+1 {
				t.Errorf("repo limit = %d, want %d (over-fetch by one)", gotLimit, tt.limit+1)
			}
			if len(res.Items) != tt.wantItems {
				t.Errorf("items = %d, want %d", len(res.Items), tt.wantItems)
			}
			if res.HasMore != tt.wantHasMore {
				t.Errorf("hasMore = %v, want %v", res.HasMore, tt.wantHasMore)
			}
			wantTypes := []string{TypeImportFinished, TypeNewAndamento, TypeDeadlineDueSoonAviso, TypeDeadlineMissedAviso, TypeTrialEndingSoonAviso}
			types := res.Filters["type"]
			if len(types) != len(wantTypes) {
				t.Fatalf("type options = %+v, want %v", types, wantTypes)
			}
			for i, want := range wantTypes {
				if types[i].Label != want || types[i].Value != want {
					t.Errorf("types[%d] = %+v, want label==value %q", i, types[i], want)
				}
			}
		})
	}
}

// AC9(b): mark-read of an aviso not visible in the tenant (another tenant's, or one
// addressed to a different user) is ErrNotificationNotFound — and the receipt insert
// is never reached.
func TestReadUseCase_MarkRead_NotVisibleIsNotFound(t *testing.T) {
	repo := &mockReadRepo{
		visible: func(context.Context, database.Tx, string, string, string) (bool, error) {
			return false, nil
		},
	}
	uc := NewReadUseCase(repo, &fakeUOW{})

	err := uc.MarkRead(context.Background(), tenantID, recipientID, notifID)
	if !errors.Is(err, ErrNotificationNotFound) {
		t.Fatalf("err = %v, want ErrNotificationNotFound", err)
	}
	if repo.markReadCalls != 0 {
		t.Errorf("receipt inserted %d times for an invisible aviso, want 0", repo.markReadCalls)
	}
}

// A visible aviso records the receipt (the happy path beneath the 404 gate).
func TestReadUseCase_MarkRead_VisibleRecordsReceipt(t *testing.T) {
	repo := &mockReadRepo{
		visible:  func(context.Context, database.Tx, string, string, string) (bool, error) { return true, nil },
		markRead: func(context.Context, database.Tx, string, string, string) error { return nil },
	}
	uc := NewReadUseCase(repo, &fakeUOW{})

	if err := uc.MarkRead(context.Background(), tenantID, recipientID, notifID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if repo.markReadCalls != 1 {
		t.Errorf("receipt inserts = %d, want 1", repo.markReadCalls)
	}
}

// AC9(c): mark-all is idempotent — a second call runs cleanly (the repo's ON CONFLICT
// makes it a no-op), so the use case simply delegates again without error.
func TestReadUseCase_MarkAllRead_Idempotent(t *testing.T) {
	repo := &mockReadRepo{
		markAll: func(context.Context, database.Tx, string, string) error { return nil },
	}
	uc := NewReadUseCase(repo, &fakeUOW{})

	for range 2 {
		if err := uc.MarkAllRead(context.Background(), tenantID, recipientID); err != nil {
			t.Fatalf("MarkAllRead: %v", err)
		}
	}
	if repo.markAllCalls != 2 {
		t.Errorf("MarkAllRead delegated %d times, want 2", repo.markAllCalls)
	}
}

// AC9(d): the unread count is per-tenant AND per-user — the use case forwards both
// scopes to the repo verbatim (never the body). The real per-user divergence (one
// user reads, the other still sees it unread) is proven end-to-end in the integration
// suite against a real notification_read table.
func TestReadUseCase_UnreadCount_ForwardsUserScope(t *testing.T) {
	repo := &mockReadRepo{
		countUnread: func(_ context.Context, gotTenant, gotUser string) (int, error) {
			if gotTenant != tenantID || gotUser != recipientID {
				t.Errorf("scope = (%q,%q), want (%q,%q)", gotTenant, gotUser, tenantID, recipientID)
			}
			return 7, nil
		},
	}
	uc := NewReadUseCase(repo, &fakeUOW{})

	n, err := uc.UnreadCount(context.Background(), tenantID, recipientID)
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if n != 7 {
		t.Errorf("count = %d, want 7", n)
	}
}
