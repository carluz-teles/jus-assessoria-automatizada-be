package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// baseWithID builds an events.Base with the given event id and the fixture tenant as
// the aggregate, mirroring how the future producer would stamp the event.
func baseWithID(eventID string) events.Base {
	return events.Base{EventID: eventID, Aggregate: tenantID}
}

// --- test doubles -----------------------------------------------------------

// mockRepo is a hand-written Repository double: each method delegates to a func
// field, so every test injects exactly the behavior it needs. It also records the
// params it saw so tests assert what was written. Unset fields fail loudly (nil
// call) if a test reaches a path it did not expect.
type mockRepo struct {
	insertNotif        func(ctx context.Context, tx database.Tx, p InsertNotificationParams) (*Notification, error)
	insertDelivery     func(ctx context.Context, tx database.Tx, p InsertDeliveryParams) (*NotificationDelivery, error)
	updateStatus       func(ctx context.Context, tx database.Tx, p UpdateDeliveryStatusParams) (*NotificationDelivery, error)
	findEmail          func(ctx context.Context, tx database.Tx, tenantID, appUserID string) (string, error)
	findByProviderID   func(ctx context.Context, providerMessageID string) (*NotificationDelivery, error)
	hasRunningBackfill func(ctx context.Context, tx database.Tx, tenantID string) (bool, error)

	insertedNotif    []InsertNotificationParams
	insertedDelivery []InsertDeliveryParams
	updatedStatus    []UpdateDeliveryStatusParams
}

func (m *mockRepo) InsertNotification(ctx context.Context, tx database.Tx, p InsertNotificationParams) (*Notification, error) {
	m.insertedNotif = append(m.insertedNotif, p)
	return m.insertNotif(ctx, tx, p)
}

func (m *mockRepo) InsertDelivery(ctx context.Context, tx database.Tx, p InsertDeliveryParams) (*NotificationDelivery, error) {
	m.insertedDelivery = append(m.insertedDelivery, p)
	return m.insertDelivery(ctx, tx, p)
}

func (m *mockRepo) UpdateDeliveryStatus(ctx context.Context, tx database.Tx, p UpdateDeliveryStatusParams) (*NotificationDelivery, error) {
	m.updatedStatus = append(m.updatedStatus, p)
	return m.updateStatus(ctx, tx, p)
}

func (m *mockRepo) FindRecipientEmail(ctx context.Context, tx database.Tx, tenantID, appUserID string) (string, error) {
	return m.findEmail(ctx, tx, tenantID, appUserID)
}

func (m *mockRepo) FindDeliveryByProviderMessageID(ctx context.Context, providerMessageID string) (*NotificationDelivery, error) {
	return m.findByProviderID(ctx, providerMessageID)
}

func (m *mockRepo) HasRunningBackfillForTenant(ctx context.Context, tx database.Tx, tenantID string) (bool, error) {
	return m.hasRunningBackfill(ctx, tx, tenantID)
}

// The in-app inbox read side (slice 2a) is exercised by ReadUseCase's own tests
// (read_test.go, which drives the narrow readRepo). These write-path tests never
// reach them, so the mock leaves them unimplemented (a call would panic loudly).
func (m *mockRepo) ListNotifications(context.Context, ListNotificationsQuery) ([]NotificationView, error) {
	panic("mockRepo.ListNotifications: not expected on the write path")
}

func (m *mockRepo) CountUnread(context.Context, string, string) (int, error) {
	panic("mockRepo.CountUnread: not expected on the write path")
}

func (m *mockRepo) NotificationVisibleTo(context.Context, database.Tx, string, string, string) (bool, error) {
	panic("mockRepo.NotificationVisibleTo: not expected on the write path")
}

func (m *mockRepo) MarkRead(context.Context, database.Tx, string, string, string) error {
	panic("mockRepo.MarkRead: not expected on the write path")
}

func (m *mockRepo) MarkAllRead(context.Context, database.Tx, string, string) error {
	panic("mockRepo.MarkAllRead: not expected on the write path")
}

// spyChannel records the message it was asked to send and returns a preset id/error.
type spyChannel struct {
	sent []EmailMessage
	id   string
	err  error
}

func (c *spyChannel) Kind() string { return ChannelEmail }

func (c *spyChannel) Send(_ context.Context, msg EmailMessage) (string, error) {
	c.sent = append(c.sent, msg)
	return c.id, c.err
}

// fakeUOW is a no-op unit of work: it records the RLS scope each Do asked for and
// runs fn with a nil tx (the mocked repo/dedup never touch it). It counts calls so a
// test can assert the two-phase (create tx + update tx) shape.
type fakeUOW struct {
	scopes []string
	err    error
}

func (u *fakeUOW) Do(_ context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.scopes = append(u.scopes, tenantID)
	if u.err != nil {
		return u.err
	}
	return fn(nil)
}

func (u *fakeUOW) DoSystem(_ context.Context, fn func(tx database.Tx) error) error {
	u.scopes = append(u.scopes, "system")
	if u.err != nil {
		return u.err
	}
	return fn(nil)
}

// fakeDedup reports every event as first-seen by default; set seen=true to model an
// at-least-once replay. It records the ids it was asked to mark.
type fakeDedup struct {
	seen      bool
	err       error
	marked    []string
	consumers []string
}

func (d *fakeDedup) SeenOrMark(_ context.Context, _ database.Tx, consumer, eventID string) (bool, error) {
	d.consumers = append(d.consumers, consumer)
	d.marked = append(d.marked, eventID)
	return d.seen, d.err
}

// fakePublisher captures every best-effort push so a test can assert the channel and
// payload, and can be told to fail (err) to model a dropped push (the aviso is still
// persisted; the failure is logged and swallowed, not propagated).
type fakePublisher struct {
	err      error
	channels []string
	payloads [][]byte
}

func (p *fakePublisher) Publish(_ context.Context, channel string, payload []byte) error {
	p.channels = append(p.channels, channel)
	p.payloads = append(p.payloads, payload)
	return p.err
}

// --- fixtures ---------------------------------------------------------------

const (
	tenantID    = "tenant-uuid"
	recipientID = "user-uuid"
	notifID     = "notif-uuid"
	deliveryID  = "delivery-uuid"
	recipientTo = "advogado@escritorio.test"
)

// requested builds a member_joined notification.requested for the fixture tenant.
func requested(eventID string) NotificationRequested {
	return NotificationRequested{
		Base:            baseWithID(eventID),
		TenantID:        tenantID,
		RecipientUserID: recipientID,
		Type:            "member_joined",
		Payload:         map[string]any{"member_name": "Ana", "org_name": "Advocacia Silva"},
	}
}

// repoHappy returns a mockRepo whose create/resolve paths all succeed, minting the
// fixture ids; the send-phase update echoes the params back.
func repoHappy() *mockRepo {
	return &mockRepo{
		findEmail: func(context.Context, database.Tx, string, string) (string, error) {
			return recipientTo, nil
		},
		insertNotif: func(_ context.Context, _ database.Tx, p InsertNotificationParams) (*Notification, error) {
			return &Notification{ID: notifID, TenantID: p.TenantID, Type: p.Type, Status: p.Status}, nil
		},
		insertDelivery: func(_ context.Context, _ database.Tx, p InsertDeliveryParams) (*NotificationDelivery, error) {
			return &NotificationDelivery{ID: deliveryID, NotificationID: p.NotificationID, TenantID: p.TenantID, Channel: p.Channel, Status: p.Status}, nil
		},
		updateStatus: func(_ context.Context, _ database.Tx, p UpdateDeliveryStatusParams) (*NotificationDelivery, error) {
			return &NotificationDelivery{ID: p.DeliveryID, Status: p.Status, ProviderMessageID: p.ProviderMessageID, Error: p.Error}, nil
		},
	}
}

// repoInApp returns a mockRepo for the in-app use case: the create paths mint the
// fixture ids (echoing title/body/type back so tests can assert what was written) and
// no backfill is running by default (docket avisos are not suppressed).
func repoInApp() *mockRepo {
	return &mockRepo{
		insertNotif: func(_ context.Context, _ database.Tx, p InsertNotificationParams) (*Notification, error) {
			return &Notification{ID: notifID, TenantID: p.TenantID, Type: p.Type, Title: p.Title, Body: p.Body, Status: p.Status}, nil
		},
		insertDelivery: func(_ context.Context, _ database.Tx, p InsertDeliveryParams) (*NotificationDelivery, error) {
			return &NotificationDelivery{ID: deliveryID, NotificationID: p.NotificationID, TenantID: p.TenantID, Channel: p.Channel, Status: p.Status}, nil
		},
		hasRunningBackfill: func(context.Context, database.Tx, string) (bool, error) { return false, nil },
	}
}

// backfillFinished builds an acquisition.backfill_finished for the fixture tenant with
// a given terminal status and error tally (SlicesOK fills the remainder of ten slices).
func backfillFinished(eventID, status string, slicesError int) BackfillFinished {
	return BackfillFinished{
		Base:          baseWithID(eventID),
		TenantID:      tenantID,
		BackfillJobID: "job-uuid",
		IntegrationID: "integration-uuid",
		TotalSlices:   10,
		SlicesOK:      10 - slicesError,
		SlicesError:   slicesError,
		Status:        status,
	}
}

// docketEntryObserved builds an acquisition.docket_entry_observed for the fixture tenant.
func docketEntryObserved(eventID string) DocketEntryObserved {
	return DocketEntryObserved{
		Base:          baseWithID(eventID),
		TenantID:      tenantID,
		SyncRunID:     "sync-uuid",
		CourtRecordID: "record-uuid",
		DocketEntryID: "entry-uuid",
		Hash:          "hash-1",
	}
}

// deadlineDueSoon builds a deadline.due_soon for the fixture tenant with a given days_left.
func deadlineDueSoon(eventID string, daysLeft int) DeadlineDueSoon {
	return DeadlineDueSoon{
		Base:       baseWithID(eventID),
		TenantID:   tenantID,
		DeadlineID: "deadline-uuid",
		DaysLeft:   daysLeft,
	}
}

// deadlineMissed builds a deadline.missed for the fixture tenant.
func deadlineMissed(eventID string) DeadlineMissed {
	return DeadlineMissed{
		Base:       baseWithID(eventID),
		TenantID:   tenantID,
		DeadlineID: "deadline-uuid",
	}
}

var trialEndsAtFixture = time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC)

func trialEndingSoon(eventID string, daysLeft int) TrialEndingSoon {
	return TrialEndingSoon{
		Base:        baseWithID(eventID),
		TenantID:    tenantID,
		TrialEndsAt: trialEndsAtFixture,
		DaysLeft:    daysLeft,
	}
}

// --- tests ------------------------------------------------------------------

// AC2: notification.requested → notification + delivery(QUEUED) created in the
// tenant-scoped tx, then EmailChannel.Send → delivery SENT + provider_message_id.
func TestNotifyUseCase_OnNotificationRequested_SendsAndMarksSent(t *testing.T) {
	repo := repoHappy()
	channel := &spyChannel{id: "resend-123"}
	dedup := &fakeDedup{}
	uow := &fakeUOW{}
	uc := NewNotifyUseCase(repo, channel, dedup, uow)

	if err := uc.OnNotificationRequested(context.Background(), requested("evt-1")); err != nil {
		t.Fatalf("OnNotificationRequested: %v", err)
	}

	// The notification was created CREATED with the payload/type; the delivery QUEUED.
	if len(repo.insertedNotif) != 1 || repo.insertedNotif[0].Status != StatusCreated || repo.insertedNotif[0].Type != "member_joined" {
		t.Fatalf("inserted notification = %+v", repo.insertedNotif)
	}
	if len(repo.insertedDelivery) != 1 || repo.insertedDelivery[0].Status != DeliveryQueued || repo.insertedDelivery[0].Channel != ChannelEmail {
		t.Fatalf("inserted delivery = %+v", repo.insertedDelivery)
	}
	// The e-mail was sent to the resolved recipient with the event's type/payload.
	if len(channel.sent) != 1 || channel.sent[0].To != recipientTo || channel.sent[0].Type != "member_joined" {
		t.Fatalf("channel.sent = %+v", channel.sent)
	}
	// The outcome was recorded SENT with the provider id.
	if len(repo.updatedStatus) != 1 {
		t.Fatalf("update calls = %d, want 1", len(repo.updatedStatus))
	}
	upd := repo.updatedStatus[0]
	if upd.DeliveryID != deliveryID || upd.Status != DeliverySent || upd.ProviderMessageID != "resend-123" || upd.Error != "" {
		t.Fatalf("update params = %+v, want SENT + provider id", upd)
	}
	// Both phases ran under the event's tenant (create tx, then update tx).
	if len(uow.scopes) != 2 || uow.scopes[0] != tenantID || uow.scopes[1] != tenantID {
		t.Fatalf("uow scopes = %v, want two %q", uow.scopes, tenantID)
	}
	// Dedup was consulted with the event id.
	if len(dedup.marked) != 1 || dedup.marked[0] != "evt-1" {
		t.Fatalf("dedup marked = %v, want [evt-1]", dedup.marked)
	}
}

// AC3: a replay (dedup already seen) is a pure no-op — no notification, no delivery,
// no send, no second dedup effect beyond the check.
func TestNotifyUseCase_OnNotificationRequested_ReplayIsNoOp(t *testing.T) {
	repo := &mockRepo{
		insertNotif: func(context.Context, database.Tx, InsertNotificationParams) (*Notification, error) {
			t.Fatal("insert notification ran on a replay")
			return nil, nil
		},
	}
	channel := &spyChannel{id: "unused"}
	uc := NewNotifyUseCase(repo, channel, &fakeDedup{seen: true}, &fakeUOW{})

	if err := uc.OnNotificationRequested(context.Background(), requested("evt-dup")); err != nil {
		t.Fatalf("OnNotificationRequested: %v", err)
	}

	if len(repo.insertedNotif) != 0 || len(repo.insertedDelivery) != 0 {
		t.Fatalf("replay wrote: notif=%v delivery=%v", repo.insertedNotif, repo.insertedDelivery)
	}
	if len(channel.sent) != 0 {
		t.Fatalf("replay sent %d e-mails, want 0", len(channel.sent))
	}
}

// AC4: a recipient with no e-mail → delivery FAILED (recorded, not sent), and the
// listener does not fail (no error propagates, no send attempted).
func TestNotifyUseCase_OnNotificationRequested_NoRecipientEmail(t *testing.T) {
	tests := []struct {
		name  string
		ev    NotificationRequested
		email func(context.Context, database.Tx, string, string) (string, error)
	}{
		{
			name:  "recipient not found in tenant",
			ev:    requested("evt-nf"),
			email: func(context.Context, database.Tx, string, string) (string, error) { return "", ErrRecipientNotFound },
		},
		{
			name: "event carries no recipient",
			ev: func() NotificationRequested {
				ev := requested("evt-no-user")
				ev.RecipientUserID = ""
				return ev
			}(),
			email: func(context.Context, database.Tx, string, string) (string, error) {
				t.Fatal("email lookup ran for an empty recipient")
				return "", nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := repoHappy()
			repo.findEmail = tt.email
			channel := &spyChannel{}
			uc := NewNotifyUseCase(repo, channel, &fakeDedup{}, &fakeUOW{})

			if err := uc.OnNotificationRequested(context.Background(), tt.ev); err != nil {
				t.Fatalf("OnNotificationRequested: %v", err)
			}

			// A FAILED delivery was recorded with a reason; nothing was sent; no
			// status update (the delivery was born FAILED).
			if len(repo.insertedDelivery) != 1 || repo.insertedDelivery[0].Status != DeliveryFailed {
				t.Fatalf("inserted delivery = %+v, want one FAILED", repo.insertedDelivery)
			}
			if repo.insertedDelivery[0].Error == "" {
				t.Fatal("FAILED delivery recorded no reason")
			}
			if len(channel.sent) != 0 {
				t.Fatalf("sent %d e-mails for a no-address recipient, want 0", len(channel.sent))
			}
			if len(repo.updatedStatus) != 0 {
				t.Fatalf("status updated %d times, want 0", len(repo.updatedStatus))
			}
		})
	}
}

// AC2/AC4: a provider send failure does not fail the listener — it is recorded as a
// FAILED delivery with the reason, and the task acks (no error propagates).
func TestNotifyUseCase_OnNotificationRequested_SendFailureRecordsFailed(t *testing.T) {
	repo := repoHappy()
	channel := &spyChannel{err: errors.New("resend 503")}
	uc := NewNotifyUseCase(repo, channel, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnNotificationRequested(context.Background(), requested("evt-send-fail")); err != nil {
		t.Fatalf("OnNotificationRequested should not propagate a send failure: %v", err)
	}

	if len(repo.updatedStatus) != 1 {
		t.Fatalf("update calls = %d, want 1", len(repo.updatedStatus))
	}
	upd := repo.updatedStatus[0]
	if upd.Status != DeliveryFailed || upd.ProviderMessageID != "" || upd.Error == "" {
		t.Fatalf("update params = %+v, want FAILED + reason, no provider id", upd)
	}
}

// A dedup infra fault propagates (retryable), and nothing is created or sent.
func TestNotifyUseCase_OnNotificationRequested_DedupErrorPropagates(t *testing.T) {
	repo := repoHappy()
	channel := &spyChannel{}
	dedupErr := errors.New("processed_event unreachable")
	uc := NewNotifyUseCase(repo, channel, &fakeDedup{err: dedupErr}, &fakeUOW{})

	err := uc.OnNotificationRequested(context.Background(), requested("evt-infra"))
	if !errors.Is(err, dedupErr) {
		t.Fatalf("err = %v, want the dedup fault", err)
	}
	if len(repo.insertedNotif) != 0 || len(channel.sent) != 0 {
		t.Fatal("wrote or sent despite a dedup fault")
	}
}

// AC2: acquisition.backfill_finished (COMPLETED) → an import_finished aviso with the
// fixed title/COMPLETED body, plus one IN_APP delivery QUEUED, in the tenant-scoped tx.
func TestInAppUseCase_OnBackfillFinished_CreatesImportFinished(t *testing.T) {
	repo := repoInApp()
	dedup := &fakeDedup{}
	uow := &fakeUOW{}
	pub := &fakePublisher{}
	uc := NewInAppUseCase(repo, dedup, uow, pub)

	if err := uc.OnBackfillFinished(context.Background(), backfillFinished("evt-bf-1", acquisition.BackfillStatusCompleted, 0)); err != nil {
		t.Fatalf("OnBackfillFinished: %v", err)
	}

	if len(repo.insertedNotif) != 1 {
		t.Fatalf("inserted notifications = %d, want 1", len(repo.insertedNotif))
	}
	notif := repo.insertedNotif[0]
	if notif.Type != TypeImportFinished || notif.Status != StatusCreated || notif.RecipientUserID != "" {
		t.Fatalf("notification = %+v, want import_finished/CREATED/tenant-level", notif)
	}
	if notif.Title != importFinishedTitle || notif.Body != importFinishedCompletedBody {
		t.Fatalf("title/body = %q / %q, want the COMPLETED strings", notif.Title, notif.Body)
	}
	if len(repo.insertedDelivery) != 1 || repo.insertedDelivery[0].Channel != ChannelInApp || repo.insertedDelivery[0].Status != DeliveryQueued {
		t.Fatalf("inserted delivery = %+v, want one IN_APP/QUEUED", repo.insertedDelivery)
	}
	if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
		t.Fatalf("uow scopes = %v, want one %q", uow.scopes, tenantID)
	}
	if len(dedup.marked) != 1 || dedup.marked[0] != "evt-bf-1" || dedup.consumers[0] != consumerBackfill {
		t.Fatalf("dedup = {marked:%v consumers:%v}, want [evt-bf-1] under %q", dedup.marked, dedup.consumers, consumerBackfill)
	}
	// AC1: a fresh aviso is pushed once, on the tenant's channel, carrying the aviso.
	push := assertPushedOnce(t, pub)
	if push["id"] != notifID || push["type"] != TypeImportFinished {
		t.Fatalf("push id/type = %v/%v, want %q/%q", push["id"], push["type"], notifID, TypeImportFinished)
	}
	if push["title"] != importFinishedTitle || push["body"] != importFinishedCompletedBody {
		t.Fatalf("push title/body = %v/%v, want the COMPLETED strings", push["title"], push["body"])
	}
}

// assertPushedOnce asserts exactly one push landed on the fixture tenant's channel
// and returns its decoded JSON payload for field assertions.
func assertPushedOnce(t *testing.T, pub *fakePublisher) map[string]any {
	t.Helper()
	if len(pub.channels) != 1 {
		t.Fatalf("publishes = %d, want 1", len(pub.channels))
	}
	if want := "notif:" + tenantID; pub.channels[0] != want {
		t.Fatalf("push channel = %q, want %q", pub.channels[0], want)
	}
	var got map[string]any
	if err := json.Unmarshal(pub.payloads[0], &got); err != nil {
		t.Fatalf("push payload is not JSON: %v", err)
	}
	return got
}

// AC3: a PARTIAL backfill_finished names how many windows failed (slices_error) in the
// body, so the aviso says the import is incomplete without opening the job.
func TestInAppUseCase_OnBackfillFinished_PartialMentionsErrorsInBody(t *testing.T) {
	repo := repoInApp()
	uc := NewInAppUseCase(repo, &fakeDedup{}, &fakeUOW{}, &fakePublisher{})

	if err := uc.OnBackfillFinished(context.Background(), backfillFinished("evt-bf-partial", acquisition.BackfillStatusPartial, 3)); err != nil {
		t.Fatalf("OnBackfillFinished: %v", err)
	}

	if len(repo.insertedNotif) != 1 {
		t.Fatalf("inserted notifications = %d, want 1", len(repo.insertedNotif))
	}
	body := repo.insertedNotif[0].Body
	if body == importFinishedCompletedBody || !strings.Contains(body, "3") {
		t.Fatalf("PARTIAL body = %q, want it to cite the 3 failed windows", body)
	}
}

// AC4: a replay (dedup already seen) is a pure no-op — no notification, no delivery.
func TestInAppUseCase_OnBackfillFinished_ReplayIsNoOp(t *testing.T) {
	repo := repoInApp()
	repo.insertNotif = func(context.Context, database.Tx, InsertNotificationParams) (*Notification, error) {
		t.Fatal("insert notification ran on a replay")
		return nil, nil
	}
	pub := &fakePublisher{}
	uc := NewInAppUseCase(repo, &fakeDedup{seen: true}, &fakeUOW{}, pub)

	if err := uc.OnBackfillFinished(context.Background(), backfillFinished("evt-bf-dup", acquisition.BackfillStatusCompleted, 0)); err != nil {
		t.Fatalf("OnBackfillFinished: %v", err)
	}
	if len(repo.insertedNotif) != 0 || len(repo.insertedDelivery) != 0 {
		t.Fatalf("replay wrote: notif=%v delivery=%v", repo.insertedNotif, repo.insertedDelivery)
	}
	// AC2: a replay creates no aviso, so it pushes nothing.
	if len(pub.channels) != 0 {
		t.Fatalf("replay pushed %d times, want 0", len(pub.channels))
	}
}

// AC5: a docket_entry_observed is SUPPRESSED while a backfill is RUNNING — no aviso is
// created — but the event is still dedup-marked FIRST, so a redelivery after the
// backfill closes stays suppressed (the silence is permanent, not a race on state).
func TestInAppUseCase_OnDocketEntryObserved_SuppressedDuringBackfill(t *testing.T) {
	repo := repoInApp()
	repo.hasRunningBackfill = func(context.Context, database.Tx, string) (bool, error) { return true, nil }
	repo.insertNotif = func(context.Context, database.Tx, InsertNotificationParams) (*Notification, error) {
		t.Fatal("insert notification ran while a backfill was RUNNING")
		return nil, nil
	}
	dedup := &fakeDedup{}
	pub := &fakePublisher{}
	uc := NewInAppUseCase(repo, dedup, &fakeUOW{}, pub)

	if err := uc.OnDocketEntryObserved(context.Background(), docketEntryObserved("evt-dk-suppressed")); err != nil {
		t.Fatalf("OnDocketEntryObserved: %v", err)
	}

	if len(repo.insertedNotif) != 0 || len(repo.insertedDelivery) != 0 {
		t.Fatalf("suppressed event wrote: notif=%v delivery=%v", repo.insertedNotif, repo.insertedDelivery)
	}
	// The dedup mark landed BEFORE the suppression check — the event is consumed.
	if len(dedup.marked) != 1 || dedup.marked[0] != "evt-dk-suppressed" || dedup.consumers[0] != consumerDocket {
		t.Fatalf("dedup = {marked:%v consumers:%v}, want [evt-dk-suppressed] under %q", dedup.marked, dedup.consumers, consumerDocket)
	}
	// AC2: a suppressed docket entry creates no aviso, so it pushes nothing.
	if len(pub.channels) != 0 {
		t.Fatalf("suppressed event pushed %d times, want 0", len(pub.channels))
	}
}

// AC6: with no backfill running, a docket_entry_observed creates a new_andamento aviso
// + one IN_APP delivery QUEUED, tenant-level, in the tenant-scoped tx.
func TestInAppUseCase_OnDocketEntryObserved_CreatesNewAndamento(t *testing.T) {
	repo := repoInApp()
	dedup := &fakeDedup{}
	uow := &fakeUOW{}
	pub := &fakePublisher{}
	uc := NewInAppUseCase(repo, dedup, uow, pub)

	if err := uc.OnDocketEntryObserved(context.Background(), docketEntryObserved("evt-dk-1")); err != nil {
		t.Fatalf("OnDocketEntryObserved: %v", err)
	}

	if len(repo.insertedNotif) != 1 {
		t.Fatalf("inserted notifications = %d, want 1", len(repo.insertedNotif))
	}
	notif := repo.insertedNotif[0]
	if notif.Type != TypeNewAndamento || notif.Status != StatusCreated || notif.RecipientUserID != "" {
		t.Fatalf("notification = %+v, want new_andamento/CREATED/tenant-level", notif)
	}
	if notif.Title != newAndamentoTitle || notif.Body != newAndamentoBody {
		t.Fatalf("title/body = %q / %q, want the new_andamento strings", notif.Title, notif.Body)
	}
	if len(repo.insertedDelivery) != 1 || repo.insertedDelivery[0].Channel != ChannelInApp || repo.insertedDelivery[0].Status != DeliveryQueued {
		t.Fatalf("inserted delivery = %+v, want one IN_APP/QUEUED", repo.insertedDelivery)
	}
	if len(dedup.marked) != 1 || dedup.consumers[0] != consumerDocket {
		t.Fatalf("dedup = {marked:%v consumers:%v}, want one under %q", dedup.marked, dedup.consumers, consumerDocket)
	}
	// AC3: an incremental andamento (no backfill) is pushed once, carrying the aviso.
	push := assertPushedOnce(t, pub)
	if push["id"] != notifID || push["type"] != TypeNewAndamento || push["title"] != newAndamentoTitle {
		t.Fatalf("push = %v, want the new_andamento aviso", push)
	}
}

// AC7: a replay of a docket_entry_observed is a no-op — the backfill check never runs
// (dedup short-circuits before it) and nothing is written.
func TestInAppUseCase_OnDocketEntryObserved_ReplayIsNoOp(t *testing.T) {
	repo := repoInApp()
	repo.hasRunningBackfill = func(context.Context, database.Tx, string) (bool, error) {
		t.Fatal("backfill check ran on a replay")
		return false, nil
	}
	repo.insertNotif = func(context.Context, database.Tx, InsertNotificationParams) (*Notification, error) {
		t.Fatal("insert notification ran on a replay")
		return nil, nil
	}
	pub := &fakePublisher{}
	uc := NewInAppUseCase(repo, &fakeDedup{seen: true}, &fakeUOW{}, pub)

	if err := uc.OnDocketEntryObserved(context.Background(), docketEntryObserved("evt-dk-dup")); err != nil {
		t.Fatalf("OnDocketEntryObserved: %v", err)
	}
	if len(repo.insertedNotif) != 0 || len(repo.insertedDelivery) != 0 {
		t.Fatalf("replay wrote: notif=%v delivery=%v", repo.insertedNotif, repo.insertedDelivery)
	}
	// AC2: a replay creates no aviso, so it pushes nothing.
	if len(pub.channels) != 0 {
		t.Fatalf("replay pushed %d times, want 0", len(pub.channels))
	}
}

// AC4: a push that fails does not fail the handler — the aviso is already persisted,
// so the publish error is logged and swallowed (OnBackfillFinished returns nil). The
// use case still attempted the push exactly once (best-effort, no retry).
func TestInAppUseCase_OnBackfillFinished_PublishFailureIsSwallowed(t *testing.T) {
	repo := repoInApp()
	pub := &fakePublisher{err: errors.New("redis unreachable")}
	uc := NewInAppUseCase(repo, &fakeDedup{}, &fakeUOW{}, pub)

	if err := uc.OnBackfillFinished(context.Background(), backfillFinished("evt-bf-pub-fail", acquisition.BackfillStatusCompleted, 0)); err != nil {
		t.Fatalf("OnBackfillFinished should not propagate a push failure: %v", err)
	}

	// The aviso was still persisted (the push is best-effort, after the commit).
	if len(repo.insertedNotif) != 1 || len(repo.insertedDelivery) != 1 {
		t.Fatalf("aviso not persisted despite the push failure: notif=%v delivery=%v", repo.insertedNotif, repo.insertedDelivery)
	}
	// The push was attempted exactly once — a failure is not retried in the handler.
	if len(pub.channels) != 1 {
		t.Fatalf("publish attempts = %d, want 1 (no retry)", len(pub.channels))
	}
}

// fatia 4c: a deadline.due_soon creates one deadline-due-soon aviso + one IN_APP delivery
// QUEUED, tenant-level, in the tenant-scoped tx, and pushes it once. The body varies by
// days_left: "hoje" at 0, "em N dia(s)" otherwise.
func TestInAppUseCase_OnDeadlineDueSoon_CreatesAviso(t *testing.T) {
	tests := []struct {
		name     string
		daysLeft int
		wantBody string
	}{
		{name: "vence hoje", daysLeft: 0, wantBody: deadlineDueSoonTodayBody},
		{name: "vence em 1 dia", daysLeft: 1, wantBody: "Prazo vence em 1 dia(s)."},
		{name: "vence em 3 dias", daysLeft: 3, wantBody: "Prazo vence em 3 dia(s)."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := repoInApp()
			dedup := &fakeDedup{}
			uow := &fakeUOW{}
			pub := &fakePublisher{}
			uc := NewInAppUseCase(repo, dedup, uow, pub)

			if err := uc.OnDeadlineDueSoon(context.Background(), deadlineDueSoon("evt-ds-1", tt.daysLeft)); err != nil {
				t.Fatalf("OnDeadlineDueSoon: %v", err)
			}

			if len(repo.insertedNotif) != 1 {
				t.Fatalf("inserted notifications = %d, want 1", len(repo.insertedNotif))
			}
			notif := repo.insertedNotif[0]
			if notif.Type != TypeDeadlineDueSoonAviso || notif.Status != StatusCreated || notif.RecipientUserID != "" {
				t.Fatalf("notification = %+v, want deadline_due_soon/CREATED/tenant-level", notif)
			}
			if notif.Title != deadlineDueSoonTitle || notif.Body != tt.wantBody {
				t.Fatalf("title/body = %q / %q, want %q / %q", notif.Title, notif.Body, deadlineDueSoonTitle, tt.wantBody)
			}
			// The payload carries the source ids for the in-app UI to link back.
			if notif.Payload["deadline_id"] != "deadline-uuid" || notif.Payload["days_left"] != tt.daysLeft {
				t.Fatalf("payload = %v, want deadline_id + days_left", notif.Payload)
			}
			if len(repo.insertedDelivery) != 1 || repo.insertedDelivery[0].Channel != ChannelInApp || repo.insertedDelivery[0].Status != DeliveryQueued {
				t.Fatalf("inserted delivery = %+v, want one IN_APP/QUEUED", repo.insertedDelivery)
			}
			if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
				t.Fatalf("uow scopes = %v, want one %q", uow.scopes, tenantID)
			}
			if len(dedup.marked) != 1 || dedup.marked[0] != "evt-ds-1" || dedup.consumers[0] != consumerDeadlineDueSoon {
				t.Fatalf("dedup = {marked:%v consumers:%v}, want [evt-ds-1] under %q", dedup.marked, dedup.consumers, consumerDeadlineDueSoon)
			}
			// A fresh aviso is pushed once, carrying the materialized text.
			push := assertPushedOnce(t, pub)
			if push["id"] != notifID || push["type"] != TypeDeadlineDueSoonAviso || push["title"] != deadlineDueSoonTitle || push["body"] != tt.wantBody {
				t.Fatalf("push = %v, want the deadline_due_soon aviso", push)
			}
		})
	}
}

// fatia 4c: a replay of a deadline.due_soon (dedup already seen) is a pure no-op — no aviso,
// no delivery, no push.
func TestInAppUseCase_OnDeadlineDueSoon_ReplayIsNoOp(t *testing.T) {
	repo := repoInApp()
	repo.insertNotif = func(context.Context, database.Tx, InsertNotificationParams) (*Notification, error) {
		t.Fatal("insert notification ran on a replay")
		return nil, nil
	}
	pub := &fakePublisher{}
	uc := NewInAppUseCase(repo, &fakeDedup{seen: true}, &fakeUOW{}, pub)

	if err := uc.OnDeadlineDueSoon(context.Background(), deadlineDueSoon("evt-ds-dup", 3)); err != nil {
		t.Fatalf("OnDeadlineDueSoon: %v", err)
	}
	if len(repo.insertedNotif) != 0 || len(repo.insertedDelivery) != 0 {
		t.Fatalf("replay wrote: notif=%v delivery=%v", repo.insertedNotif, repo.insertedDelivery)
	}
	if len(pub.channels) != 0 {
		t.Fatalf("replay pushed %d times, want 0", len(pub.channels))
	}
}

// fatia 4c: a deadline.missed creates one "Prazo vencido" aviso + one IN_APP delivery QUEUED,
// tenant-level, and pushes it once.
func TestInAppUseCase_OnDeadlineMissed_CreatesAviso(t *testing.T) {
	repo := repoInApp()
	dedup := &fakeDedup{}
	uow := &fakeUOW{}
	pub := &fakePublisher{}
	uc := NewInAppUseCase(repo, dedup, uow, pub)

	if err := uc.OnDeadlineMissed(context.Background(), deadlineMissed("evt-ms-1")); err != nil {
		t.Fatalf("OnDeadlineMissed: %v", err)
	}

	if len(repo.insertedNotif) != 1 {
		t.Fatalf("inserted notifications = %d, want 1", len(repo.insertedNotif))
	}
	notif := repo.insertedNotif[0]
	if notif.Type != TypeDeadlineMissedAviso || notif.Status != StatusCreated || notif.RecipientUserID != "" {
		t.Fatalf("notification = %+v, want deadline_missed/CREATED/tenant-level", notif)
	}
	if notif.Title != deadlineMissedTitle || notif.Body != deadlineMissedBody {
		t.Fatalf("title/body = %q / %q, want the missed strings", notif.Title, notif.Body)
	}
	if notif.Payload["deadline_id"] != "deadline-uuid" {
		t.Fatalf("payload = %v, want deadline_id", notif.Payload)
	}
	if len(repo.insertedDelivery) != 1 || repo.insertedDelivery[0].Channel != ChannelInApp || repo.insertedDelivery[0].Status != DeliveryQueued {
		t.Fatalf("inserted delivery = %+v, want one IN_APP/QUEUED", repo.insertedDelivery)
	}
	if len(dedup.marked) != 1 || dedup.marked[0] != "evt-ms-1" || dedup.consumers[0] != consumerDeadlineMissed {
		t.Fatalf("dedup = {marked:%v consumers:%v}, want [evt-ms-1] under %q", dedup.marked, dedup.consumers, consumerDeadlineMissed)
	}
	push := assertPushedOnce(t, pub)
	if push["id"] != notifID || push["type"] != TypeDeadlineMissedAviso {
		t.Fatalf("push = %v, want the deadline_missed aviso", push)
	}
}

// fatia 4c: a replay of a deadline.missed is a pure no-op — no aviso, no delivery, no push.
func TestInAppUseCase_OnDeadlineMissed_ReplayIsNoOp(t *testing.T) {
	repo := repoInApp()
	repo.insertNotif = func(context.Context, database.Tx, InsertNotificationParams) (*Notification, error) {
		t.Fatal("insert notification ran on a replay")
		return nil, nil
	}
	pub := &fakePublisher{}
	uc := NewInAppUseCase(repo, &fakeDedup{seen: true}, &fakeUOW{}, pub)

	if err := uc.OnDeadlineMissed(context.Background(), deadlineMissed("evt-ms-dup")); err != nil {
		t.Fatalf("OnDeadlineMissed: %v", err)
	}
	if len(repo.insertedNotif) != 0 || len(repo.insertedDelivery) != 0 {
		t.Fatalf("replay wrote: notif=%v delivery=%v", repo.insertedNotif, repo.insertedDelivery)
	}
	if len(pub.channels) != 0 {
		t.Fatalf("replay pushed %d times, want 0", len(pub.channels))
	}
}

// fatia 2: a billing.trial_ending_soon creates one trial-ending-soon aviso + one IN_APP
// delivery QUEUED, tenant-level, in the tenant-scoped tx, and pushes it once. The body
// varies by days_left: "hoje" at 0, "em N dia(s)" otherwise — same split as due_soon.
func TestInAppUseCase_OnTrialEndingSoon_CreatesAviso(t *testing.T) {
	tests := []struct {
		name     string
		daysLeft int
		wantBody string
	}{
		{name: "termina hoje", daysLeft: 0, wantBody: trialEndingSoonTodayBody},
		{name: "termina em 2 dias", daysLeft: 2, wantBody: "Seu período de teste termina em 2 dia(s)."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := repoInApp()
			dedup := &fakeDedup{}
			uow := &fakeUOW{}
			pub := &fakePublisher{}
			uc := NewInAppUseCase(repo, dedup, uow, pub)

			if err := uc.OnTrialEndingSoon(context.Background(), trialEndingSoon("evt-tr-1", tt.daysLeft)); err != nil {
				t.Fatalf("OnTrialEndingSoon: %v", err)
			}

			if len(repo.insertedNotif) != 1 {
				t.Fatalf("inserted notifications = %d, want 1", len(repo.insertedNotif))
			}
			notif := repo.insertedNotif[0]
			if notif.Type != TypeTrialEndingSoonAviso || notif.Status != StatusCreated || notif.RecipientUserID != "" {
				t.Fatalf("notification = %+v, want trial_ending_soon/CREATED/tenant-level", notif)
			}
			if notif.Title != trialEndingSoonTitle || notif.Body != tt.wantBody {
				t.Fatalf("title/body = %q / %q, want %q / %q", notif.Title, notif.Body, trialEndingSoonTitle, tt.wantBody)
			}
			if notif.Payload["days_left"] != tt.daysLeft {
				t.Fatalf("payload = %v, want days_left", notif.Payload)
			}
			if len(repo.insertedDelivery) != 1 || repo.insertedDelivery[0].Channel != ChannelInApp || repo.insertedDelivery[0].Status != DeliveryQueued {
				t.Fatalf("inserted delivery = %+v, want one IN_APP/QUEUED", repo.insertedDelivery)
			}
			if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
				t.Fatalf("uow scopes = %v, want one %q", uow.scopes, tenantID)
			}
			if len(dedup.marked) != 1 || dedup.marked[0] != "evt-tr-1" || dedup.consumers[0] != consumerTrialEndingSoon {
				t.Fatalf("dedup = {marked:%v consumers:%v}, want [evt-tr-1] under %q", dedup.marked, dedup.consumers, consumerTrialEndingSoon)
			}
			push := assertPushedOnce(t, pub)
			if push["id"] != notifID || push["type"] != TypeTrialEndingSoonAviso || push["title"] != trialEndingSoonTitle || push["body"] != tt.wantBody {
				t.Fatalf("push = %v, want the trial_ending_soon aviso", push)
			}
		})
	}
}

// fatia 2: a replay of a billing.trial_ending_soon (dedup already seen) is a pure no-op —
// no aviso, no delivery, no push.
func TestInAppUseCase_OnTrialEndingSoon_ReplayIsNoOp(t *testing.T) {
	repo := repoInApp()
	repo.insertNotif = func(context.Context, database.Tx, InsertNotificationParams) (*Notification, error) {
		t.Fatal("insert notification ran on a replay")
		return nil, nil
	}
	pub := &fakePublisher{}
	uc := NewInAppUseCase(repo, &fakeDedup{seen: true}, &fakeUOW{}, pub)

	if err := uc.OnTrialEndingSoon(context.Background(), trialEndingSoon("evt-tr-dup", 2)); err != nil {
		t.Fatalf("OnTrialEndingSoon: %v", err)
	}
	if len(repo.insertedNotif) != 0 || len(repo.insertedDelivery) != 0 {
		t.Fatalf("replay wrote: notif=%v delivery=%v", repo.insertedNotif, repo.insertedDelivery)
	}
	if len(pub.channels) != 0 {
		t.Fatalf("replay pushed %d times, want 0", len(pub.channels))
	}
}

// AC9: ValidChannel accepts the two known channels and rejects everything else,
// including the empty string.
func TestValidChannel(t *testing.T) {
	tests := []struct {
		name    string
		channel string
		want    bool
	}{
		{name: "EMAIL", channel: ChannelEmail, want: true},
		{name: "IN_APP", channel: ChannelInApp, want: true},
		{name: "unknown", channel: "SMS", want: false},
		{name: "empty", channel: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidChannel(tt.channel); got != tt.want {
				t.Errorf("ValidChannel(%q) = %v, want %v", tt.channel, got, tt.want)
			}
		})
	}
}

// deliveryAt builds a persisted delivery fixture in a given status carrying a
// provider id, so the webhook use-case tests can drive the lookup → update flow.
func deliveryAt(status DeliveryStatus, providerID string) *NotificationDelivery {
	return &NotificationDelivery{
		ID:                deliveryID,
		NotificationID:    notifID,
		TenantID:          tenantID,
		Channel:           ChannelEmail,
		Status:            status,
		ProviderMessageID: providerID,
	}
}

// AC3/AC4: a provider bounce/complaint webhook locates the delivery by the provider's
// message id and flips its status in that delivery's tenant scope, preserving the
// provider id and recording the reason. An unknown id or an already-applied status is
// an idempotent no-op (the endpoint just acks).
func TestWebhookUseCase_MarkDeliveryOutcome(t *testing.T) {
	ctx := context.Background()

	t.Run("bounce flips the located delivery to BOUNCED under its tenant scope", func(t *testing.T) {
		repo := &mockRepo{
			findByProviderID: func(_ context.Context, providerID string) (*NotificationDelivery, error) {
				if providerID != "resend-abc" {
					t.Fatalf("looked up %q, want resend-abc", providerID)
				}
				return deliveryAt(DeliverySent, "resend-abc"), nil
			},
			updateStatus: func(_ context.Context, _ database.Tx, p UpdateDeliveryStatusParams) (*NotificationDelivery, error) {
				return &NotificationDelivery{ID: p.DeliveryID, Status: p.Status}, nil
			},
		}
		uow := &fakeUOW{}
		uc := NewWebhookUseCase(repo, uow)

		if err := uc.MarkDeliveryOutcome(ctx, "resend-abc", DeliveryBounced, "hard bounce"); err != nil {
			t.Fatalf("MarkDeliveryOutcome: %v", err)
		}
		// The write ran in the delivery's tenant scope (barrier 2).
		if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
			t.Fatalf("update scopes = %v, want one under %q", uow.scopes, tenantID)
		}
		// It flipped to BOUNCED, kept the provider id, and recorded the reason.
		if len(repo.updatedStatus) != 1 {
			t.Fatalf("updates = %d, want 1", len(repo.updatedStatus))
		}
		got := repo.updatedStatus[0]
		if got.DeliveryID != deliveryID || got.TenantID != tenantID || got.Status != DeliveryBounced {
			t.Fatalf("update = %+v, want BOUNCED for the delivery under its tenant", got)
		}
		if got.ProviderMessageID != "resend-abc" || got.Error != "hard bounce" {
			t.Fatalf("update = %+v, want provider id preserved and reason recorded", got)
		}
	})

	t.Run("complaint flips to COMPLAINED", func(t *testing.T) {
		repo := &mockRepo{
			findByProviderID: func(context.Context, string) (*NotificationDelivery, error) {
				return deliveryAt(DeliverySent, "resend-xyz"), nil
			},
			updateStatus: func(_ context.Context, _ database.Tx, p UpdateDeliveryStatusParams) (*NotificationDelivery, error) {
				return &NotificationDelivery{ID: p.DeliveryID, Status: p.Status}, nil
			},
		}
		uc := NewWebhookUseCase(repo, &fakeUOW{})

		if err := uc.MarkDeliveryOutcome(ctx, "resend-xyz", DeliveryComplained, complaintReason); err != nil {
			t.Fatalf("MarkDeliveryOutcome: %v", err)
		}
		if len(repo.updatedStatus) != 1 || repo.updatedStatus[0].Status != DeliveryComplained {
			t.Fatalf("updates = %+v, want one COMPLAINED", repo.updatedStatus)
		}
	})

	t.Run("unknown provider id is an ack — no update, no tx", func(t *testing.T) {
		repo := &mockRepo{
			findByProviderID: func(context.Context, string) (*NotificationDelivery, error) {
				return nil, ErrDeliveryNotFound
			},
		}
		uow := &fakeUOW{}
		uc := NewWebhookUseCase(repo, uow)

		if err := uc.MarkDeliveryOutcome(ctx, "never-sent", DeliveryBounced, "x"); err != nil {
			t.Fatalf("MarkDeliveryOutcome: %v", err)
		}
		if len(repo.updatedStatus) != 0 || len(uow.scopes) != 0 {
			t.Fatal("updated or opened a tx for an unknown provider id")
		}
	})

	t.Run("already at the target status is an idempotent no-op", func(t *testing.T) {
		repo := &mockRepo{
			findByProviderID: func(context.Context, string) (*NotificationDelivery, error) {
				return deliveryAt(DeliveryBounced, "resend-dup"), nil
			},
		}
		uow := &fakeUOW{}
		uc := NewWebhookUseCase(repo, uow)

		if err := uc.MarkDeliveryOutcome(ctx, "resend-dup", DeliveryBounced, "x"); err != nil {
			t.Fatalf("MarkDeliveryOutcome: %v", err)
		}
		if len(repo.updatedStatus) != 0 || len(uow.scopes) != 0 {
			t.Fatal("re-updated a delivery already at the target status")
		}
	})

	t.Run("an empty provider id is an ack — no lookup", func(t *testing.T) {
		called := false
		repo := &mockRepo{
			findByProviderID: func(context.Context, string) (*NotificationDelivery, error) {
				called = true
				return nil, nil
			},
		}
		uc := NewWebhookUseCase(repo, &fakeUOW{})

		if err := uc.MarkDeliveryOutcome(ctx, "", DeliveryBounced, "x"); err != nil {
			t.Fatalf("MarkDeliveryOutcome: %v", err)
		}
		if called {
			t.Fatal("looked up a delivery for an empty provider id")
		}
	})

	t.Run("a lookup infra fault propagates", func(t *testing.T) {
		boom := errors.New("pool unreachable")
		repo := &mockRepo{
			findByProviderID: func(context.Context, string) (*NotificationDelivery, error) {
				return nil, boom
			},
		}
		uc := NewWebhookUseCase(repo, &fakeUOW{})

		if err := uc.MarkDeliveryOutcome(ctx, "resend-err", DeliveryBounced, "x"); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the lookup fault", err)
		}
	})
}
