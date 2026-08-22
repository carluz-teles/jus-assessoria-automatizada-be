package certificate

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

// UseCase carries the certificate use cases: storing an A1 certificate securely
// (parse → validate → envelope-encrypt → persist + outbox, all in one tx), listing
// a tenant's certificates (metadata only), and revoking one. It depends only on the
// Repository, the SecretVault, the outbox publisher and the UnitOfWork interfaces —
// never a concrete implementation.
//
// SECURITY: the vault wraps the per-record DEK; the plaintext .pfx password reaches
// only Create's parse step and is dropped there — it is never stored on the use
// case, never logged, never passed to the repo.
type UseCase struct {
	repo   Repository
	vault  vault.SecretVault
	outbox publisher
	uow    database.UnitOfWork
	now    func() time.Time
}

// Option configures a UseCase at construction (clock seam for deterministic tests).
type Option func(*UseCase)

// WithClock overrides the reference clock used for the expiry check and the
// revoked_at stamp. Production leaves the default (time.Now).
func WithClock(now func() time.Time) Option {
	return func(uc *UseCase) { uc.now = now }
}

// NewUseCase wires the use case to its repository, vault, outbox publisher and unit
// of work. opts is variadic so the binary's positional call stays source-compatible.
func NewUseCase(repo Repository, v vault.SecretVault, outbox publisher, uow database.UnitOfWork, opts ...Option) *UseCase {
	uc := &UseCase{repo: repo, vault: v, outbox: outbox, uow: uow, now: time.Now}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// CreateCommand is the POST /v1/certificates input the handler builds from the
// multipart request + the verified principal. TenantID/OwnerUserID come from the
// principal, NEVER the body. PFXData is the raw uploaded bytes; Password is used
// only to parse and is discarded after Create returns (the struct is not retained).
type CreateCommand struct {
	TenantID    string
	OwnerUserID string
	PFXData     []byte
	Password    string
}

// Create stores a certificate securely (docs: the A1 milestone). Steps:
//  1. parse + validate the .pfx with the password (wrong password / expired /
//     malformed → typed KindInvalid); the private key is decoded only to prove the
//     password and is dropped — never stored.
//  2. envelope-encrypt the RAW .pfx bytes (fresh DEK, AES-256-GCM, DEK wrapped by
//     the vault).
//  3. in ONE tenant-scoped tx: persist the metadata + envelope and publish
//     certificate.added — the row and the event commit together (transactional
//     outbox).
//
// The password is NEVER persisted or logged; it lives only in the CreateCommand the
// handler discards after this call. Returns the created certificate as a view
// (metadata only).
func (uc *UseCase) Create(ctx context.Context, cmd CreateCommand) (CertificateView, error) {
	meta, err := parsePFX(cmd.PFXData, cmd.Password, uc.now())
	if err != nil {
		return CertificateView{}, err
	}

	env, err := seal(ctx, uc.vault, cmd.PFXData)
	if err != nil {
		return CertificateView{}, err
	}

	var created *Certificate
	err = uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		saved, err := uc.repo.Insert(ctx, tx, InsertParams{
			TenantID:    cmd.TenantID,
			OwnerUserID: cmd.OwnerUserID,
			Meta:        meta,
			Envelope:    env,
		})
		if err != nil {
			return err
		}
		created = saved
		return uc.outbox.Publish(ctx, tx, newCertificateAdded(saved))
	})
	if err != nil {
		return CertificateView{}, err
	}

	return viewFromEntity(created), nil
}

// PreviewCommand is the POST /v1/certificates/preview input. It carries only the
// bytes + password — no tenant/owner, because preview stores nothing. The password
// is used only to parse and is discarded when this call returns.
type PreviewCommand struct {
	PFXData  []byte
	Password string
}

// Preview parses + validates a .pfx WITHOUT storing anything (the wizard's
// "Validação" step). It decodes the file (wrong password / malformed → typed 4xx),
// then REPORTS the metadata + the BE-owned checks — an expired cert still previews
// successfully with nao_expirado=false, so the FE can warn rather than block. This
// is idempotent and read-only: no tx, no outbox, no persistence. The private key
// and the password never leave this call.
func (uc *UseCase) Preview(_ context.Context, cmd PreviewCommand) (PreviewResult, error) {
	meta, hasChain, err := decodePFX(cmd.PFXData, cmd.Password)
	if err != nil {
		return PreviewResult{}, err
	}

	now := uc.now()
	return PreviewResult{
		SubjectCN:   meta.SubjectCN,
		OAB:         meta.OAB,
		Issuer:      meta.Issuer,
		Serial:      meta.Serial,
		NotBefore:   meta.NotBefore,
		NotAfter:    meta.NotAfter,
		Fingerprint: meta.Fingerprint,
		Checks: PreviewChecks{
			NaoExpirado:    !now.After(meta.NotAfter) && !now.Before(meta.NotBefore),
			CadeiaOk:       hasChain,
			TitularConfere: meta.SubjectCN != "",
		},
	}, nil
}

// List returns a tenant's certificates (metadata only), newest first. It runs in a
// tenant-scoped tx so RLS applies to the read (barrier 2) on top of the explicit
// tenant filter (barrier 1). An empty tenant yields an empty slice, never an error.
func (uc *UseCase) List(ctx context.Context, tenantID string) ([]CertificateView, error) {
	var views []CertificateView
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		v, err := uc.repo.List(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		views = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return views, nil
}

// Revoke soft-revokes a certificate (DELETE /v1/certificates/:id): in ONE
// tenant-scoped tx it stamps revoked_at (a missing/foreign/already-revoked id →
// ErrCertificateNotFound → 404) and publishes certificate.revoked — the flip and
// the event commit together. tenant_id comes from the principal, the id from the
// path.
func (uc *UseCase) Revoke(ctx context.Context, tenantID, id string) error {
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		if err := uc.repo.Revoke(ctx, tx, tenantID, id, uc.now()); err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newCertificateRevoked(tenantID, id))
	})
}
