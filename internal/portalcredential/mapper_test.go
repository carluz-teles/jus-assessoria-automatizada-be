package portalcredential

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/portalcredential/portalcredentialdb"
	"github.com/jusassessoria/platform/lib/vault"
)

func TestPortalCredentialToEntity_MapsRequiredFields(t *testing.T) {
	t.Parallel()

	id, tenantID, appUserID, credRef := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	row := portalcredentialdb.PortalCredential{
		ID:            id,
		TenantID:      tenantID,
		AppUserID:     appUserID,
		Portal:        PortalTJSPEproc,
		Login:         "advogado",
		CredentialRef: credRef,
		Status:        StatusActive,
	}

	ent := portalCredentialToEntity(row)

	if ent.ID != id.String() || ent.TenantID != tenantID.String() || ent.AppUserID != appUserID.String() {
		t.Errorf("id/tenant/user did not round-trip: %+v", ent)
	}
	if ent.CredentialRef != credRef.String() {
		t.Errorf("CredentialRef = %q, want %q", ent.CredentialRef, credRef.String())
	}
	if ent.Portal != PortalTJSPEproc || ent.Login != "advogado" || ent.Status != StatusActive {
		t.Errorf("portal/login/status did not round-trip: %+v", ent)
	}
}

func TestPortalCredentialToEntity_CollapsesNullableFieldsToZeroValues(t *testing.T) {
	t.Parallel()

	row := portalcredentialdb.PortalCredential{
		ID:            uuid.New(),
		TenantID:      uuid.New(),
		AppUserID:     uuid.New(),
		CredentialRef: uuid.New(),
		// LastError nil, LastVerifiedAt/ConfiguredBy/CreatedAt/UpdatedAt invalid —
		// a never-verified, never-audited credential (the shape UpsertPortalCredential
		// produces on first insert before any ConfiguredBy is set, defensively).
	}

	ent := portalCredentialToEntity(row)

	if ent.LastError != "" {
		t.Errorf("LastError = %q, want empty", ent.LastError)
	}
	if !ent.LastVerifiedAt.IsZero() {
		t.Errorf("LastVerifiedAt = %v, want zero", ent.LastVerifiedAt)
	}
	if ent.ConfiguredBy != "" {
		t.Errorf("ConfiguredBy = %q, want empty", ent.ConfiguredBy)
	}
}

func TestPortalCredentialToEntity_PopulatesOptionalFieldsWhenPresent(t *testing.T) {
	t.Parallel()

	configuredBy := uuid.New()
	lastErr := "usuário ou senha inválidos, segundo o portal"
	now := time.Now().UTC().Truncate(time.Second)

	row := portalcredentialdb.PortalCredential{
		ID:             uuid.New(),
		TenantID:       uuid.New(),
		AppUserID:      uuid.New(),
		CredentialRef:  uuid.New(),
		LastError:      &lastErr,
		LastVerifiedAt: pgtype.Timestamptz{Time: now, Valid: true},
		ConfiguredBy:   pgtype.UUID{Bytes: configuredBy, Valid: true},
		CreatedAt:      pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt:      pgtype.Timestamptz{Time: now, Valid: true},
	}

	ent := portalCredentialToEntity(row)

	if ent.LastError != lastErr {
		t.Errorf("LastError = %q, want %q", ent.LastError, lastErr)
	}
	if !ent.LastVerifiedAt.Equal(now) {
		t.Errorf("LastVerifiedAt = %v, want %v", ent.LastVerifiedAt, now)
	}
	if ent.ConfiguredBy != configuredBy.String() {
		t.Errorf("ConfiguredBy = %q, want %q", ent.ConfiguredBy, configuredBy.String())
	}
}

func TestSealedToTenantSecretParams_RoundTripsSealedBytes(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	sealed := vault.Sealed{
		Ciphertext:    []byte("ciphertext"),
		Nonce:         []byte("nonce"),
		DEKCiphertext: []byte("wrapped-dek"),
		DEKNonce:      []byte("dek-nonce"),
	}

	params, err := sealedToTenantSecretParams(tenantID.String(), sealed)
	if err != nil {
		t.Fatalf("sealedToTenantSecretParams() error = %v", err)
	}

	if params.TenantID != tenantID {
		t.Errorf("TenantID = %v, want %v", params.TenantID, tenantID)
	}
	if string(params.Ciphertext) != "ciphertext" || string(params.Nonce) != "nonce" ||
		string(params.WrappedDek) != "wrapped-dek" || string(params.DekNonce) != "dek-nonce" {
		t.Errorf("Sealed bytes did not round-trip into InsertTenantSecretParams: %+v", params)
	}
}

func TestSealedToTenantSecretParams_RejectsInvalidTenantID(t *testing.T) {
	t.Parallel()

	if _, err := sealedToTenantSecretParams("not-a-uuid", vault.Sealed{}); err == nil {
		t.Error("sealedToTenantSecretParams() error = nil, want an error for a malformed tenant id")
	}
}

func TestTenantSecretToSealed_RoundTripsWithSealedToTenantSecretParams(t *testing.T) {
	t.Parallel()

	original := vault.Sealed{
		Ciphertext:    []byte("ct"),
		Nonce:         []byte("n"),
		DEKCiphertext: []byte("wd"),
		DEKNonce:      []byte("dn"),
	}

	row := portalcredentialdb.TenantSecret{
		Ciphertext: original.Ciphertext,
		Nonce:      original.Nonce,
		WrappedDek: original.DEKCiphertext,
		DekNonce:   original.DEKNonce,
	}

	got := tenantSecretToSealed(row)
	if string(got.Ciphertext) != string(original.Ciphertext) ||
		string(got.Nonce) != string(original.Nonce) ||
		string(got.DEKCiphertext) != string(original.DEKCiphertext) ||
		string(got.DEKNonce) != string(original.DEKNonce) {
		t.Errorf("tenantSecretToSealed() = %+v, want it to mirror the row's byte columns", got)
	}
}
