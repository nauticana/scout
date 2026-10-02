package domain

// RestrictionLayer is one immutable version of standing prohibitions applied on
// top of every pinned release at decision time: deny policy statements and
// guardrail rules that block or redact. A layer can only restrict. Digest is the
// SHA-256 hex of its canonical content, empty for a layer never written.
type RestrictionLayer struct {
	Digest     string
	Denials    []PolicyStatement
	Guardrails []GuardrailRule
}

// RestrictionLayers are the layers in force for one tenant: the platform's,
// which applies to every tenant, and the tenant's own.
type RestrictionLayers struct {
	Platform RestrictionLayer
	Tenant   RestrictionLayer
}
