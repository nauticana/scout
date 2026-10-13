package domain

// ScopedPrincipalRef identifies a principal within one tenant.
type ScopedPrincipalRef struct {
	TenantID int64
	PrincipalRef
}

// ScopedClientRef identifies a client within one tenant.
type ScopedClientRef struct {
	TenantID  int64
	ClientRef string
}

// Labels are tenant-scoped display captions keyed by reference.
type Labels struct {
	Tenants    map[int64]string
	Principals map[ScopedPrincipalRef]string
	Clients    map[ScopedClientRef]string
}
