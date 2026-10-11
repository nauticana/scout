package scope

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// limitsValue is the limits binding. Zero is not limited at the binding's scope.
type limitsValue struct {
	TurnsPerMinute     int64 `json:"turns_per_minute"`
	MaxSteps           int64 `json:"max_steps"`
	MaxToolCalls       int64 `json:"max_tool_calls"`
	MaxDurationSeconds int64 `json:"max_duration_seconds"`
}

func (v limitsValue) fields() []limitField {
	return []limitField{
		{"turns_per_minute", v.TurnsPerMinute}, {"max_steps", v.MaxSteps},
		{"max_tool_calls", v.MaxToolCalls}, {"max_duration_seconds", v.MaxDurationSeconds},
	}
}

type limitField struct {
	name  string
	value int64
}

func decodeLimits(raw []byte) (limitsValue, error) {
	var value limitsValue
	if len(bytes.TrimSpace(raw)) == 0 {
		return value, nil
	}
	if err := strictDecode(raw, &value); err != nil {
		return value, fmt.Errorf("%w: limits value must be an object of non-negative integers: %v", domain.ErrValidation, err)
	}
	for _, field := range value.fields() {
		if field.value < 0 || field.value > domain.MaxExecutionLimit {
			return value, fmt.Errorf("%w: limits %s must be an integer from 0 to %d", domain.ErrValidation, field.name, domain.MaxExecutionLimit)
		}
	}
	return value, nil
}

// LimitsMerger keeps the tighter of each inherited and child limit, so a child cannot
// raise or unset a limit its parent sets, even with a replace binding.
type LimitsMerger struct{}

func (LimitsMerger) Kind() domain.ResourceKind { return domain.ResourceLimits }

func (LimitsMerger) Merge(_ context.Context, inherited []byte, override domain.ScopedBinding) ([]byte, error) {
	if override.MergeMode != domain.MergeReplace && override.MergeMode != "" {
		return nil, fmt.Errorf("%w: limits support replace only", domain.ErrValidation)
	}
	parent, err := decodeLimits(inherited)
	if err != nil {
		return nil, err
	}
	child, err := decodeLimits(override.Value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(limitsValue{
		TurnsPerMinute:     tighterLimit(parent.TurnsPerMinute, child.TurnsPerMinute),
		MaxSteps:           tighterLimit(parent.MaxSteps, child.MaxSteps),
		MaxToolCalls:       tighterLimit(parent.MaxToolCalls, child.MaxToolCalls),
		MaxDurationSeconds: tighterLimit(parent.MaxDurationSeconds, child.MaxDurationSeconds),
	})
}

func tighterLimit(parent, child int64) int64 {
	if parent > 0 {
		return minPositive(parent, child)
	}
	return child
}

// checkLimits applies at_most per field: a child may lower a field its parent limits,
// never raise or unset it, and may set any field its parent leaves unlimited.
func checkLimits(kind domain.ResourceKind, inherited, candidate []byte) error {
	parent, err := decodeLimits(inherited)
	if err != nil {
		return err
	}
	child, err := decodeLimits(candidate)
	if err != nil {
		return err
	}
	childFields := child.fields()
	for index, field := range parent.fields() {
		if limit := childFields[index].value; field.value > 0 && (limit == 0 || limit > field.value) {
			return fmt.Errorf("%w: %s %s may not be raised above %d or unset", domain.ErrAuthorityExceeded, kind, field.name, field.value)
		}
	}
	return nil
}

var _ contract.ResourceMerger = LimitsMerger{}
