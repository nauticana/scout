package domain

// ObjectRef points at content kept in object storage; relational rows store
// only the reference and the digest that verifies it.
type ObjectRef struct {
	URI    string
	Digest string
}
