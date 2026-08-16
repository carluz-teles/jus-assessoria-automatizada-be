package portalcredential

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/portalcredential/portalcredentialdb"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/vault"
)

// Repository is the persistence port the use case depends on (it never sees the
// concrete sqlc impl). Every method receives the caller's transaction — the use
// case owns the tx+RLS boundary (CLAUDE.md), the repo only participates — so a
// Configure's secret write, portal_credential upsert and old-secret cleanup
// commit atomically.
type Repository interface {
	// InsertSecret persists a newly sealed secret and returns its id — the value
	// that becomes the next portal_credential.credential_ref.
	InsertSecret(ctx context.Context, tx database.Tx, tenantID string, sealed vault.Sealed) (secretID string, err error)
	// GetSecret reads a sealed secret back by (tenant, id) for lib/vault.Open.
	// Never called across a different tenant's row — RLS would reject it anyway,
	// but the WHERE is the app-level barrier 1.
	GetSecret(ctx context.Context, tx database.Tx, tenantID, secretID string) (vault.Sealed, error)
	// DeleteSecret removes a sealed secret row — used to clean up the OLD secret
	// on a reconfigure, and the credential's own secret on Delete.
	DeleteSecret(ctx context.Context, tx database.Tx, tenantID, secretID string) error

	// UpsertPortalCredential writes (or overwrites) the caller's own credential
	// row for a portal.
	UpsertPortalCredential(ctx context.Context, tx database.Tx, params UpsertPortalCredentialParams) (*PortalCredential, error)
	// GetPortalCredential reads the caller's own credential for a portal.
	// Returns ErrPortalCredentialNotFound instead of (nil, nil) when absent.
	GetPortalCredential(ctx context.Context, tx database.Tx, tenantID, appUserID, portal string) (*PortalCredential, error)
	// DeletePortalCredential removes the caller's own credential row. A miss is
	// a no-op (idempotent delete), matched by the use case checking existence
	// first via GetPortalCredential when it needs the credential_ref to clean up.
	DeletePortalCredential(ctx context.Context, tx database.Tx, tenantID, appUserID, portal string) error
}

// UpsertPortalCredentialParams carries the write shape UpsertPortalCredential
// needs — a plain struct instead of the sqlc-generated one so the use case
// never imports portalcredentialdb (the mapper is the only place that does).
type UpsertPortalCredentialParams struct {
	TenantID       string
	AppUserID      string
	Portal         string
	Login          string
	CredentialRef  string
	Status         string
	LastError      string    // empty means SQL NULL
	LastVerifiedAt time.Time // zero means SQL NULL (never verified)
	ConfiguredBy   string
}

type repository struct{}

var _ Repository = (*repository)(nil)

// NewRepository builds the sqlc-backed Repository. It holds no state — every
// method binds portalcredentialdb.Queries to the caller's tx.
func NewRepository() Repository {
	return &repository{}
}

func (r *repository) InsertSecret(ctx context.Context, tx database.Tx, tenantID string, sealed vault.Sealed) (string, error) {
	params, err := sealedToTenantSecretParams(tenantID, sealed)
	if err != nil {
		return "", apperr.NewInvalid("tenant id inválido")
	}

	row, err := portalcredentialdb.New(tx).InsertTenantSecret(ctx, params)
	if err != nil {
		return "", apperr.NewInfra("erro ao gravar segredo no cofre", err)
	}
	return row.ID.String(), nil
}

func (r *repository) GetSecret(ctx context.Context, tx database.Tx, tenantID, secretID string) (vault.Sealed, error) {
	tid, sid, err := parseTenantAndID(tenantID, secretID)
	if err != nil {
		return vault.Sealed{}, apperr.NewInvalid("identificador inválido")
	}

	row, err := portalcredentialdb.New(tx).GetTenantSecret(ctx, portalcredentialdb.GetTenantSecretParams{
		TenantID: tid,
		ID:       sid,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return vault.Sealed{}, apperr.NewNotFound("segredo não encontrado")
		}
		return vault.Sealed{}, apperr.NewInfra("erro ao ler segredo do cofre", err)
	}
	return tenantSecretToSealed(row), nil
}

func (r *repository) DeleteSecret(ctx context.Context, tx database.Tx, tenantID, secretID string) error {
	tid, sid, err := parseTenantAndID(tenantID, secretID)
	if err != nil {
		return apperr.NewInvalid("identificador inválido")
	}

	if err := portalcredentialdb.New(tx).DeleteTenantSecret(ctx, portalcredentialdb.DeleteTenantSecretParams{
		TenantID: tid,
		ID:       sid,
	}); err != nil {
		return apperr.NewInfra("erro ao remover segredo do cofre", err)
	}
	return nil
}

func (r *repository) UpsertPortalCredential(ctx context.Context, tx database.Tx, params UpsertPortalCredentialParams) (*PortalCredential, error) {
	tenantID, err := uuid.Parse(params.TenantID)
	if err != nil {
		return nil, apperr.NewInvalid("tenant id inválido")
	}
	appUserID, err := uuid.Parse(params.AppUserID)
	if err != nil {
		return nil, apperr.NewInvalid("app user id inválido")
	}
	credentialRef, err := uuid.Parse(params.CredentialRef)
	if err != nil {
		return nil, apperr.NewInvalid("credential ref inválido")
	}

	sqlcParams := portalcredentialdb.UpsertPortalCredentialParams{
		TenantID:      tenantID,
		AppUserID:     appUserID,
		Portal:        params.Portal,
		Login:         params.Login,
		CredentialRef: credentialRef,
		Status:        params.Status,
	}
	if params.LastError != "" {
		sqlcParams.LastError = &params.LastError
	}
	if !params.LastVerifiedAt.IsZero() {
		sqlcParams.LastVerifiedAt = pgtype.Timestamptz{Time: params.LastVerifiedAt, Valid: true}
	}
	if params.ConfiguredBy != "" {
		configuredBy, err := uuid.Parse(params.ConfiguredBy)
		if err != nil {
			return nil, apperr.NewInvalid("configured_by inválido")
		}
		sqlcParams.ConfiguredBy = pgtype.UUID{Bytes: configuredBy, Valid: true}
	}

	row, err := portalcredentialdb.New(tx).UpsertPortalCredential(ctx, sqlcParams)
	if err != nil {
		return nil, apperr.NewInfra("erro ao gravar credencial de portal", err)
	}
	return portalCredentialToEntity(row), nil
}

func (r *repository) GetPortalCredential(ctx context.Context, tx database.Tx, tenantID, appUserID, portal string) (*PortalCredential, error) {
	tid, uid, err := parseTenantAndID(tenantID, appUserID)
	if err != nil {
		return nil, apperr.NewInvalid("identificador inválido")
	}

	row, err := portalcredentialdb.New(tx).GetPortalCredential(ctx, portalcredentialdb.GetPortalCredentialParams{
		TenantID:  tid,
		AppUserID: uid,
		Portal:    portal,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPortalCredentialNotFound
		}
		return nil, apperr.NewInfra("erro ao ler credencial de portal", err)
	}
	return portalCredentialToEntity(row), nil
}

func (r *repository) DeletePortalCredential(ctx context.Context, tx database.Tx, tenantID, appUserID, portal string) error {
	tid, uid, err := parseTenantAndID(tenantID, appUserID)
	if err != nil {
		return apperr.NewInvalid("identificador inválido")
	}

	if err := portalcredentialdb.New(tx).DeletePortalCredential(ctx, portalcredentialdb.DeletePortalCredentialParams{
		TenantID:  tid,
		AppUserID: uid,
		Portal:    portal,
	}); err != nil {
		return apperr.NewInfra("erro ao remover credencial de portal", err)
	}
	return nil
}

// parseTenantAndID parses two uuid strings together — the common shape of
// every (tenant, X) lookup in this repository.
func parseTenantAndID(tenantID, id string) (tid, xid uuid.UUID, err error) {
	tid, err = uuid.Parse(tenantID)
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, err
	}
	xid, err = uuid.Parse(id)
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, err
	}
	return tid, xid, nil
}
