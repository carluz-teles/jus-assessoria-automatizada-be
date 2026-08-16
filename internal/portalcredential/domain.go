package portalcredential

import (
	"context"
	"errors"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/vault"
)

// UseCase carries the portalcredential use cases. It depends on the Repository
// interface, the PortalLoginChecker port and the UnitOfWork — never on a
// concrete pg/HTTP implementation (CLAUDE.md: domain.go depends on interfaces).
type UseCase struct {
	repo    Repository
	checker PortalLoginChecker
	vault   *vault.Vault
	uow     database.UnitOfWork
}

// NewUseCase wires the portalcredential use cases to their dependencies.
func NewUseCase(repo Repository, checker PortalLoginChecker, v *vault.Vault, uow database.UnitOfWork) *UseCase {
	return &UseCase{repo: repo, checker: checker, vault: v, uow: uow}
}

// Configure is PUT /v1/scraping/portal-credential's use case: it tests the
// (login, password) pair against the REAL TJSP eproc portal BEFORE persisting
// anything (docs/erd-tribunal-scraping.md's "validação síncrona real"), then
// writes the outcome in one transaction:
//
//   - LoginOutcomeSuccess  → seals the password, upserts ACTIVE + last_verified_at=now(),
//     and returns the credential with no error — the handler answers 200.
//   - LoginOutcomeRejected → NEVER persists as ACTIVE; returns ErrPortalRejectedCredential
//     without writing anything, so a wrong password never overwrites a working
//     credential. The handler maps this to 400.
//   - LoginOutcomeInconclusive → still seals and persists (a network hiccup or an
//     unrecognized page must not block the advogado from saving), but as
//     AUTH_FAILED with LastError carrying the inconclusive detail and no
//     last_verified_at bump — the UI shows "salvo, verificação pendente", not
//     an error. The handler answers 200 either way.
//
// tenantID/appUserID come from the verified principal (never the body);
// login/password come from the validated request body.
func (uc *UseCase) Configure(ctx context.Context, tenantID, appUserID, login, password string) (*PortalCredential, error) {
	result, err := uc.checker.Check(ctx, login, password)
	if err != nil {
		return nil, err // context cancelled/expired — propagate, nothing persisted
	}

	if result.Outcome == LoginOutcomeRejected {
		return nil, ErrPortalRejectedCredential
	}

	sealed, err := uc.vault.Seal(password)
	if err != nil {
		return nil, err
	}

	status := StatusActive
	lastError := ""
	var lastVerifiedAt time.Time
	if result.Outcome == LoginOutcomeSuccess {
		lastVerifiedAt = time.Now().UTC()
	} else {
		status = StatusAuthFailed
		lastError = result.Detail
	}

	var saved *PortalCredential
	err = uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		// A reconfigure may already have a credential (and a secret it points
		// at); resolve it first so the OLD secret can be cleaned up after the
		// new one lands — never leave two sealed passwords for the same
		// advogado×portal outliving the credential that references only one.
		var oldSecretRef string
		existing, err := uc.repo.GetPortalCredential(ctx, tx, tenantID, appUserID, PortalTJSPEproc)
		if err == nil {
			oldSecretRef = existing.CredentialRef
		} else if !errors.Is(err, ErrPortalCredentialNotFound) {
			return err
		}

		secretID, err := uc.repo.InsertSecret(ctx, tx, tenantID, sealed)
		if err != nil {
			return err
		}

		cred, err := uc.repo.UpsertPortalCredential(ctx, tx, UpsertPortalCredentialParams{
			TenantID:       tenantID,
			AppUserID:      appUserID,
			Portal:         PortalTJSPEproc,
			Login:          login,
			CredentialRef:  secretID,
			Status:         status,
			LastError:      lastError,
			LastVerifiedAt: lastVerifiedAt,
			ConfiguredBy:   appUserID,
		})
		if err != nil {
			return err
		}

		if oldSecretRef != "" {
			if err := uc.repo.DeleteSecret(ctx, tx, tenantID, oldSecretRef); err != nil {
				return err
			}
		}

		saved = cred
		return nil
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// Get returns the caller's own credential for TJSP eproc — never the password,
// never the credential_ref (the handler's view type omits both;
// ErrPortalCredentialNotFound surfaces as a typed 404 when none is configured).
func (uc *UseCase) Get(ctx context.Context, tenantID, appUserID string) (*PortalCredential, error) {
	var cred *PortalCredential
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		c, err := uc.repo.GetPortalCredential(ctx, tx, tenantID, appUserID, PortalTJSPEproc)
		if err != nil {
			return err
		}
		cred = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cred, nil
}

// Delete removes the caller's own credential and its underlying sealed secret,
// in one transaction — no secret material outlives the credential that
// referenced it. A caller with no credential configured gets
// ErrPortalCredentialNotFound (not persisted, but visible), so DELETE answers a
// typed 404 instead of a silent no-op 204.
func (uc *UseCase) Delete(ctx context.Context, tenantID, appUserID string) error {
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		existing, err := uc.repo.GetPortalCredential(ctx, tx, tenantID, appUserID, PortalTJSPEproc)
		if err != nil {
			return err
		}

		if err := uc.repo.DeletePortalCredential(ctx, tx, tenantID, appUserID, PortalTJSPEproc); err != nil {
			return err
		}

		return uc.repo.DeleteSecret(ctx, tx, tenantID, existing.CredentialRef)
	})
}
