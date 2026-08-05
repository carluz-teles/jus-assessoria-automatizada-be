//go:build integration

// Notifications READ integration tests (slice 2a) — prove the in-app inbox against a
// REAL Postgres: the list + unread badge + mark-read are PER USER (two members of the
// same tenant read independently), the keyset paging is stable, a mark-read of another
// tenant's aviso is not-found, and the per-user receipts table is RLS-isolated.
//
// These drive the ReadUseCase directly (repo + unit of work) against the container —
// the HTTP envelope/status is covered by the handler unit tests.
package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/notifications"
	"github.com/jusassessoria/platform/lib/database"
)

// newReadUC wires the in-app inbox read use case against the real container.
func newReadUC(pool *pgxpool.Pool) *notifications.ReadUseCase {
	return notifications.NewReadUseCase(
		notifications.NewRepository(pool),
		database.NewUnitOfWork(pool),
	)
}

// seedUser inserts one app_user for the tenant with a unique clerk id/email (owner
// insert, RLS bypassed) and returns its id.
func seedUser(t *testing.T, pool *pgxpool.Pool, tenantID, label string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO app_user (clerk_user_id, tenant_id, email) VALUES ($1, $2, $3) RETURNING id::text`,
		label+"-clerk", tenantID, label+"@x.test",
	).Scan(&id); err != nil {
		t.Fatalf("seed user %s: %v", label, err)
	}
	return id
}

// seedNotification inserts one aviso (owner insert). recipientUserID "" → a
// tenant-level aviso (recipient_user_id NULL, visible to every user of the tenant).
func seedNotification(t *testing.T, pool *pgxpool.Pool, tenantID, recipientUserID, typ string) string {
	t.Helper()
	var recipient any
	if recipientUserID != "" {
		recipient = recipientUserID
	}
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO notification (tenant_id, recipient_user_id, type, title, body, status)
		 VALUES ($1, $2, $3, 'Título', 'Corpo', 'CREATED') RETURNING id::text`,
		tenantID, recipient, typ,
	).Scan(&id); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	return id
}

// readUnread is the unread badge count for a (tenant, user) via the use case.
func readUnread(t *testing.T, uc *notifications.ReadUseCase, tenantID, userID string) int {
	t.Helper()
	n, err := uc.UnreadCount(context.Background(), tenantID, userID)
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	return n
}

// readListQ builds a first-page list query (max sentinel cursor) for a (tenant, user).
func readListQ(tenantID, userID string, unreadOnly bool) notifications.ListNotificationsQuery {
	return notifications.ListNotificationsQuery{
		TenantID:    tenantID,
		UserID:      userID,
		LastCreated: "9999-12-31T23:59:59.999999999Z",
		LastID:      "ffffffff-ffff-ffff-ffff-ffffffffffff",
		UnreadOnly:  unreadOnly,
		Limit:       20,
	}
}

// AC1/AC2/AC3/AC4/AC10: read state is PER USER. Two tenant-level avisos are visible to
// both members; when user A reads one, only A's unread count and `read` flag change —
// user B still sees both unread. mark-all zeroes A without touching B.
func TestNotifications_Read_PerUserReadState(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-read-peruser", 0)
	userA := seedUser(t, pool, tenantID, "read-a")
	userB := seedUser(t, pool, tenantID, "read-b")

	n1 := seedNotification(t, pool, tenantID, "", notifications.TypeNewAndamento)
	seedNotification(t, pool, tenantID, "", notifications.TypeImportFinished)

	uc := newReadUC(pool)

	// Both members start with 2 unread.
	if got := readUnread(t, uc, tenantID, userA); got != 2 {
		t.Fatalf("userA unread = %d, want 2", got)
	}
	if got := readUnread(t, uc, tenantID, userB); got != 2 {
		t.Fatalf("userB unread = %d, want 2", got)
	}

	// User A reads n1 → A drops to 1; B is untouched (the receipt is per-user).
	if err := uc.MarkRead(ctx, tenantID, userA, n1); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if got := readUnread(t, uc, tenantID, userA); got != 1 {
		t.Fatalf("userA unread after read = %d, want 1", got)
	}
	if got := readUnread(t, uc, tenantID, userB); got != 2 {
		t.Fatalf("userB unread = %d, want 2 (a per-user receipt must not leak)", got)
	}

	// A's full list marks n1 read, the other unread.
	items, _, err := uc.List(ctx, readListQ(tenantID, userA, false))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("userA list = %d rows, want 2", len(items))
	}
	readByID := map[string]bool{}
	for _, it := range items {
		readByID[it.ID] = it.Read
	}
	if !readByID[n1] {
		t.Errorf("n1.read = false for userA, want true")
	}

	// ?unread=true returns only the one A has not read (not n1).
	unreadItems, _, err := uc.List(ctx, readListQ(tenantID, userA, true))
	if err != nil {
		t.Fatalf("List unread: %v", err)
	}
	if len(unreadItems) != 1 || unreadItems[0].ID == n1 {
		t.Fatalf("userA unread list = %+v, want the single non-n1 aviso", unreadItems)
	}

	// mark-all zeroes A; a replay is a clean no-op; B is still fully unread.
	if err := uc.MarkAllRead(ctx, tenantID, userA); err != nil {
		t.Fatalf("MarkAllRead: %v", err)
	}
	if got := readUnread(t, uc, tenantID, userA); got != 0 {
		t.Fatalf("userA unread after mark-all = %d, want 0", got)
	}
	if err := uc.MarkAllRead(ctx, tenantID, userA); err != nil {
		t.Fatalf("MarkAllRead replay: %v", err)
	}
	if got := readUnread(t, uc, tenantID, userB); got != 2 {
		t.Fatalf("userB unread = %d, want 2 (unaffected by A's mark-all)", got)
	}
}

// AC1: the inbox is keyset-paginated — with 3 avisos and limit 2, page 1 returns 2 and
// signals more; page 2 (resumed from page 1's last created_at/id) returns the last 1
// with no overlap.
func TestNotifications_Read_KeysetPaging(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-read-page", 0)
	userA := seedUser(t, pool, tenantID, "read-page-a")
	for range 3 {
		seedNotification(t, pool, tenantID, "", notifications.TypeNewAndamento)
	}

	uc := newReadUC(pool)
	q := readListQ(tenantID, userA, false)
	q.Limit = 2

	page1, hasMore, err := uc.List(ctx, q)
	if err != nil {
		t.Fatalf("List page1: %v", err)
	}
	if len(page1) != 2 || !hasMore {
		t.Fatalf("page1 = %d rows hasMore=%v, want 2/true", len(page1), hasMore)
	}

	last := page1[len(page1)-1]
	q.LastCreated = last.CreatedAt.Format(time.RFC3339Nano)
	q.LastID = last.ID

	page2, hasMore, err := uc.List(ctx, q)
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2) != 1 || hasMore {
		t.Fatalf("page2 = %d rows hasMore=%v, want 1/false", len(page2), hasMore)
	}
	if page2[0].ID == page1[0].ID || page2[0].ID == page1[1].ID {
		t.Fatalf("page2 aviso %q overlaps page1", page2[0].ID)
	}
}

// AC3: a mark-read of an aviso that is not visible in the caller's tenant (another
// tenant's, or a garbage id) is ErrNotificationNotFound (→ 404).
func TestNotifications_Read_MarkRead_CrossTenantNotFound(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-read-xt-a", 0)
	seedTenant(t, pool, tenantB, "org-read-xt-b", 0)
	userB := seedUser(t, pool, tenantB, "read-xt-b")
	avisoA := seedNotification(t, pool, tenantA, "", notifications.TypeNewAndamento)

	uc := newReadUC(pool)

	if err := uc.MarkRead(ctx, tenantB, userB, avisoA); !errors.Is(err, notifications.ErrNotificationNotFound) {
		t.Fatalf("cross-tenant mark-read err = %v, want ErrNotificationNotFound", err)
	}
	if err := uc.MarkRead(ctx, tenantB, userB, "not-a-uuid"); !errors.Is(err, notifications.ErrNotificationNotFound) {
		t.Fatalf("garbage id mark-read err = %v, want ErrNotificationNotFound", err)
	}
}

// AC5/AC10: the per-user receipts table is RLS-isolated — with app.tenant_id set the
// app_rls role sees only that tenant's receipts; unset, it sees none.
func TestNotifications_Read_RLS_ReceiptsIsolated(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	// The non-owner role the policies bind to (idempotent — shared with the other RLS
	// suites; created here too so this test stands alone regardless of order).
	mustExec(t, pool, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_rls') THEN
			CREATE ROLE app_rls;
		END IF;
	END $$`)
	mustExec(t, pool, `GRANT USAGE ON SCHEMA public TO app_rls`)
	mustExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_rls`)

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-read-rls-a", 0)
	seedTenant(t, pool, tenantB, "org-read-rls-b", 0)
	userA := seedUser(t, pool, tenantA, "read-rls-a")
	avisoA := seedNotification(t, pool, tenantA, "", notifications.TypeNewAndamento)

	if err := newReadUC(pool).MarkRead(ctx, tenantA, userA, avisoA); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}

	tests := []struct {
		name     string
		tenantID string // empty = do not set app.tenant_id
		want     int
	}{
		{name: "tenant A sees its own receipt", tenantID: tenantA, want: 1},
		{name: "tenant B sees none", tenantID: tenantB, want: 0},
		{name: "no tenant set sees nothing", tenantID: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countNotificationReadAsRLSRole(t, tt.tenantID); got != tt.want {
				t.Errorf("notification_read count under RLS = %d, want %d", got, tt.want)
			}
		})
	}
}

// countNotificationReadAsRLSRole counts notification_read rows visible to the non-owner
// app_rls role with app.tenant_id set (or unset when empty), on a DEDICATED connection
// — same pristine-connection rationale as countNotificationsAsRLSRole (see rls_test.go).
func countNotificationReadAsRLSRole(t *testing.T, tenantID string) int {
	t.Helper()
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) // read-only probe; never commit

	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_rls"); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if tenantID != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
			t.Fatalf("set_config: %v", err)
		}
	}

	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM notification_read").Scan(&count); err != nil {
		t.Fatalf("count notification_read: %v", err)
	}
	return count
}
