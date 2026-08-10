package domain

// GuardrailConfig is an immutable set of tenant policy rules.
type GuardrailConfig struct {
	Version string
	Rules   []byte
}
