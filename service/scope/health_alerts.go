package scope

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type healthAlertField struct {
	name         string
	value, limit int64
}

func healthAlertFields(p domain.HealthAlertPolicy) []healthAlertField {
	return []healthAlertField{
		{"min_turns", p.MinTurns, domain.MaxHealthCount},
		{"max_failure_rate_bp", p.MaxFailureRateBP, domain.MaxHealthRateBP},
		{"max_p95_latency_ms", p.MaxP95LatencyMS, domain.MaxHealthLatencyMS},
		{"max_hard_safety_events", p.MaxHardSafetyEvents, domain.MaxHealthCount},
		{"max_silence_minutes", p.MaxSilenceMinutes, domain.MaxHealthWindowMinutes},
		{"max_unresolved_minutes", p.MaxUnresolvedMinutes, domain.MaxHealthWindowMinutes},
	}
}

// decodeHealthAlerts validates a health_alerts value; an empty value is no policy.
func decodeHealthAlerts(raw []byte) (domain.HealthAlertPolicy, bool, error) {
	var value domain.HealthAlertPolicy
	if len(bytes.TrimSpace(raw)) == 0 {
		return value, false, nil
	}
	if err := strictDecode(raw, &value); err != nil {
		return value, false, fmt.Errorf("%w: health_alerts value must be an object of non-negative integers: %v", domain.ErrValidation, err)
	}
	if value.WindowMinutes < 0 || value.WindowMinutes > domain.MaxHealthWindowMinutes {
		return value, false, fmt.Errorf("%w: health_alerts window_minutes must be from 0 to %d", domain.ErrValidation, domain.MaxHealthWindowMinutes)
	}
	for _, field := range healthAlertFields(value) {
		if field.value < 0 || field.value > field.limit {
			return value, false, fmt.Errorf("%w: health_alerts %s must be from 0 to %d", domain.ErrValidation, field.name, field.limit)
		}
	}
	return value, true, nil
}

// DecodeHealthAlerts returns the policy an effective health_alerts value holds.
func DecodeHealthAlerts(raw []byte) (domain.HealthAlertPolicy, error) {
	value, present, err := decodeHealthAlerts(raw)
	if err == nil && !present {
		err = fmt.Errorf("%w: health_alerts value is empty", domain.ErrValidation)
	}
	return value, err
}

// HealthAlertsMerger keeps the stricter of each inherited and child field: a child may lower a
// threshold, the sample floor, the silence span, or the unresolved-outcome span its parent sets
// and set one its parent leaves at 0, and its 0 inherits. The window is the parent's once the parent sets one.
type HealthAlertsMerger struct{}

func (HealthAlertsMerger) Kind() domain.ResourceKind { return domain.ResourceHealthAlerts }

func (HealthAlertsMerger) Merge(_ context.Context, inherited []byte, override domain.ScopedBinding) ([]byte, error) {
	if override.MergeMode != domain.MergeReplace && override.MergeMode != "" {
		return nil, fmt.Errorf("%w: health_alerts supports replace only", domain.ErrValidation)
	}
	parent, _, err := decodeHealthAlerts(inherited)
	if err != nil {
		return nil, err
	}
	child, err := DecodeHealthAlerts(override.Value)
	if err != nil {
		return nil, err
	}
	if parent.WindowMinutes > 0 {
		if child.WindowMinutes != 0 && child.WindowMinutes != parent.WindowMinutes {
			return nil, fmt.Errorf("%w: health_alerts may not change the inherited window of %d minutes", domain.ErrAuthorityExceeded, parent.WindowMinutes)
		}
		child.WindowMinutes = parent.WindowMinutes
	}
	return json.Marshal(domain.HealthAlertPolicy{
		WindowMinutes:        child.WindowMinutes,
		MinTurns:             tighterLimit(parent.MinTurns, child.MinTurns),
		MaxFailureRateBP:     tighterLimit(parent.MaxFailureRateBP, child.MaxFailureRateBP),
		MaxP95LatencyMS:      tighterLimit(parent.MaxP95LatencyMS, child.MaxP95LatencyMS),
		MaxHardSafetyEvents:  tighterLimit(parent.MaxHardSafetyEvents, child.MaxHardSafetyEvents),
		MaxSilenceMinutes:    tighterLimit(parent.MaxSilenceMinutes, child.MaxSilenceMinutes),
		MaxUnresolvedMinutes: tighterLimit(parent.MaxUnresolvedMinutes, child.MaxUnresolvedMinutes),
	})
}

// checkHealthAlerts refuses a candidate that keeps quiet about anything its inherited policy
// alerts on: a changed window, or a field raised above or unset from what the parent sets.
func checkHealthAlerts(kind domain.ResourceKind, inherited, candidate []byte) error {
	parent, bound, err := decodeHealthAlerts(inherited)
	if err != nil || !bound {
		return err
	}
	child, err := DecodeHealthAlerts(candidate)
	if err != nil {
		return err
	}
	if parent.WindowMinutes > 0 && child.WindowMinutes != parent.WindowMinutes {
		return fmt.Errorf("%w: %s may not change the inherited window of %d minutes", domain.ErrAuthorityExceeded, kind, parent.WindowMinutes)
	}
	childFields := healthAlertFields(child)
	for index, field := range healthAlertFields(parent) {
		if value := childFields[index].value; field.value > 0 && (value == 0 || value > field.value) {
			return fmt.Errorf("%w: %s %s may not be raised above %d or unset", domain.ErrAuthorityExceeded, kind, field.name, field.value)
		}
	}
	return nil
}

// HealthAlertsValue returns the health_alerts value among resources; nil when none is bound.
func HealthAlertsValue(resources []domain.EffectiveResource) []byte {
	for _, resource := range resources {
		if resource.ResourceKind == domain.ResourceHealthAlerts {
			return resource.Value
		}
	}
	return nil
}

var _ contract.ResourceMerger = HealthAlertsMerger{}
