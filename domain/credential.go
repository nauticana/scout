package domain

import "time"

// CredentialBinding points one principal at a scoped identity for one tool. It
// holds a reference into the keel secret or OAuth-connection store and never
// secret material, so it is safe in logs, definitions, and audit payloads.
type CredentialBinding struct {
	TenantID  int64
	Principal PrincipalRef
	ToolID    string
	// CredentialRef names a keel-held service identity or delegated connection.
	CredentialRef string
	// DelegatedFrom names the human whose connection is used, when one is.
	DelegatedFrom PrincipalRef
	// GrantID is the delegation whose revocation invalidates a delegated connection.
	GrantID string
	// Scopes are the least-privilege scopes the binding may request.
	Scopes  []string
	Purpose string
	// MaxTTL bounds the lifetime of an issued credential; zero takes the provider default.
	MaxTTL    time.Duration
	ValidFrom time.Time
	ValidTo   time.Time
	RevokedAt time.Time
}
