package identity

import "github.com/jusassessoria/platform/internal/identity/identitydb"

// mapper.go is the boundary where driver types die (docs/erd-backend.md §4b.3):
// uuid.UUID and pgtype.* are absorbed here so the entity stays pure. Repositories
// return *Tenant / *AppUser, never the sqlc row.

func tenantToEntity(r identitydb.Tenant) *Tenant {
	return &Tenant{
		ID:         r.ID.String(),
		ClerkOrgID: r.ClerkOrgID,
		Name:       r.Name,
		CreatedAt:  r.CreatedAt.Time,
	}
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
