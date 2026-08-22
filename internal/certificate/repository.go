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

// signable is a certificate loaded for the signing path: its envelope (encrypted
// key material) plus the fields needed to gate the sign (owner, expiry, revoked).
// It NEVER crosses the API boundary — only signDigest consumes the envelope.
type signable struct {
	OwnerUserID string
	NotAfter    time.Time
	RevokedAt   *time.Time
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
	// LoadEnvelope loads one certificate's envelope + sign-gating fields, scoped to
	// tenant. A missing/foreign id is ErrCertificateNotFound (never nil, nil). This
	// is the ONLY repo method that reads the encrypted key material back out.
	LoadEnvelope(ctx context.Context, tx database.Tx, tenantID, id string) (signable, error)
	// RecordSigning appends a signing_event audit row (same tx as the sign). It
	// stores the digest only — never the signature, key, or password.
	RecordSigning(ctx context.Context, tx database.Tx, tenantID, certificateID, signerUserID string, digest []byte) error
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

// LoadEnvelope reads the certificate's envelope + gating fields in-tx (so RLS
// applies). A no-row / malformed-id result is the typed ErrCertificateNotFound.
func (r *pgRepository) LoadEnvelope(ctx context.Context, tx database.Tx, tenantID, id string) (signable, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return signable{}, database.WrapInfra(err)
	}
	cid, err := uuid.Parse(id)
	if err != nil {
		// A malformed id can never match a real row; treat as not found.
		return signable{}, ErrCertificateNotFound
	}

	row, err := certificatedb.New(tx).GetCertificateEnvelope(ctx, certificatedb.GetCertificateEnvelopeParams{
		ID:       cid,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return signable{}, ErrCertificateNotFound
	}
	if err != nil {
		return signable{}, database.WrapInfra(err)
	}
	return envelopeRowToSignable(row), nil
}

// RecordSigning writes the audit row in-tx. RETURNING always yields a row, so any
// error here is infra.
func (r *pgRepository) RecordSigning(ctx context.Context, tx database.Tx, tenantID, certificateID, signerUserID string, digest []byte) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	cid, err := uuid.Parse(certificateID)
	if err != nil {
		return database.WrapInfra(err)
	}
	sid, err := uuid.Parse(signerUserID)
	if err != nil {
		return database.WrapInfra(err)
	}

	_, err = certificatedb.New(tx).InsertSigningEvent(ctx, certificatedb.InsertSigningEventParams{
		TenantID:      tid,
		CertificateID: cid,
		SignerUserID:  sid,
		DigestSha256:  digest,
	})
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
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
