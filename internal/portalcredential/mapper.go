package portalcredential

import (
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/portalcredential/portalcredentialdb"
	"github.com/jusassessoria/platform/lib/vault"
)

// mapper.go is the boundary where driver types die: uuid.UUID, pgtype.* die
// here so the entity stays pure. The repository returns *PortalCredential,
// never the sqlc row — CLAUDE.md's convention every slice follows.

// portalCredentialToEntity decodes one sqlc row into a PortalCredential. Nullable
// driver types (LastError, LastVerifiedAt, ConfiguredBy) collapse to the
// entity's zero values when absent — a never-verified credential has a zero
// LastVerifiedAt, exactly like a never-run SyncRun's FinishedAt elsewhere in the
// repo (internal/acquisition/mapper.go).
func portalCredentialToEntity(r portalcredentialdb.PortalCredential) *PortalCredential {
	ent := &PortalCredential{
		ID:            r.ID.String(),
		TenantID:      r.TenantID.String(),
		AppUserID:     r.AppUserID.String(),
		Portal:        r.Portal,
		Login:         r.Login,
		CredentialRef: r.CredentialRef.String(),
		Status:        r.Status,
	}
	if r.LastError != nil {
		ent.LastError = *r.LastError
	}
	if r.LastVerifiedAt.Valid {
		ent.LastVerifiedAt = r.LastVerifiedAt.Time
	}
	if r.ConfiguredBy.Valid {
		ent.ConfiguredBy = uuid.UUID(r.ConfiguredBy.Bytes).String()
	}
	if r.CreatedAt.Valid {
		ent.CreatedAt = r.CreatedAt.Time
	}
	if r.UpdatedAt.Valid {
		ent.UpdatedAt = r.UpdatedAt.Time
	}
	return ent
}

// sealedToTenantSecretParams maps a lib/vault.Sealed into the sqlc insert params
// for tenant_secret — the only place Sealed's byte slices cross into SQL.
func sealedToTenantSecretParams(tenantID string, sealed vault.Sealed) (portalcredentialdb.InsertTenantSecretParams, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return portalcredentialdb.InsertTenantSecretParams{}, err
	}
	return portalcredentialdb.InsertTenantSecretParams{
		TenantID:   tid,
		Ciphertext: sealed.Ciphertext,
		Nonce:      sealed.Nonce,
		WrappedDek: sealed.DEKCiphertext,
		DekNonce:   sealed.DEKNonce,
	}, nil
}

// tenantSecretToSealed reverses the mapping above for lib/vault.Open.
func tenantSecretToSealed(r portalcredentialdb.TenantSecret) vault.Sealed {
	return vault.Sealed{
		Ciphertext:    r.Ciphertext,
		Nonce:         r.Nonce,
		DEKCiphertext: r.WrappedDek,
		DEKNonce:      r.DekNonce,
	}
}
