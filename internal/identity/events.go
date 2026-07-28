package identity

// Event contracts for the identity slice. Other slices may import these structs
// as the shape they consume — they are the only identity types allowed to cross
// a slice boundary (slices communicate by event, never by entity/repo).
//
// Publishing is NOT wired here: the producer writes to the outbox in the same tx
// as the entity (docs §4c.1), and that wiring lands when a listener needs it.

// TenantProvisioned is emitted after a tenant is created or synced from Clerk.
type TenantProvisioned struct {
	TenantID   string
	ClerkOrgID string
}

// UserProvisioned is emitted after an app_user is created or synced from Clerk.
type UserProvisioned struct {
	UserID   string
	TenantID string
}
