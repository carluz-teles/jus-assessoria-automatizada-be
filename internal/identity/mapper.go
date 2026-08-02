package identity

import (
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/identity/identitydb"
	"github.com/jusassessoria/platform/lib/database"
)

// mapper.go is the boundary where driver types die (docs/erd-backend.md §4b.3):
// uuid.UUID and pgtype.* are absorbed here so the entity stays pure. Repositories
// return *Tenant / *AppUser, never the sqlc row.

// tenantToEntity maps a tenant row to the entity, decoding the address jsonb into
// a typed *Address. It returns an error because a malformed jsonb payload is an
// infra fault (we wrote it, so it should always parse) that must not be swallowed.
func tenantToEntity(r identitydb.Tenant) (*Tenant, error) {
	address, err := decodeAddress(r.Address)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return &Tenant{
		ID:                    r.ID.String(),
		ClerkOrgID:            r.ClerkOrgID,
		Name:                  r.Name,
		CreatedAt:             r.CreatedAt.Time,
		CNPJ:                  derefString(r.Cnpj),
		LegalName:             derefString(r.LegalName),
		TradeName:             derefString(r.TradeName),
		Address:               address,
		Phone:                 derefString(r.Phone),
		Email:                 derefString(r.Email),
		OnboardingCompletedAt: timeToPtr(r.OnboardingCompletedAt),
	}, nil
}

// meToEntity maps the GET /identity/me read model row to Me. UserID is left empty
// here — the use case fills it with the Clerk user id — since the row itself
// carries only the tenant scope and the onboarding gate.
func meToEntity(r identitydb.GetMeByClerkUserRow) *Me {
	tenantID := r.TenantID.String()
	return &Me{
		TenantID:              &tenantID,
		OnboardingCompletedAt: timeToPtr(r.OnboardingCompletedAt),
	}
}

// decodeAddress unmarshals the tenant.address jsonb into a typed Address. A NULL
// or empty column maps to nil (the tenant has no address yet), not an empty struct.
func decodeAddress(raw []byte) (*Address, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var address Address
	if err := json.Unmarshal(raw, &address); err != nil {
		return nil, err
	}
	return &address, nil
}

// encodeAddress serializes an Address to the jsonb payload written to the column.
func encodeAddress(address Address) ([]byte, error) {
	return json.Marshal(address)
}

// timeToPtr collapses a nullable timestamptz to a *time.Time, nil standing in for
// SQL NULL — tenant.onboarding_completed_at is unset until onboarding completes.
func timeToPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

func userToEntity(r identitydb.AppUser) *AppUser {
	return &AppUser{
		ID:          r.ID.String(),
		ClerkUserID: r.ClerkUserID,
		TenantID:    r.TenantID.String(),
		Email:       r.Email,
		Name:        derefString(r.Name),
		Phone:       derefString(r.Phone),
		Role:        Role(r.Role),
		CreatedAt:   r.CreatedAt.Time,
	}
}

// membershipToEntity maps an UpsertMembership row to the entity, absorbing the
// pgtype/uuid driver types. The row's `joined` flag is a control signal for the use
// case (new-vs-replay), not part of the entity, so it is read separately by the repo.
func membershipToEntity(r identitydb.UpsertMembershipRow) *Membership {
	return &Membership{
		ID:                r.ID.String(),
		TenantID:          r.TenantID.String(),
		AppUserID:         r.AppUserID.String(),
		ClerkMembershipID: derefString(r.ClerkMembershipID),
		Role:              Role(r.Role),
		Status:            MembershipStatus(r.Status),
		JoinedAt:          r.JoinedAt.Time,
		RemovedAt:         timeToPtr(r.RemovedAt),
	}
}

// membershipModelToEntity maps a base membership row (RETURNING *) to the entity,
// absorbing the pgtype/uuid driver types. Used by the soft-delete path, which
// returns the whole row rather than the UpsertMembership control shape.
func membershipModelToEntity(r identitydb.Membership) *Membership {
	return &Membership{
		ID:                r.ID.String(),
		TenantID:          r.TenantID.String(),
		AppUserID:         r.AppUserID.String(),
		ClerkMembershipID: derefString(r.ClerkMembershipID),
		Role:              Role(r.Role),
		Status:            MembershipStatus(r.Status),
		JoinedAt:          r.JoinedAt.Time,
		RemovedAt:         timeToPtr(r.RemovedAt),
	}
}

// derefString collapses a nullable text column (*string) to a plain string, an
// empty string standing in for SQL NULL — app_user.name and .phone are optional.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// textToNull is the inverse: an empty string is written as SQL NULL, not "".
// Used for every optional text column the slice writes (name, phone).
func textToNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
