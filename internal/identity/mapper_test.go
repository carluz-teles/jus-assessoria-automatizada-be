package identity

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/identity/identitydb"
)

func TestTenantToEntity(t *testing.T) {
	id := uuid.New()
	created := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)

	got := tenantToEntity(identitydb.Tenant{
		ID:         id,
		ClerkOrgID: "org_abc",
		Name:       "Escritório",
		CreatedAt:  pgtype.Timestamptz{Time: created, Valid: true},
	})

	if got.ID != id.String() {
		t.Errorf("ID = %q, want %q", got.ID, id.String())
	}
	if got.ClerkOrgID != "org_abc" || got.Name != "Escritório" {
		t.Errorf("fields = %+v", got)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, created)
	}
}

func TestUserToEntity(t *testing.T) {
	id, tenantID := uuid.New(), uuid.New()
	name := "Ana"
	phone := "+5511987654321"

	tests := []struct {
		name      string
		rowName   *string
		rowPhone  *string
		wantName  string
		wantPhone string
	}{
		{name: "named user with phone", rowName: &name, rowPhone: &phone, wantName: "Ana", wantPhone: phone},
		{name: "null name and phone collapse to empty string", rowName: nil, rowPhone: nil, wantName: "", wantPhone: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := userToEntity(identitydb.AppUser{
				ID:          id,
				ClerkUserID: "user_xyz",
				TenantID:    tenantID,
				Email:       "a@b.com",
				Name:        tt.rowName,
				Phone:       tt.rowPhone,
				Role:        "ADMIN",
				CreatedAt:   pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
			})

			if got.ID != id.String() || got.TenantID != tenantID.String() {
				t.Errorf("uuid mapping wrong: %+v", got)
			}
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if got.Phone != tt.wantPhone {
				t.Errorf("Phone = %q, want %q", got.Phone, tt.wantPhone)
			}
			if got.Role != RoleAdmin {
				t.Errorf("Role = %q, want %q", got.Role, RoleAdmin)
			}
		})
	}
}

func TestTextToNull(t *testing.T) {
	if got := textToNull(""); got != nil {
		t.Errorf("textToNull(\"\") = %v, want nil", got)
	}
	got := textToNull("Ana")
	if got == nil || *got != "Ana" {
		t.Errorf("textToNull(\"Ana\") = %v, want pointer to \"Ana\"", got)
	}
}

// derefString and textToNull are inverses for any non-empty text.
func TestTextRoundTrip(t *testing.T) {
	const value = "Ana"
	if got := derefString(textToNull(value)); got != value {
		t.Errorf("round-trip = %q, want %q", got, value)
	}
}
