package certificate

import "github.com/jusassessoria/platform/lib/events"

// Domain facts this slice publishes on the transactional outbox. They carry ONLY
// metadata (ids + fingerprint) — never key material or the password. Downstream
// slices (a future server-side-signing fatia, an audit log) consume them; nothing
// consumes them today, but emitting them in the create/revoke tx keeps the audit
// trail atomic with the write (transactional outbox convention).

const (
	aggregateType        = "certificate"
	typeCertificateAdded = "certificate.added"
	typeCertRevoked      = "certificate.revoked"
)

// certificateAdded is emitted when a certificate is stored. Fingerprint uniquely
// identifies the cert without exposing the key.
type certificateAdded struct {
	events.Base
	TenantID    string `json:"tenant_id"`
	OwnerUserID string `json:"owner_user_id"`
	Fingerprint string `json:"fingerprint"`
}

func newCertificateAdded(c *Certificate) certificateAdded {
	return certificateAdded{
		Base:        events.Base{EventID: "certificate.added:" + c.ID, Aggregate: c.ID},
		TenantID:    c.TenantID,
		OwnerUserID: c.OwnerUserID,
		Fingerprint: c.Fingerprint,
	}
}

func (certificateAdded) AggregateType() string { return aggregateType }
func (certificateAdded) Type() string          { return typeCertificateAdded }

// certificateRevoked is emitted when a certificate is revoked.
type certificateRevoked struct {
	events.Base
	TenantID string `json:"tenant_id"`
}

func newCertificateRevoked(tenantID, id string) certificateRevoked {
	return certificateRevoked{
		Base:     events.Base{EventID: "certificate.revoked:" + id, Aggregate: id},
		TenantID: tenantID,
	}
}

func (certificateRevoked) AggregateType() string { return aggregateType }
func (certificateRevoked) Type() string          { return typeCertRevoked }
