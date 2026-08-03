package notifications

import (
	"context"
	"errors"
	"testing"

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
	insertNotif      func(ctx context.Context, tx database.Tx, p InsertNotificationParams) (*Notification, error)
	insertDelivery   func(ctx context.Context, tx database.Tx, p InsertDeliveryParams) (*NotificationDelivery, error)
	updateStatus     func(ctx context.Context, tx database.Tx, p UpdateDeliveryStatusParams) (*NotificationDelivery, error)
	findEmail        func(ctx context.Context, tx database.Tx, tenantID, appUserID string) (string, error)
	findByProviderID func(ctx context.Context, providerMessageID string) (*NotificationDelivery, error)

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
	seen   bool
	err    error
	marked []string
}

func (d *fakeDedup) SeenOrMark(_ context.Context, _ database.Tx, _ /*consumer*/, eventID string) (bool, error) {
	d.marked = append(d.marked, eventID)
	return d.seen, d.err
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
