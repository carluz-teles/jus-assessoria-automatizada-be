package certificate

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/certificate/certificatedb"
	"github.com/jusassessoria/platform/lib/database"
)

// InsertParams is the write DTO for a new certificate — the parsed metadata plus
// the envelope. It is the domain's own shape (plain Go types); the repo translates
// it to the sqlc params at the boundary so the use case never touches uuid.UUID or
// pgtype.*. tenant_id + owner_user_id come from the trusted principal.
type InsertParams struct {
	TenantID    string
	OwnerUserID string
	Meta        parsedCertificate
	Envelope    envelope
}

// Repository is the persistence port the use case depends on (never the concrete
// impl). Every method receives the caller's transaction — the use case owns the
// boundary, the repo participates — so RLS (barrier 2) scopes every statement to
// the principal's tenant on top of the explicit tenant_id filter (barrier 1). All
// reads here run in-tx precisely so RLS applies; there is no pool-backed read.
type Repository interface {
	// Insert persists a parsed + envelope-encrypted certificate. RETURNING always
	// yields a row (no not-found branch).
	Insert(ctx context.Context, tx database.Tx, p InsertParams) (*Certificate, error)
	// List returns a tenant's certificates (metadata only) newest first, with the
	// owner's display name joined. Never carries envelope columns.
	List(ctx context.Context, tx database.Tx, tenantID string) ([]CertificateView, error)
	// Revoke soft-revokes an active certificate by id, scoped to tenant. A missing,
	// foreign, or already-revoked id is ErrCertificateNotFound (never nil, nil).
	Revoke(ctx context.Context, tx database.Tx, tenantID, id string, revokedAt time.Time) error
}

// pgRepository is the sqlc-backed implementation. It holds no pool: every method
// binds the generated queries to the passed transaction (the deadline/document
// write pattern).
type pgRepository struct{}

var _ Repository = (*pgRepository)(nil)

// NewRepository returns the tx-bound repository.
func NewRepository() Repository { return &pgRepository{} }

// Insert persists the certificate inside the caller's tx.
func (r *pgRepository) Insert(ctx context.Context, tx database.Tx, p InsertParams) (*Certificate, error) {
	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	ownerID, err := uuid.Parse(p.OwnerUserID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	row, err := certificatedb.New(tx).InsertCertificate(ctx, certificatedb.InsertCertificateParams{
		TenantID:    tenantID,
		OwnerUserID: ownerID,
		SubjectCn:   p.Meta.SubjectCN,
		Oab:         textToNull(p.Meta.OAB),
		Issuer:      p.Meta.Issuer,
		Serial:      p.Meta.Serial,
		NotBefore:   timeToTimestamptz(p.Meta.NotBefore),
		NotAfter:    timeToTimestamptz(p.Meta.NotAfter),
		Fingerprint: p.Meta.Fingerprint,
		Ciphertext:  p.Envelope.Ciphertext,
		Nonce:       p.Envelope.Nonce,
		WrappedDek:  p.Envelope.WrappedDEK,
		KekRef:      p.Envelope.KEKRef,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return insertRowToEntity(row), nil
}

// List reads the tenant's certificates in-tx (so RLS applies). An empty result is
// an empty slice, not an error.
func (r *pgRepository) List(ctx context.Context, tx database.Tx, tenantID string) ([]CertificateView, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	rows, err := certificatedb.New(tx).ListCertificates(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	views := make([]CertificateView, 0, len(rows))
	for _, row := range rows {
		views = append(views, listRowToView(row))
	}
	return views, nil
}

// Revoke soft-revokes the certificate in-tx. A no-row RETURNING result (missing,
// foreign, or already revoked) is the typed ErrCertificateNotFound, never nil, nil.
func (r *pgRepository) Revoke(ctx context.Context, tx database.Tx, tenantID, id string, revokedAt time.Time) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	cid, err := uuid.Parse(id)
	if err != nil {
		// A malformed id can never match a real row; treat as not found.
		return ErrCertificateNotFound
	}

	_, err = certificatedb.New(tx).RevokeCertificate(ctx, certificatedb.RevokeCertificateParams{
		ID:        cid,
		TenantID:  tid,
		RevokedAt: timeToTimestamptz(revokedAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCertificateNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}
