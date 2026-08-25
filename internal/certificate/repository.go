package certificate

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/certificate/certificatedb"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// Repository é a porta que o caso de uso enxerga. Implementação em pgRepository
// abaixo. Todo método é tenant-scoped (barrier 1) e roda dentro da tx do caller
// (UoW no domain), com RLS como barrier 2.
type Repository interface {
	Insert(ctx context.Context, tx database.Tx, c *Certificate) (id string, createdAt time.Time, err error)
	// GetByID inclui o envelope (uso interno pelo Sign).
	GetByID(ctx context.Context, tx database.Tx, tenantID, id string) (*Certificate, error)
	ListActive(ctx context.Context, tx database.Tx, tenantID string) ([]CertificateWithOwner, error)
	Revoke(ctx context.Context, tx database.Tx, tenantID, id string) error
	// RecordSigning appends an audit row (signing_event) for a server-side signature —
	// digest only, never the signature or key material.
	RecordSigning(ctx context.Context, tx database.Tx, tenantID, certificateID, signerUserID string, digest []byte) error
}

// CertificateWithOwner é o shape do read model da lista — inclui o nome do
// owner (joined em app_user). O envelope NÃO viaja aqui (a lista não precisa).
type CertificateWithOwner struct {
	Certificate
	OwnerUserName string // "" quando o join não achou
}

// NewRepository devolve a implementação pgx do Repository.
func NewRepository() Repository { return &pgRepository{} }

type pgRepository struct{}

func (r *pgRepository) Insert(ctx context.Context, tx database.Tx, c *Certificate) (string, time.Time, error) {
	tid, err := uuid.Parse(c.TenantID)
	if err != nil {
		return "", time.Time{}, apperr.NewInvalid("tenant id inválido")
	}
	uid, err := uuid.Parse(c.OwnerUserID)
	if err != nil {
		return "", time.Time{}, apperr.NewInvalid("owner id inválido")
	}
	row, err := certificatedb.New(tx).InsertCertificate(ctx, certificatedb.InsertCertificateParams{
		TenantID:    tid,
		OwnerUserID: uid,
		SubjectCn:   c.SubjectCN,
		Oab:         nullString(c.OAB),
		Issuer:      c.Issuer,
		Serial:      c.Serial,
		NotBefore:   pgtype.Timestamptz{Time: c.NotBefore, Valid: true},
		NotAfter:    pgtype.Timestamptz{Time: c.NotAfter, Valid: true},
		Fingerprint: c.Fingerprint,
		Ciphertext:  c.Envelope.Ciphertext,
		Nonce:       c.Envelope.Nonce,
		WrappedDek:  c.Envelope.WrappedDEK,
		KekRef:      c.Envelope.KEKRef,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return "", time.Time{}, ErrCertificateAlreadyExists
		}
		return "", time.Time{}, database.WrapInfra(err)
	}
	return row.ID.String(), row.CreatedAt.Time, nil
}

func (r *pgRepository) GetByID(ctx context.Context, tx database.Tx, tenantID, id string) (*Certificate, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, apperr.NewInvalid("tenant id inválido")
	}
	cid, err := uuid.Parse(id)
	if err != nil {
		return nil, apperr.NewInvalid("id do certificado inválido")
	}
	row, err := certificatedb.New(tx).GetCertificateByID(ctx, certificatedb.GetCertificateByIDParams{
		ID:       cid,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCertificateNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return &Certificate{
		ID:          row.ID.String(),
		TenantID:    row.TenantID.String(),
		OwnerUserID: row.OwnerUserID.String(),
		SubjectCN:   row.SubjectCn,
		OAB:         derefString(row.Oab),
		Issuer:      row.Issuer,
		Serial:      row.Serial,
		NotBefore:   row.NotBefore.Time.UTC(),
		NotAfter:    row.NotAfter.Time.UTC(),
		Fingerprint: row.Fingerprint,
		Envelope: Envelope{
			Ciphertext: row.Ciphertext,
			Nonce:      row.Nonce,
			WrappedDEK: row.WrappedDek,
			KEKRef:     row.KekRef,
		},
		CreatedAt: row.CreatedAt.Time.UTC(),
		RevokedAt: timePtr(row.RevokedAt),
	}, nil
}

func (r *pgRepository) ListActive(ctx context.Context, tx database.Tx, tenantID string) ([]CertificateWithOwner, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, apperr.NewInvalid("tenant id inválido")
	}
	rows, err := certificatedb.New(tx).ListCertificatesByTenant(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]CertificateWithOwner, 0, len(rows))
	for _, row := range rows {
		out = append(out, CertificateWithOwner{
			Certificate: Certificate{
				ID:          row.ID.String(),
				TenantID:    row.TenantID.String(),
				OwnerUserID: row.OwnerUserID.String(),
				SubjectCN:   row.SubjectCn,
				OAB:         derefString(row.Oab),
				Issuer:      row.Issuer,
				Serial:      row.Serial,
				NotBefore:   row.NotBefore.Time.UTC(),
				NotAfter:    row.NotAfter.Time.UTC(),
				Fingerprint: row.Fingerprint,
				CreatedAt:   row.CreatedAt.Time.UTC(),
				RevokedAt:   timePtr(row.RevokedAt),
				// Envelope não vem no read model da lista (não é necessário).
			},
			OwnerUserName: derefString(row.OwnerUserName),
		})
	}
	return out, nil
}

func (r *pgRepository) Revoke(ctx context.Context, tx database.Tx, tenantID, id string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return apperr.NewInvalid("tenant id inválido")
	}
	cid, err := uuid.Parse(id)
	if err != nil {
		return apperr.NewInvalid("id do certificado inválido")
	}
	_, err = certificatedb.New(tx).RevokeCertificate(ctx, certificatedb.RevokeCertificateParams{
		ID:        cid,
		TenantID:  tid,
		RevokedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCertificateNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) RecordSigning(ctx context.Context, tx database.Tx, tenantID, certificateID, signerUserID string, digest []byte) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return apperr.NewInvalid("tenant id inválido")
	}
	cid, err := uuid.Parse(certificateID)
	if err != nil {
		return apperr.NewInvalid("id do certificado inválido")
	}
	uid, err := uuid.Parse(signerUserID)
	if err != nil {
		return apperr.NewInvalid("signer id inválido")
	}
	_, err = certificatedb.New(tx).InsertSigningEvent(ctx, certificatedb.InsertSigningEventParams{
		TenantID:      tid,
		CertificateID: cid,
		SignerUserID:  uid,
		DigestSha256:  digest,
	})
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// ── pequenos helpers ────────────────────────────────────────────────────────

func nullString(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}
