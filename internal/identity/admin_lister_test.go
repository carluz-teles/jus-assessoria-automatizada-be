package identity

import (
	"context"
	"errors"
	"testing"
)

// FLO-69: ListTenantAdminIDs reuses ListOrgMembers and filters to RoleAdmin only
// — a LAWYER member is excluded, an ADMIN is included, by app_user id (not the
// membership row's own id).
func TestAdminListerAdapter_ListTenantAdminIDs_FiltersToAdmins(t *testing.T) {
	repo := &mockRepo{
		listOrgMembers: func(_ context.Context, tenantID string) ([]OrgMember, error) {
			if tenantID != "tenant-uuid" {
				t.Fatalf("scope = %q, want tenant-uuid", tenantID)
			}
			return []OrgMember{
				{ID: "admin-1", Name: "Ana", Role: RoleAdmin},
				{ID: "lawyer-1", Name: "Bruno", Role: RoleLawyer},
				{ID: "admin-2", Name: "Carla", Role: RoleAdmin},
			}, nil
		},
	}
	adapter := NewAdminListerAdapter(repo)

	got, err := adapter.ListTenantAdminIDs(context.Background(), "tenant-uuid")
	if err != nil {
		t.Fatalf("ListTenantAdminIDs: %v", err)
	}
	if len(got) != 2 || got[0] != "admin-1" || got[1] != "admin-2" {
		t.Fatalf("admin ids = %v, want [admin-1 admin-2]", got)
	}
}

// No admins in the tenant → an empty (never nil) slice, not an error.
func TestAdminListerAdapter_ListTenantAdminIDs_NoAdmins(t *testing.T) {
	repo := &mockRepo{
		listOrgMembers: func(context.Context, string) ([]OrgMember, error) {
			return []OrgMember{{ID: "lawyer-1", Role: RoleLawyer}}, nil
		},
	}
	adapter := NewAdminListerAdapter(repo)

	got, err := adapter.ListTenantAdminIDs(context.Background(), "tenant-uuid")
	if err != nil {
		t.Fatalf("ListTenantAdminIDs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("admin ids = %v, want none", got)
	}
}

// A repo failure propagates unchanged.
func TestAdminListerAdapter_ListTenantAdminIDs_RepoErrorPropagates(t *testing.T) {
	boom := errors.New("db unreachable")
	repo := &mockRepo{
		listOrgMembers: func(context.Context, string) ([]OrgMember, error) { return nil, boom },
	}
	adapter := NewAdminListerAdapter(repo)

	_, err := adapter.ListTenantAdminIDs(context.Background(), "tenant-uuid")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
