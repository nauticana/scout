package domain

import "time"

// EffectStatus is what reading a mutation's postcondition back established.
type EffectStatus string

const (
	EffectSatisfied EffectStatus = "satisfied"
	EffectViolated  EffectStatus = "violated"
	EffectUnknown   EffectStatus = "unknown"
)

// EffectObservation is the external state read independently of the mutation's
// response; an acknowledgement is not an effect. Satisfied and violated rest on
// Evidence, the immutable records of what was seen; without any they are unknown.
type EffectObservation struct {
	Status     EffectStatus `json:"status"`
	Reason     string       `json:"reason,omitempty"`
	ObservedAt time.Time    `json:"observed_at,omitzero"`
	Evidence   []ObjectRef  `json:"evidence,omitempty"`
	// Reconciled marks an effect that already held, so the mutation was not invoked.
	Reconciled bool `json:"reconciled,omitempty"`
	// Output is the tool output of a reconciled call; it must satisfy the tool's output schema.
	Output []byte `json:"-"`
}
