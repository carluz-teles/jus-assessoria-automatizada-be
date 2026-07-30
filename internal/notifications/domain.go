package notifications

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jusassessoria/platform/lib/database"
)

// consumerNotifications is the processed_event consumer name this slice dedups
// under. Each consumer dedups independently (docs §4c.3), so it is slice-specific.
const consumerNotifications = "notifications"

// deduper is the idempotency guard port. It marks (consumer, eventID) inside the
// caller's tx so the mark and the notification it guards commit together — a crash
// can never leave an event marked-but-not-applied. The slice adapter wraps
// events.Dedup bound to the tx.
type deduper interface {
	SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (seen bool, err error)
}

// NotifyUseCase turns a `notification.requested` event into a persisted aviso and a
// delivered e-mail. It depends on the Repository, Channel, deduper and UnitOfWork
// interfaces — never a concrete implementation (docs §2.5).
type NotifyUseCase struct {
	repo    Repository
	channel Channel
	dedup   deduper
	uow     database.UnitOfWork
}

// NewNotifyUseCase wires the use case to its repository, delivery channel, dedup
// guard and unit of work.
func NewNotifyUseCase(repo Repository, channel Channel, dedup deduper, uow database.UnitOfWork) *NotifyUseCase {
	return &NotifyUseCase{repo: repo, channel: channel, dedup: dedup, uow: uow}
}

// pendingSend carries, from the write tx to the send that follows it, everything
// needed to deliver and then record the outcome. It is non-nil only when a fresh
// QUEUED delivery was created — a replay (dedup no-op) or an unresolvable recipient
// leaves it nil, so no e-mail is attempted in those cases.
type pendingSend struct {
	deliveryID string
	tenantID   string
	email      string
	notifType  string
	payload    map[string]any
}

// OnNotificationRequested handles one `notification.requested`. It runs in two
// phases so an external send never holds a DB connection (the billing molde):
//
//  1. In one tenant-scoped tx: dedup the event (a replay marks nothing and returns
//     before any write — no duplicate aviso, no re-send); resolve the recipient's
//     e-mail; create the notification (CREATED); then create the delivery — QUEUED
//     when there is an address, or FAILED immediately when there is none (an
//     unreachable recipient is recorded, not allowed to wedge the queue).
//  2. After commit, and only when a QUEUED delivery was created: send the e-mail and
//     record the outcome (SENT + provider id, or FAILED + reason) in its own tx.
//
// Idempotency: the in-tx dedup is the guard. On a genuine replay it returns seen and
// this is a pure no-op; the delivery's UNIQUE(notification_id, channel) is the floor
// beneath that. So a QUEUED delivery reaching phase 2 is always first-and-only —
// nothing sends twice.
func (uc *NotifyUseCase) OnNotificationRequested(ctx context.Context, ev NotificationRequested) error {
	var pending *pendingSend

	err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerNotifications, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		email, err := uc.resolveEmail(ctx, tx, ev)
		if err != nil {
			return err
		}

		// Store the recipient id only when it resolved to a real app_user (email set);
		// otherwise NULL — recipient_user_id has an FK to app_user, so a dangling id
		// from the event must not be persisted (an unresolved recipient is still
		// recorded, as a tenant-level aviso with a FAILED delivery).
		recipient := ev.RecipientUserID
		if email == "" {
			recipient = ""
		}

		notif, err := uc.repo.InsertNotification(ctx, tx, InsertNotificationParams{
			TenantID:        ev.TenantID,
			RecipientUserID: recipient,
			Type:            ev.Type,
			Payload:         ev.Payload,
			Status:          StatusCreated,
		})
		if err != nil {
			return err
		}

		if email == "" {
			// No address — record the delivery as FAILED and stop (não trava).
			_, err := uc.repo.InsertDelivery(ctx, tx, InsertDeliveryParams{
				NotificationID: notif.ID,
				TenantID:       ev.TenantID,
				Channel:        ChannelEmail,
				Status:         DeliveryFailed,
				Error:          noRecipientEmailReason,
			})
			return err
		}

		delivery, err := uc.repo.InsertDelivery(ctx, tx, InsertDeliveryParams{
			NotificationID: notif.ID,
			TenantID:       ev.TenantID,
			Channel:        ChannelEmail,
			Status:         DeliveryQueued,
		})
		if err != nil {
			return err
		}

		pending = &pendingSend{
			deliveryID: delivery.ID,
			tenantID:   ev.TenantID,
			email:      email,
			notifType:  ev.Type,
			payload:    ev.Payload,
		}
		return nil
	})
	if err != nil {
		return err
	}
	if pending == nil {
		return nil // replay no-op, or the recorded-FAILED no-address path
	}

	return uc.deliver(ctx, *pending)
}

// resolveEmail returns the recipient's e-mail, or "" when there is none to deliver
// to — an event with no recipient, or a recipient not found in the tenant. Both
// collapse to the empty-address path (a FAILED delivery), so only a real infra fault
// propagates. The read runs inside the caller's tx: app_user is tenant-scoped, so
// RLS must be in force.
func (uc *NotifyUseCase) resolveEmail(ctx context.Context, tx database.Tx, ev NotificationRequested) (string, error) {
	if ev.RecipientUserID == "" {
		return "", nil
	}

	email, err := uc.repo.FindRecipientEmail(ctx, tx, ev.TenantID, ev.RecipientUserID)
	if errors.Is(err, ErrRecipientNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return email, nil
}

// deliver sends the e-mail (outside any tx) and records the outcome in its own
// tenant-scoped tx. A send failure is not returned: it is logged once and stored as
// a FAILED delivery (single-handling rule — the error becomes state, not a
// propagated value), so the task acks and the queue drains rather than retrying an
// address the provider already rejected (a genuine bounce is the 3d slice's job).
// Only a failure to RECORD the outcome propagates, as infra.
func (uc *NotifyUseCase) deliver(ctx context.Context, p pendingSend) error {
	providerID, sendErr := uc.channel.Send(ctx, EmailMessage{
		To:      p.email,
		Type:    p.notifType,
		Payload: p.payload,
	})

	status, msgID, errText := DeliverySent, providerID, ""
	if sendErr != nil {
		status, msgID, errText = DeliveryFailed, "", sendErr.Error()
		slog.ErrorContext(ctx, "notifications: email delivery failed",
			"delivery_id", p.deliveryID,
			"type", p.notifType,
			"error", sendErr,
		)
	}

	return uc.uow.Do(ctx, p.tenantID, func(tx database.Tx) error {
		_, err := uc.repo.UpdateDeliveryStatus(ctx, tx, UpdateDeliveryStatusParams{
			DeliveryID:        p.deliveryID,
			TenantID:          p.tenantID,
			Status:            status,
			ProviderMessageID: msgID,
			Error:             errText,
		})
		return err
	})
}
