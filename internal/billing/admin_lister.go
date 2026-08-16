package billing

import "context"

// AdminLister resolves the ADMIN members of a tenant — the port failPayment
// consults to fan out the payment_failed e-mail to every admin, not just an
// arbitrary user. billing has no membership data of its own; the consumer
// (billing) defines this narrow port and a provider in another slice (identity,
// which already owns membership/role) supplies the concrete adapter — the same
// consumer-defines-the-port shape as acquisition.EntitlementChecker /
// billing.EntitlementAdapter, just with the roles reversed. Optional: a nil
// AdminLister (the zero value of UseCase.adminLister) makes failPayment skip the
// fan-out entirely, so tests and any composition that never wires one keep
// working unchanged.
type AdminLister interface {
	// ListTenantAdminIDs returns the internal app_user ids of every ACTIVE ADMIN
	// membership in tenantID. An empty slice (not an error) is a valid outcome —
	// a tenant with zero admins simply gets no fan-out.
	ListTenantAdminIDs(ctx context.Context, tenantID string) ([]string, error)
}
