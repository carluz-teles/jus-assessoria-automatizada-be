package identity

import "context"

// AdminListerAdapter implements billing.AdminLister over this slice's Repository.
// It exists so billing can fan out an e-mail to every admin of a tenant WITHOUT
// the two slices importing each other's entity/repo: billing defines the port it
// needs (AdminLister), identity supplies this concrete reader, and cmd/api
// injects it — the same consumer-defines-the-port rule billing.EntitlementAdapter
// already follows for acquisition, with the roles reversed here (identity is the
// provider, billing the consumer).
type AdminListerAdapter struct {
	repo Repository
}

// NewAdminListerAdapter builds the adapter over the identity repository.
// ListOrgMembers reads on the pool (no tx), so a *pgxpool.Pool-backed repo is all
// it needs.
func NewAdminListerAdapter(repo Repository) *AdminListerAdapter {
	return &AdminListerAdapter{repo: repo}
}

// ListTenantAdminIDs reuses ListOrgMembers (the same read GET
// /v1/organization/members serves) and filters to RoleAdmin — no new query: the
// team is small, so filtering in Go after one read is simpler than a second SQL
// query for what is already a screen-sized result set.
func (a *AdminListerAdapter) ListTenantAdminIDs(ctx context.Context, tenantID string) ([]string, error) {
	members, err := a.repo.ListOrgMembers(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		if m.Role == RoleAdmin {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}
