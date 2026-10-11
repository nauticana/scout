package domain

// StepIdempotencyStatus is the lifecycle code of one step's idempotency claim: an
// idempotency_status code.
type StepIdempotencyStatus string

const (
	StepClaimed   StepIdempotencyStatus = "C"
	StepCommitted StepIdempotencyStatus = "M"
	StepAbandoned StepIdempotencyStatus = "A"
)

// Valid reports whether the status is a seeded idempotency_status code.
func (status StepIdempotencyStatus) Valid() bool {
	return status == StepClaimed || status == StepCommitted || status == StepAbandoned
}
