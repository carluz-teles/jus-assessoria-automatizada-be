package court

import (
	"bytes"
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/eproc"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/vault"
)

// publisher is the narrow outbox port — the producer half of the transactional
// outbox. *events.Outbox satisfies it structurally.
type publisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

// UseCase orchestrates court_connection lifecycle: create, and Connect (the
// automated-MFA-enrollment-then-retry dance this fatia exists for). It never talks
// to a portal directly — that's what the providers map hides.
type UseCase struct {
	repo      Repository
	uow       database.UnitOfWork
	vault     *vault.Vault
	outbox    publisher
	providers map[string]CourtProvider // key: system, e.g. "EPROC"
	now       func() time.Time
}

// NewUseCase wires the use case. vault is required — a slice that persists MFA
// seeds and credentials with no vault configured is a boot-time config error, same
// convention certificate/draft follow for their own optional-but-required-if-used
// dependencies.
func NewUseCase(repo Repository, uow database.UnitOfWork, v *vault.Vault, outbox publisher) *UseCase {
	return &UseCase{
		repo:      repo,
		uow:       uow,
		vault:     v,
		outbox:    outbox,
		providers: make(map[string]CourtProvider),
		now:       time.Now,
	}
}

// RegisterProvider wires an adapter for one system (e.g. "EPROC" → EprocProvider).
// Called once at boot (cmd/api/main.go), never mid-request.
func (uc *UseCase) RegisterProvider(system string, p CourtProvider) {
	uc.providers[system] = p
}

// CreateConnection registers a new court_connection in DISCONNECTED status. It does
// NOT connect — the caller (handler) calls Connect right after, in a separate
// request/step, so a slow first connect never blocks the create response.
func (uc *UseCase) CreateConnection(ctx context.Context, tenantID, appUserID, court, system string, method AuthenticationMethod, credentialRef, certificateRef string) (*CourtConnection, error) {
	conn := &CourtConnection{
		TenantID:             tenantID,
		AppUserID:            appUserID,
		Court:                court,
		System:               system,
		AuthenticationMethod: method,
		CredentialRef:        credentialRef,
		CertificateRef:       certificateRef,
		Status:               StatusDisconnected,
	}
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		id, createdAt, err := uc.repo.Insert(ctx, tx, conn)
		if err != nil {
			return err
		}
		conn.ID = id
		conn.CreatedAt = createdAt
		return nil
	})
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// ListConnections is the read model for the FE's "Conexões com tribunais" screen.
func (uc *UseCase) ListConnections(ctx context.Context, tenantID string) ([]CourtConnection, error) {
	var out []CourtConnection
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		conns, err := uc.repo.List(ctx, tx, tenantID)
		out = conns
		return err
	})
	return out, err
}

// Connect authenticates one court_connection against its portal. This is the whole
// point of the fatia: when the provider signals ErrMFAEnrollmentRequired (no TOTP
// seed on file yet), Connect runs EnrollMFA automatically, seals the resulting seed
// into the vault, and retries Connect EXACTLY ONCE with it — no human, no phone, no
// "please paste the QR code" screen. A second MFA failure after that retry is a
// real error (enrollment itself is broken), not looped again.
func (uc *UseCase) Connect(ctx context.Context, tenantID, id string) (*CourtConnection, error) {
	var conn *CourtConnection
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		c, err := uc.repo.GetByID(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		conn = c
		return uc.repo.UpdateStatus(ctx, tx, tenantID, id, StatusAuthenticating, nil, "")
	})
	if err != nil {
		return nil, err
	}

	provider, ok := uc.providers[conn.System]
	if !ok {
		return nil, ErrProviderNotRegistered
	}

	seed, err := uc.openSeed(ctx, tenantID, conn.MFASeedRef)
	if err != nil {
		return nil, err
	}

	connectErr := provider.Connect(ctx, conn, seed)
	if connectErr == ErrMFAEnrollmentRequired {
		connectErr = uc.enrollAndRetry(ctx, provider, conn)
	}

	return conn, uc.finalizeConnect(ctx, tenantID, conn, connectErr)
}

// enrollAndRetry runs MFAEnroller.EnrollMFA (when the provider supports it), seals
// the seed, and retries Connect once. A provider without MFAEnroller (no automated
// enrollment support yet) surfaces ErrMFAEnrollmentFailed immediately — the use case
// never guesses at a fallback UX here.
func (uc *UseCase) enrollAndRetry(ctx context.Context, provider CourtProvider, conn *CourtConnection) error {
	enroller, ok := provider.(MFAEnroller)
	if !ok {
		return ErrMFAEnrollmentFailed
	}

	if err := uc.uow.Do(ctx, conn.TenantID, func(tx database.Tx) error {
		return uc.repo.UpdateStatus(ctx, tx, conn.TenantID, conn.ID, StatusMFAEnrollmentRequired, nil, "")
	}); err != nil {
		return err
	}

	seed, err := enroller.EnrollMFA(ctx, conn)
	if err != nil {
		return err
	}

	if err := uc.sealAndStoreSeed(ctx, conn, seed); err != nil {
		return err
	}

	return provider.Connect(ctx, conn, seed)
}

// sealAndStoreSeed seals seed into the vault, persists it as a tenant_secret, and
// points conn.MFASeedRef at it — the common tail of both the (future) automated
// EnrollMFA path and SubmitMFASeed's human-assisted one.
func (uc *UseCase) sealAndStoreSeed(ctx context.Context, conn *CourtConnection, seed string) error {
	sealed, err := uc.vault.Seal(seed)
	if err != nil {
		return err
	}

	var seedRef string
	if err := uc.uow.Do(ctx, conn.TenantID, func(tx database.Tx) error {
		ref, err := uc.repo.InsertSecret(ctx, tx, conn.TenantID, sealed)
		if err != nil {
			return err
		}
		seedRef = ref
		return uc.repo.UpdateMFASeedRef(ctx, tx, conn.TenantID, conn.ID, ref)
	}); err != nil {
		return err
	}
	conn.MFASeedRef = seedRef
	return nil
}

// SubmitMFASeed is the human-assisted enrollment path this fatia landed on after
// investigation: eproc's own MFA (re)configuration requires the lawyer's
// username/password, which a certificate-only connection doesn't have (see
// EprocProvider's doc) — so the lawyer captures their EXISTING/reconfigured TOTP QR
// ONCE, by hand, in their own browser (their certificate already lives in their OS's
// store, so THAT login is frictionless for them), and hands the result to this
// endpoint as either a screenshot or the manual-entry text. From here on, every
// future login generates its own code automatically (WithTOTPSeed) — no more
// screenshots, unlike the competitor pattern this was compared against.
//
// secret is the ALREADY-EXTRACTED TOTP secret (the handler calls totp.DecodeQRImage +
// totp.ExtractSecret on whatever the lawyer submitted before reaching this method —
// domain.go stays free of image-decoding concerns, same layering as everywhere else).
func (uc *UseCase) SubmitMFASeed(ctx context.Context, tenantID, id, secret string) (*CourtConnection, error) {
	var conn *CourtConnection
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		c, err := uc.repo.GetByID(ctx, tx, tenantID, id)
		conn = c
		return err
	})
	if err != nil {
		return nil, err
	}

	if err := uc.sealAndStoreSeed(ctx, conn, secret); err != nil {
		return nil, err
	}

	provider, ok := uc.providers[conn.System]
	if !ok {
		return nil, ErrProviderNotRegistered
	}

	connectErr := provider.Connect(ctx, conn, secret)
	return conn, uc.finalizeConnect(ctx, tenantID, conn, connectErr)
}

// finalizeConnect persists the outcome of a Connect attempt (success or the mapped
// status/error) and publishes court.connection_state_changed inside the same tx —
// the FE learns the outcome via that event (polling or a live subscription), never by
// guessing from the HTTP response of an async operation.
func (uc *UseCase) finalizeConnect(ctx context.Context, tenantID string, conn *CourtConnection, connectErr error) error {
	status := StatusConnected
	errMsg := ""
	var lastAuth *time.Time
	if connectErr != nil {
		status = classifyConnectError(connectErr)
		errMsg = connectErr.Error()
	} else {
		now := uc.now().UTC()
		lastAuth = &now
	}

	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		if err := uc.repo.UpdateStatus(ctx, tx, tenantID, conn.ID, status, lastAuth, errMsg); err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newConnectionStateChanged(tenantID, conn.ID, status))
	})
	if err != nil {
		return err
	}

	conn.Status = status
	conn.Error = errMsg
	conn.LastAuthenticatedAt = lastAuth
	return connectErr
}

// classifyConnectError maps a provider error onto the court_connection state machine.
// Intentionally coarse for now (this fatia's scope is "does Connect work end to end",
// not a full taxonomy of every eproc failure mode) — refine as real failures show up.
func classifyConnectError(err error) Status {
	if err == ErrMFAEnrollmentFailed {
		return StatusMFAEnrollmentRequired
	}
	return StatusError
}

// openSeed decrypts the MFA seed for a connection, or returns "" when none is on
// file yet (a brand-new connection, or one whose enrollment never completed).
func (uc *UseCase) openSeed(ctx context.Context, tenantID, mfaSeedRef string) (string, error) {
	if mfaSeedRef == "" {
		return "", nil
	}
	var sealed vault.Sealed
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		s, err := uc.repo.GetSecret(ctx, tx, tenantID, mfaSeedRef)
		sealed = s
		return err
	})
	if err != nil {
		return "", err
	}
	return uc.vault.Open(sealed)
}

// openSession decrypts the persisted session for a connection, or returns nil when
// none has been saved yet — WithSession no-ops on that (falls through to a normal
// login), same tolerant shape as an empty/corrupted session everywhere else.
func (uc *UseCase) openSession(ctx context.Context, tenantID, sessionRef string) (eproc.Session, error) {
	if sessionRef == "" {
		return nil, nil
	}
	var sealed vault.Sealed
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		s, err := uc.repo.GetSecret(ctx, tx, tenantID, sessionRef)
		sealed = s
		return err
	})
	if err != nil {
		return nil, err
	}
	raw, err := uc.vault.Open(sealed)
	if err != nil {
		return nil, err
	}
	return eproc.Session(raw), nil
}

// sealAndStoreSession seals session into the vault, persists it as a NEW
// tenant_secret (same "never mutate in place, repoint the reference" pattern
// sealAndStoreSeed uses), and points conn.SessionRef at it.
func (uc *UseCase) sealAndStoreSession(ctx context.Context, tenantID, connectionID string, session eproc.Session) (string, error) {
	sealed, err := uc.vault.Seal(string(session))
	if err != nil {
		return "", err
	}

	var ref string
	err = uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		r, err := uc.repo.InsertSecret(ctx, tx, tenantID, sealed)
		if err != nil {
			return err
		}
		ref = r
		return uc.repo.UpdateSessionRef(ctx, tx, tenantID, connectionID, r, uc.now().UTC())
	})
	if err != nil {
		return "", err
	}
	return ref, nil
}

// fetchAutosBatchSize bounds a single FetchAutosBatch execution — big enough to
// make real progress per task, small enough to keep task duration predictable.
// Overflow (more due records than fit) becomes a continuation event (listener.go),
// never a bigger batch.
const fetchAutosBatchSize = 20

// BatchResult is FetchAutosBatch's outcome: whether the listener should schedule a
// continuation (more due work than fit in this batch) and which specific items hit
// a TRANSIENT fault and need an individual retry — the caller (cmd/worker-court's
// task handler) turns RetryItems into individual FetchAutosItem asynq tasks with
// their own MaxRetry/backoff, rather than this domain method tight-looping the
// whole connection's continuation on a single stuck item.
type BatchResult struct {
	HasMore    bool
	RetryItems []FetchStateItem
}

// FetchAutosBatch pulls up to fetchAutosBatchSize due court_records for
// connectionID, reusing ONE session across all of them via CourtProvider.FetchAutos's
// threading contract (a login only happens on genuine staleness inside the
// provider, never once per item just because a fresh client was built).
//
// A connection not in CONNECTED status is skipped entirely — the listener is
// responsible for not even scheduling this against a broken connection in the
// first place (see listener.go), this is the last line of defense.
//
// Per-item classification: apperr Unauthorized/Forbidden means the provider's own
// re-login (triggered transparently on a stale session) ALSO failed — the
// CREDENTIAL itself is broken, not just the session. That aborts the whole batch
// and transitions conn.Status via the same path Connect uses, because insisting on
// the rest of the batch with a dead credential just produces more of the same
// failure. Any OTHER error is transient: that item is left due (not marked
// fetched) and returned in RetryItems for the caller to reenqueue individually —
// the rest of the batch keeps going.
func (uc *UseCase) FetchAutosBatch(ctx context.Context, tenantID, connectionID string) (BatchResult, error) {
	var conn *CourtConnection
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		c, err := uc.repo.GetByID(ctx, tx, tenantID, connectionID)
		conn = c
		return err
	})
	if err != nil {
		return BatchResult{}, err
	}
	if conn.Status != StatusConnected {
		return BatchResult{}, nil
	}

	provider, ok := uc.providers[conn.System]
	if !ok {
		return BatchResult{}, ErrProviderNotRegistered
	}

	seed, err := uc.openSeed(ctx, tenantID, conn.MFASeedRef)
	if err != nil {
		return BatchResult{}, err
	}
	originalSession, err := uc.openSession(ctx, tenantID, conn.SessionRef)
	if err != nil {
		return BatchResult{}, err
	}
	session := originalSession

	var due []FetchStateItem
	err = uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		items, err := uc.repo.ListDueFetchState(ctx, tx, connectionID, fetchAutosBatchSize+1)
		due = items
		return err
	})
	if err != nil {
		return BatchResult{}, err
	}
	if len(due) == 0 {
		return BatchResult{}, nil
	}

	result := BatchResult{}
	if len(due) > fetchAutosBatchSize {
		result.HasMore = true
		due = due[:fetchAutosBatchSize]
	}

	itemsFetched := 0
	var abortErr error
	for _, item := range due {
		var autos AutosResult
		autos, session, err = provider.FetchAutos(ctx, conn, seed, session, item.CourtRecordID, item.CNJNumber, cursorOrZero(item.DocketCursor))
		switch {
		case err == nil:
			if markErr := uc.markItemFetched(ctx, tenantID, connectionID, item.CourtRecordID, autos.LatestCursor); markErr != nil {
				return BatchResult{}, markErr
			}
			itemsFetched++
		case eproc.IsUnauthorized(err) || eproc.IsForbidden(err):
			abortErr = err
		default:
			result.RetryItems = append(result.RetryItems, item)
		}
		if abortErr != nil {
			break
		}
	}

	if err := uc.persistSessionIfChanged(ctx, tenantID, connectionID, originalSession, session); err != nil {
		return BatchResult{}, err
	}
	if err := uc.recordSyncRun(ctx, tenantID, itemsFetched, len(result.RetryItems), abortErr); err != nil {
		return BatchResult{}, err
	}

	if abortErr != nil {
		return BatchResult{}, uc.finalizeConnect(ctx, tenantID, conn, abortErr)
	}
	return result, nil
}

// FetchAutosItem fetches exactly ONE record, ignoring court_fetch_state's "due"
// window (a caller retrying a SPECIFIC known item after a transient fault wants
// THAT item, not whatever happens to be due now). Meant to run as an individually
// asynq-retried task (cmd/worker-court) — asynq's own MaxRetry/backoff bounds the
// attempts, so this never needs its own retry_count bookkeeping.
func (uc *UseCase) FetchAutosItem(ctx context.Context, tenantID, connectionID, courtRecordID, cnjNumber string) error {
	var conn *CourtConnection
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		c, err := uc.repo.GetByID(ctx, tx, tenantID, connectionID)
		conn = c
		return err
	})
	if err != nil {
		return err
	}
	if conn.Status != StatusConnected {
		return nil // connection already flagged broken elsewhere — nothing to do
	}

	provider, ok := uc.providers[conn.System]
	if !ok {
		return ErrProviderNotRegistered
	}

	seed, err := uc.openSeed(ctx, tenantID, conn.MFASeedRef)
	if err != nil {
		return err
	}
	session, err := uc.openSession(ctx, tenantID, conn.SessionRef)
	if err != nil {
		return err
	}

	var cursor time.Time
	err = uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		state, err := uc.repo.GetFetchState(ctx, tx, connectionID, courtRecordID)
		cursor = cursorOrZero(state.DocketCursor)
		return err
	})
	if err != nil {
		return err
	}

	autos, sessionOut, fetchErr := provider.FetchAutos(ctx, conn, seed, session, courtRecordID, cnjNumber, cursor)
	if err := uc.persistSessionIfChanged(ctx, tenantID, connectionID, session, sessionOut); err != nil {
		return err
	}

	if fetchErr != nil {
		if eproc.IsUnauthorized(fetchErr) || eproc.IsForbidden(fetchErr) {
			return uc.finalizeConnect(ctx, tenantID, conn, fetchErr)
		}
		return fetchErr // transient — the asynq task handler's own retry policy takes it from here
	}
	return uc.markItemFetched(ctx, tenantID, connectionID, courtRecordID, autos.LatestCursor)
}

// markItemFetched persists a successful fetch: last_fetched_at=now, docket_cursor
// advances only when the fetch actually returned events (a zero LatestCursor means
// "no docket activity found", not "reset the cursor to zero").
func (uc *UseCase) markItemFetched(ctx context.Context, tenantID, connectionID, courtRecordID string, latestCursor time.Time) error {
	var cursor *time.Time
	if !latestCursor.IsZero() {
		c := latestCursor
		cursor = &c
	}
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		return uc.repo.MarkFetchStateFetched(ctx, tx, connectionID, courtRecordID, uc.now().UTC(), cursor)
	})
}

// persistSessionIfChanged seals+stores the (possibly renewed) session only when it
// actually differs from what the caller started with — comparing raw bytes avoids
// an unconditional reseal+InsertSecret (a new tenant_secret row) on every batch even
// when the provider's session never needed renewing.
func (uc *UseCase) persistSessionIfChanged(ctx context.Context, tenantID, connectionID string, original, session eproc.Session) error {
	if len(session) == 0 || bytes.Equal(original, session) {
		return nil
	}
	_, err := uc.sealAndStoreSession(ctx, tenantID, connectionID, session)
	return err
}

// recordSyncRun opens+closes a sync_run row for this batch in one call — the batch
// is short-lived enough (one provider round-trip per item, sequential) that there's
// no value in a separately-visible RUNNING state the way DJEN's long OAB windows
// have; OK/FAILED is all a batch this size ever needs to show.
func (uc *UseCase) recordSyncRun(ctx context.Context, tenantID string, itemsFetched, itemsRetried int, abortErr error) error {
	now := uc.now().UTC()
	status := "OK"
	errMsg := ""
	if abortErr != nil {
		status = "FAILED"
		errMsg = abortErr.Error()
	}
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		runID, err := uc.repo.OpenSyncRun(ctx, tx, tenantID, now)
		if err != nil {
			return err
		}
		return uc.repo.CloseSyncRun(ctx, tx, runID, status, itemsFetched, itemsRetried, now, errMsg)
	})
}

// eprocSystemName is the only System value routed today — v0 simplification (see
// Repository.FindByCourtSystem's doc): a real CourtSystemResolver (TJSP → eproc |
// e-SAJ, reavaliada por sync) is future work once a second system exists to choose
// between.
const eprocSystemName = "EPROC"

// OnCourtRecordObserved is FetchAutosBatch's arrival trigger: resolves whether
// ev's tribunal has a CONNECTED court_connection for this tenant, and if so,
// records the observation (court_fetch_state) and requests a batch. No matching or
// no CONNECTED connection is the COMMON case (most observed records aren't owned
// by a connected advogado, or the connection needs attention) — an expected no-op,
// never an error, so every future observation for a broken connection doesn't keep
// scheduling doomed work against it (see fetchAutosRequested's doc for why the
// listener, not the batch, is the gate).
func (uc *UseCase) OnCourtRecordObserved(ctx context.Context, ev courtRecordObserved) error {
	var conn *CourtConnection
	err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		c, err := uc.repo.FindByCourtSystem(ctx, tx, ev.TenantID, ev.Court, eprocSystemName)
		conn = c
		return err
	})
	if err == ErrConnectionNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if conn.Status != StatusConnected {
		return nil
	}

	now := uc.now().UTC()
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		if err := uc.repo.UpsertFetchStateObserved(ctx, tx, ev.TenantID, conn.ID, ev.CourtRecordID, ev.CNJNumber, now); err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newFetchAutosRequested(ev.TenantID, conn.ID))
	})
}

// OnFetchAutosRequested runs one FetchAutosBatch step and reacts to its outcome:
// HasMore schedules the NEXT step (fresh event id, see fetchAutosRequested's doc);
// each RetryItem gets its own fetch_autos_item_requested event (bounded by the
// relay's own MaxRetry, see that event's doc).
func (uc *UseCase) OnFetchAutosRequested(ctx context.Context, ev fetchAutosRequested) error {
	result, err := uc.FetchAutosBatch(ctx, ev.TenantID, ev.ConnectionID)
	if err != nil {
		return err
	}

	now := uc.now().UTC()
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		if result.HasMore {
			if err := uc.outbox.Publish(ctx, tx, ev.nextFetchAutosStep(now)); err != nil {
				return err
			}
		}
		for _, item := range result.RetryItems {
			if err := uc.outbox.Publish(ctx, tx, newFetchAutosItemRequested(ev.TenantID, ev.ConnectionID, item)); err != nil {
				return err
			}
		}
		return nil
	})
}

// OnFetchAutosItemRequested retries exactly the one record the event names —
// asynq's own retry/backoff (bounded by the relay's MaxRetry) drives further
// attempts if this one also fails transiently.
func (uc *UseCase) OnFetchAutosItemRequested(ctx context.Context, ev fetchAutosItemRequested) error {
	return uc.FetchAutosItem(ctx, ev.TenantID, ev.ConnectionID, ev.CourtRecordID, ev.CNJNumber)
}

// cursorOrZero unwraps a possibly-nil stored docket_cursor: nil means "never
// fetched", which the zero time.Time already represents to CourtProvider.FetchAutos
// (every event counts as new).
func cursorOrZero(cursor *time.Time) time.Time {
	if cursor == nil {
		return time.Time{}
	}
	return *cursor
}
