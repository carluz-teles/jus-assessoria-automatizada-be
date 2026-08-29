package court

import (
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/database"
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
