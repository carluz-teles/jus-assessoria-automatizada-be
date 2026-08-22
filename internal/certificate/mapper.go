package certificate

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/certificate/certificatedb"
)

// mapper.go is the boundary where driver types die: uuid.UUID and pgtype.* are
// absorbed here so the entity/read models stay pure. The repo returns *Certificate
// / CertificateView, never the sqlc row. Envelope columns are NOT mapped back onto
// any read model — only InsertCertificate carries them, and only inbound.

// insertRowToEntity maps the INSERT RETURNING row (metadata only) to the entity.
// The envelope fields are left zero: they were the INPUT, not something the row
// returns, and no caller reads them back on the create path.
func insertRowToEntity(r certificatedb.InsertCertificateRow) *Certificate {
	return &Certificate{
		ID:          r.ID.String(),
		TenantID:    r.TenantID.String(),
		OwnerUserID: r.OwnerUserID.String(),
		SubjectCN:   r.SubjectCn,
		OAB:         derefString(r.Oab),
		Issuer:      r.Issuer,
		Serial:      r.Serial,
		NotBefore:   r.NotBefore.Time,
		NotAfter:    r.NotAfter.Time,
		Fingerprint: r.Fingerprint,
		CreatedAt:   r.CreatedAt.Time,
		RevokedAt:   timestamptzToPtr(r.RevokedAt),
	}
}

// getRowToEntity maps the GET row (metadata only) to the entity for the revoke
// path's existence check.
func getRowToEntity(r certificatedb.GetCertificateRow) *Certificate {
	return &Certificate{
		ID:          r.ID.String(),
		TenantID:    r.TenantID.String(),
		OwnerUserID: r.OwnerUserID.String(),
		SubjectCN:   r.SubjectCn,
		OAB:         derefString(r.Oab),
		Issuer:      r.Issuer,
		Serial:      r.Serial,
		NotBefore:   r.NotBefore.Time,
		NotAfter:    r.NotAfter.Time,
		Fingerprint: r.Fingerprint,
		CreatedAt:   r.CreatedAt.Time,
		RevokedAt:   timestamptzToPtr(r.RevokedAt),
	}
}

// envelopeRowToSignable maps the GetCertificateEnvelope row (the ONLY row that
// carries encrypted key material) to the internal signable. The envelope bytes
// stay inside the backend — this type never reaches a read model.
func envelopeRowToSignable(r certificatedb.GetCertificateEnvelopeRow) signable {
	return signable{
		OwnerUserID: r.OwnerUserID.String(),
		NotAfter:    r.NotAfter.Time,
		RevokedAt:   timestamptzToPtr(r.RevokedAt),
		Envelope: envelope{
			Ciphertext: r.Ciphertext,
			Nonce:      r.Nonce,
			WrappedDEK: r.WrappedDek,
			KEKRef:     r.KekRef,
		},
	}
}

// listRowToView maps a ListCertificates row (metadata + joined owner name) to the
// read model. This is the ONLY place owner_user_name is threaded in — the LEFT
// JOIN yields nil when the app_user row is gone, so OwnerUserName stays nil.
func listRowToView(r certificatedb.ListCertificatesRow) CertificateView {
	return CertificateView{
		ID:            r.ID.String(),
		SubjectCN:     r.SubjectCn,
		OAB:           derefString(r.Oab),
		Issuer:        r.Issuer,
		Serial:        r.Serial,
		NotBefore:     r.NotBefore.Time,
		NotAfter:      r.NotAfter.Time,
		Fingerprint:   r.Fingerprint,
		OwnerUserID:   r.OwnerUserID.String(),
		OwnerUserName: r.OwnerUserName,
		CreatedAt:     r.CreatedAt.Time,
		RevokedAt:     timestamptzToPtr(r.RevokedAt),
	}
}

// viewFromEntity renders the created entity (INSERT result) as the read model, so
// the POST response and the GET list entries are the same JSON shape. The owner
// name is not known on the create path (no join), so it is nil there.
func viewFromEntity(c *Certificate) CertificateView {
	return CertificateView{
		ID:            c.ID,
		SubjectCN:     c.SubjectCN,
		OAB:           c.OAB,
		Issuer:        c.Issuer,
		Serial:        c.Serial,
		NotBefore:     c.NotBefore,
		NotAfter:      c.NotAfter,
		Fingerprint:   c.Fingerprint,
		OwnerUserID:   c.OwnerUserID,
		OwnerUserName: nil,
		CreatedAt:     c.CreatedAt,
		RevokedAt:     c.RevokedAt,
	}
}

// timestamptzToPtr collapses a nullable timestamptz to *time.Time (nil = SQL NULL).
func timestamptzToPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// timeToTimestamptz is the inverse for a required time (always valid).
func timeToTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// derefString collapses a nullable text column (*string) to a plain string.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// textToNull is the inverse: an empty string becomes SQL NULL (an absent OAB).
func textToNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
