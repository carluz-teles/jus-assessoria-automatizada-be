package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jusassessoria/platform/internal/acquisition"
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

// publisher is the best-effort real-time push port. The in-app use case calls it
// AFTER the write commits, so the SSE endpoint (a later slice) can deliver a fresh
// aviso the moment it lands. It is NOT the transactional outbox: a publish failure
// is logged and swallowed (the aviso is already persisted — the client picks it up
// on the next load). The slice adapter is lib/pubsub over Redis.
type publisher interface {
	Publish(ctx context.Context, channel string, payload []byte) error
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

		if enabled, err := uc.channelEnabled(ctx, tx, ev.TenantID, recipient, ev.Type, ChannelEmail); err != nil {
			return err
		} else if !enabled {
			// The recipient opted this type out of EMAIL: the notification fact above
			// still committed (it stays visible wherever it is visible today — the
			// preference only ever gates the SEND), but no delivery is queued and no
			// e-mail goes out. Recorded as SKIPPED, not FAILED: nothing went wrong.
			_, err := uc.repo.InsertDelivery(ctx, tx, InsertDeliveryParams{
				NotificationID: notif.ID,
				TenantID:       ev.TenantID,
				Channel:        ChannelEmail,
				Status:         DeliverySkipped,
				Error:          skippedByPreferenceReason,
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

// channelEnabled reports whether channel is enabled for (tenantID, recipientID,
// notifType), consulting notification_preference inside the caller's tx (RLS
// scopes it to the event's tenant, same as resolveEmail). ErrPreferenceNotFound —
// the recipient never overrode this type — is the default: every channel is
// enabled, so a tenant that never touches preferences behaves exactly as before
// this slice existed. recipientID empty (a tenant-level aviso) also defaults to
// enabled: preferences are per-user, and a tenant-level aviso has no single user
// to look one up for.
func (uc *NotifyUseCase) channelEnabled(ctx context.Context, tx database.Tx, tenantID, recipientID, notifType, channel string) (bool, error) {
	if recipientID == "" {
		return true, nil
	}

	channels, err := uc.repo.FindPreferenceChannels(ctx, tx, tenantID, recipientID, notifType)
	if errors.Is(err, ErrPreferenceNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return slices.Contains(channels, channel), nil
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

// skippedByPreferenceReason is the delivery.error text recorded when the recipient
// explicitly opted this type out of the channel — a human-readable reason, not a
// sentinel, mirroring noRecipientEmailReason.
const skippedByPreferenceReason = "recipient opted this type out of this channel"

// PreferenceUseCase backs GET/PUT /v1/notifications/preferences: the caller's own
// saved channel overrides. It depends on the Repository and UnitOfWork interfaces,
// never a concrete implementation (docs §2.5).
type PreferenceUseCase struct {
	repo Repository
	uow  database.UnitOfWork
}

// NewPreferenceUseCase wires the preference use case to its repository and unit of
// work.
func NewPreferenceUseCase(repo Repository, uow database.UnitOfWork) *PreferenceUseCase {
	return &PreferenceUseCase{repo: repo, uow: uow}
}

// GetPreferences reads the caller's saved overrides — a plain pool read (no tx),
// scoped to (tenantID, appUserID) from the verified principal, never the request.
func (uc *PreferenceUseCase) GetPreferences(ctx context.Context, tenantID, appUserID string) ([]NotificationPreference, error) {
	return uc.repo.ListPreferences(ctx, tenantID, appUserID)
}

// SetPreference saves the caller's full enabled-channel set for one notification
// type. tenantID/appUserID come from the verified principal, never the body — a
// caller can only ever set their OWN preference. channels is the whole set, not a
// delta (ON CONFLICT in the query replaces it); an empty slice is valid and means
// "no channel enabled for this type" (an explicit full opt-out), distinct from no
// row at all (the default, every channel enabled).
func (uc *PreferenceUseCase) SetPreference(ctx context.Context, tenantID, appUserID, notifType string, channels []string) (*NotificationPreference, error) {
	var pref *NotificationPreference
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		pref, err = uc.repo.UpsertPreference(ctx, tx, tenantID, appUserID, notifType, channels)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pref, nil
}

// Materialized PT text for the in-app avisos (slice 1a). The in-app channel has no
// render step (unlike EMAIL), so the use case writes these strings straight onto the
// notification. The import_finished body varies by outcome; the others are fixed.
const (
	importFinishedTitle         = "Importação de processos concluída"
	importFinishedCompletedBody = "A importação inicial dos seus processos foi concluída com sucesso."
	importFinishedPartialBody   = "A importação inicial dos seus processos foi concluída, mas %d janela(s) não puderam ser sincronizadas e serão retentadas automaticamente."

	newAndamentoTitle = "Novo andamento processual"
	newAndamentoBody  = "Um novo andamento foi identificado em um dos seus processos."

	// Deadline avisos (fatia 4c). The due_soon title is fixed; its body varies by
	// days_left (0 → "hoje", else "em N dia(s)"). The missed aviso is fully fixed.
	deadlineDueSoonTitle     = "Prazo a vencer"
	deadlineDueSoonTodayBody = "Prazo vence hoje."
	deadlineDueSoonBody      = "Prazo vence em %d dia(s)."
	deadlineMissedTitle      = "Prazo vencido"
	deadlineMissedBody       = "Um prazo venceu sem confirmação."

	// Trial aviso (fatia 2). The body varies by days_left the same way
	// deadlineDueSoonBody does (0 → "hoje").
	trialEndingSoonTitle     = "Período de teste terminando"
	trialEndingSoonTodayBody = "Seu período de teste termina hoje."
	trialEndingSoonBody      = "Seu período de teste termina em %d dia(s)."
)

// InAppUseCase turns two acquisition events into IN_APP avisos (slice 1a): a
// backfill_finished becomes one import_finished aviso; a docket_entry_observed becomes
// one new_andamento aviso, UNLESS the tenant's onboarding backfill is still RUNNING (the
// bulk import's single import_finished aviso covers that window). It records the aviso
// fact plus one QUEUED IN_APP delivery in the event's tenant scope — no external send
// (real-time push is a later slice). It depends on the Repository, deduper and
// UnitOfWork interfaces, never a concrete implementation (docs §2.5).
type InAppUseCase struct {
	repo  Repository
	dedup deduper
	uow   database.UnitOfWork
	pub   publisher
}

// NewInAppUseCase wires the in-app use case to its repository, dedup guard, unit of
// work and the best-effort push publisher (used only after a fresh aviso commits).
func NewInAppUseCase(repo Repository, dedup deduper, uow database.UnitOfWork, pub publisher) *InAppUseCase {
	return &InAppUseCase{repo: repo, dedup: dedup, uow: uow, pub: pub}
}

// inAppPush is the JSON envelope the SSE endpoint (a later slice) relays to the
// browser. It carries exactly what the in-app inbox needs to render the new aviso
// without a refetch — the full row is still the source of truth in Postgres.
type inAppPush struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// pushChannel is the per-tenant Redis channel an aviso is published on, so a
// subscriber only ever sees its own tenant's pushes (isolation at the fan-out too).
func pushChannel(tenantID string) string { return "notif:" + tenantID }

// OnBackfillFinished handles one acquisition.backfill_finished: in the event's tenant
// scope it dedups (a replay marks nothing new and returns before any write), then
// records an import_finished aviso whose body reports the outcome (a clean COMPLETED,
// or a PARTIAL naming how many windows failed). The dedup mark and the aviso commit
// together — a crash never leaves the event marked-but-unrecorded.
func (uc *InAppUseCase) OnBackfillFinished(ctx context.Context, ev BackfillFinished) error {
	var created *Notification
	err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerBackfill, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		title, body := renderImportFinished(ev)
		notif, err := uc.record(ctx, tx, ev.TenantID, TypeImportFinished, title, body, map[string]any{
			"backfill_job_id": ev.BackfillJobID,
			"integration_id":  ev.IntegrationID,
			"status":          ev.Status,
			"total_slices":    ev.TotalSlices,
			"slices_ok":       ev.SlicesOK,
			"slices_error":    ev.SlicesError,
		})
		if err != nil {
			return err
		}
		created = notif
		return nil
	})
	if err != nil {
		return err
	}
	// Push only a genuinely new aviso, and only after the commit so the row the SSE
	// endpoint fetches is already visible. A replay (created == nil) pushes nothing.
	uc.publish(ctx, ev.TenantID, TypeBackfillFinished, created)
	return nil
}

// OnDocketEntryObserved handles one acquisition.docket_entry_observed. In the event's
// tenant scope it dedups FIRST (so the event is consumed exactly once regardless of
// what follows), then SUPPRESSES the aviso when a backfill is still RUNNING for the
// tenant — the onboarding import's import_finished aviso already stands in for that
// burst of andamentos, so a per-entry aviso would be noise. Marking before suppressing
// makes the silence permanent: a redelivery after the backfill closes is a dedup
// no-op, not a late aviso. Otherwise it records a new_andamento aviso.
func (uc *InAppUseCase) OnDocketEntryObserved(ctx context.Context, ev DocketEntryObserved) error {
	var created *Notification
	err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerDocket, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		running, err := uc.repo.HasRunningBackfillForTenant(ctx, tx, ev.TenantID)
		if err != nil {
			return err
		}
		if running {
			return nil // suppressed during onboarding — the dedup mark above stands
		}

		notif, err := uc.record(ctx, tx, ev.TenantID, TypeNewAndamento, newAndamentoTitle, newAndamentoBody, map[string]any{
			"court_record_id": ev.CourtRecordID,
			"docket_entry_id": ev.DocketEntryID,
			"sync_run_id":     ev.SyncRunID,
		})
		if err != nil {
			return err
		}
		created = notif
		return nil
	})
	if err != nil {
		return err
	}
	// Push only outside the backfill window and only for a fresh aviso: a suppressed
	// or replayed event leaves created == nil, so nothing is pushed.
	uc.publish(ctx, ev.TenantID, TypeDocketEntryObserved, created)
	return nil
}

// OnDeadlineDueSoon handles one deadline.due_soon (fatia 4c). In the event's tenant scope it
// dedups FIRST (so the event is consumed exactly once), then records a deadline-due-soon aviso
// whose body reports how many days remain — or "hoje" at zero. The dedup mark and the aviso
// commit together, so a crash never leaves the event marked-but-unrecorded. Unlike the docket
// aviso, it is NEVER suppressed during onboarding: a prazo nearing its vencimento always
// warrants surfacing (and the deadline slice only emits it for a re-checked, still-active prazo).
func (uc *InAppUseCase) OnDeadlineDueSoon(ctx context.Context, ev DeadlineDueSoon) error {
	var created *Notification
	err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerDeadlineDueSoon, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		title, body := renderDeadlineDueSoon(ev.DaysLeft)
		notif, err := uc.record(ctx, tx, ev.TenantID, TypeDeadlineDueSoonAviso, title, body, map[string]any{
			"deadline_id": ev.DeadlineID,
			"days_left":   ev.DaysLeft,
		})
		if err != nil {
			return err
		}
		created = notif
		return nil
	})
	if err != nil {
		return err
	}
	// Push only a genuinely new aviso, after the commit (a replay leaves created == nil).
	uc.publish(ctx, ev.TenantID, TypeDeadlineDueSoon, created)
	return nil
}

// OnDeadlineMissed handles one deadline.missed (fatia 4c): a prazo auto-marked MISSED at the
// D+1 carência. Same tenant-scoped dedup-then-record shape as OnDeadlineDueSoon, with a fixed
// "Prazo vencido" aviso and the deadline id in the payload.
func (uc *InAppUseCase) OnDeadlineMissed(ctx context.Context, ev DeadlineMissed) error {
	var created *Notification
	err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerDeadlineMissed, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		notif, err := uc.record(ctx, tx, ev.TenantID, TypeDeadlineMissedAviso, deadlineMissedTitle, deadlineMissedBody, map[string]any{
			"deadline_id": ev.DeadlineID,
		})
		if err != nil {
			return err
		}
		created = notif
		return nil
	})
	if err != nil {
		return err
	}
	uc.publish(ctx, ev.TenantID, TypeDeadlineMissed, created)
	return nil
}

// OnTrialEndingSoon handles one billing.trial_ending_soon (fatia 2). In the
// event's tenant scope it dedups FIRST (so the event is consumed exactly once),
// then records a trial-ending-soon aviso whose body reports how many days remain.
// Same dedup-then-record shape as OnDeadlineDueSoon; billing's fire handler
// already re-checked the subscription's live state before emitting this event, so
// this consumer trusts the payload as-is.
func (uc *InAppUseCase) OnTrialEndingSoon(ctx context.Context, ev TrialEndingSoon) error {
	var created *Notification
	err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerTrialEndingSoon, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		title, body := renderTrialEndingSoon(ev.DaysLeft)
		notif, err := uc.record(ctx, tx, ev.TenantID, TypeTrialEndingSoonAviso, title, body, map[string]any{
			"trial_ends_at": ev.TrialEndsAt,
			"days_left":     ev.DaysLeft,
		})
		if err != nil {
			return err
		}
		created = notif
		return nil
	})
	if err != nil {
		return err
	}
	uc.publish(ctx, ev.TenantID, TypeTrialEndingSoon, created)
	return nil
}

// renderDeadlineDueSoon materializes the due_soon title/body from days_left. The title is
// fixed; the body is the "hoje" text at zero, else the "em N dia(s)" text — so the aviso
// tells the user exactly how much runway is left without opening the prazo.
func renderDeadlineDueSoon(daysLeft int) (title, body string) {
	if daysLeft == 0 {
		return deadlineDueSoonTitle, deadlineDueSoonTodayBody
	}
	return deadlineDueSoonTitle, fmt.Sprintf(deadlineDueSoonBody, daysLeft)
}

// renderTrialEndingSoon materializes the trial_ending_soon title/body from
// days_left, mirroring renderDeadlineDueSoon's "hoje" vs "em N dia(s)" split.
func renderTrialEndingSoon(daysLeft int) (title, body string) {
	if daysLeft == 0 {
		return trialEndingSoonTitle, trialEndingSoonTodayBody
	}
	return trialEndingSoonTitle, fmt.Sprintf(trialEndingSoonBody, daysLeft)
}

// record writes the tenant-level aviso fact (CREATED) and its single IN_APP delivery
// (QUEUED) in the caller's tx — the in-app analog of the email use case's create phase,
// minus the external send. title/body are materialized here; the payload carries the
// source ids for the in-app UI to link back.
func (uc *InAppUseCase) record(ctx context.Context, tx database.Tx, tenantID, notifType, title, body string, payload map[string]any) (*Notification, error) {
	notif, err := uc.repo.InsertNotification(ctx, tx, InsertNotificationParams{
		TenantID: tenantID,
		Type:     notifType,
		Title:    title,
		Body:     body,
		Payload:  payload,
		Status:   StatusCreated,
	})
	if err != nil {
		return nil, err
	}

	_, err = uc.repo.InsertDelivery(ctx, tx, InsertDeliveryParams{
		NotificationID: notif.ID,
		TenantID:       tenantID,
		Channel:        ChannelInApp,
		Status:         DeliveryQueued,
	})
	if err != nil {
		return nil, err
	}
	return notif, nil
}

// publish fires the best-effort real-time push for a freshly-created aviso. It is a
// no-op when notif is nil (a replay or a suppressed docket entry created nothing).
// Both a marshal fault and a publish fault are LOGGED and swallowed (single-handling
// rule): the aviso is already committed, so a failed push must not fail the handler
// and make asynq retry — the client just gets the aviso on its next fetch instead.
func (uc *InAppUseCase) publish(ctx context.Context, tenantID, eventType string, notif *Notification) {
	if notif == nil {
		return
	}

	payload, err := json.Marshal(inAppPush{
		ID:        notif.ID,
		Type:      notif.Type,
		Title:     notif.Title,
		Body:      notif.Body,
		CreatedAt: notif.CreatedAt,
	})
	if err != nil {
		slog.ErrorContext(ctx, "notifications: in-app push marshal failed",
			"tenant_id", tenantID,
			"notification_id", notif.ID,
			"event_type", eventType,
			"error", err,
		)
		return
	}

	if err := uc.pub.Publish(ctx, pushChannel(tenantID), payload); err != nil {
		slog.ErrorContext(ctx, "notifications: in-app push publish failed",
			"tenant_id", tenantID,
			"notification_id", notif.ID,
			"event_type", eventType,
			"error", err,
		)
	}
}

// renderImportFinished materializes the title/body for an import_finished aviso. The
// title is fixed; the body is the clean COMPLETED text, or the PARTIAL text naming how
// many windows failed (slices_error) so the user sees the import is incomplete.
func renderImportFinished(ev BackfillFinished) (title, body string) {
	if ev.Status == acquisition.BackfillStatusPartial {
		return importFinishedTitle, fmt.Sprintf(importFinishedPartialBody, ev.SlicesError)
	}
	return importFinishedTitle, importFinishedCompletedBody
}

// WebhookUseCase records a provider-callback outcome (a Resend bounce/complaint) onto
// the delivery it concerns. It is the api-side counterpart to the listener's
// NotifyUseCase: it needs no Channel and no dedup registration — it only locates a
// delivery by the provider's message id and flips its status. It depends on the
// Repository and UnitOfWork interfaces, never a concrete implementation (docs §2.5).
type WebhookUseCase struct {
	repo Repository
	uow  database.UnitOfWork
}

// NewWebhookUseCase wires the provider-webhook use case to its repository and unit of
// work. The repository must be pool-backed (NewRepository) — the delivery lookup is
// tenant-less and runs outside any tx.
func NewWebhookUseCase(repo Repository, uow database.UnitOfWork) *WebhookUseCase {
	return &WebhookUseCase{repo: repo, uow: uow}
}

// MarkDeliveryOutcome flips the delivery identified by the provider's message id to
// status (BOUNCED or COMPLAINED), stamping reason on its error. The lookup runs on
// the pool (the webhook carries no tenant); the write runs in that delivery's tenant
// scope, so barrier 2 (RLS) still guards it. The provider id is preserved, not
// re-derived, so the correlation to the original send survives.
//
// Idempotent for at-least-once webhook delivery: an unknown message id (nothing we
// sent) and a status already at the target both collapse to a no-op so the endpoint
// simply acks and the provider stops retrying.
func (uc *WebhookUseCase) MarkDeliveryOutcome(ctx context.Context, providerMessageID string, status DeliveryStatus, reason string) error {
	if providerMessageID == "" {
		return nil // nothing to correlate — ack
	}

	delivery, err := uc.repo.FindDeliveryByProviderMessageID(ctx, providerMessageID)
	if errors.Is(err, ErrDeliveryNotFound) {
		return nil // an id we never sent — ack, do not make the provider retry
	}
	if err != nil {
		return err
	}
	if delivery.Status == status {
		return nil // replay of the same outcome — already recorded
	}

	return uc.uow.Do(ctx, delivery.TenantID, func(tx database.Tx) error {
		_, err := uc.repo.UpdateDeliveryStatus(ctx, tx, UpdateDeliveryStatusParams{
			DeliveryID:        delivery.ID,
			TenantID:          delivery.TenantID,
			Status:            status,
			ProviderMessageID: delivery.ProviderMessageID,
			Error:             reason,
		})
		return err
	})
}
