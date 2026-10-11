package scope

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Bounds of an evaluation_gate value.
const (
	maxGateMetricLength = 64
	maxGateSamples      = 1_000_000
)

// decodeEvaluationGate validates an evaluation_gate value and returns it canonical, scopes
// sorted; an empty value is no gate.
func decodeEvaluationGate(raw []byte) (domain.EvaluationGatePolicy, bool, error) {
	var value domain.EvaluationGatePolicy
	if len(bytes.TrimSpace(raw)) == 0 {
		return value, false, nil
	}
	if err := strictDecode(raw, &value); err != nil {
		return value, false, fmt.Errorf("%w: evaluation_gate value must be a gate object: %v", domain.ErrValidation, err)
	}
	if metric := strings.TrimSpace(value.Metric); metric == "" || metric != value.Metric || len(metric) > maxGateMetricLength {
		return value, false, fmt.Errorf("%w: evaluation_gate metric must be 1 to %d characters", domain.ErrValidation, maxGateMetricLength)
	}
	if value.MinScoreBP < 0 || value.MinScoreBP > domain.MaxEvaluationGateScoreBP {
		return value, false, fmt.Errorf("%w: evaluation_gate min_score_bp must be from 0 to %d", domain.ErrValidation, domain.MaxEvaluationGateScoreBP)
	}
	if regression := value.MaxRegressionBP; regression != nil && (*regression < 0 || *regression > domain.MaxEvaluationGateScoreBP) {
		return value, false, fmt.Errorf("%w: evaluation_gate max_regression_bp must be from 0 to %d", domain.ErrValidation, domain.MaxEvaluationGateScoreBP)
	}
	if value.MinSamples < 0 || value.MinSamples > maxGateSamples {
		return value, false, fmt.Errorf("%w: evaluation_gate min_samples must be from 0 to %d", domain.ErrValidation, maxGateSamples)
	}
	if len(value.RequiredScopes) == 0 {
		return value, false, fmt.Errorf("%w: evaluation_gate required_scopes must name dev, gate, or both", domain.ErrValidation)
	}
	for index, scope := range value.RequiredScopes {
		if !scope.Valid() || slices.Contains(value.RequiredScopes[:index], scope) {
			return value, false, fmt.Errorf("%w: evaluation_gate required_scopes must be distinct dev or gate", domain.ErrValidation)
		}
	}
	slices.Sort(value.RequiredScopes)
	return value, true, nil
}

// DecodeEvaluationGate returns the evaluation gate an effective evaluation_gate value holds.
func DecodeEvaluationGate(raw []byte) (domain.EvaluationGatePolicy, error) {
	value, present, err := decodeEvaluationGate(raw)
	if err == nil && !present {
		err = fmt.Errorf("%w: evaluation_gate value is empty", domain.ErrValidation)
	}
	return value, err
}

// EvaluationGateDigest is the SHA-256 of a gate's canonical encoding, so a decision records
// exactly which policy it applied.
func EvaluationGateDigest(policy domain.EvaluationGatePolicy) (string, error) {
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	if encoded, err = CanonicalValue(domain.ResourceEvaluationGate, encoded); err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// EvaluationGateMerger keeps the stricter of each inherited and child requirement, so a
// child may raise the minimum score or sample count, lower the allowed regression, or require
// another scope, and nothing it binds loosens its parent's gate. The metric is the parent's.
type EvaluationGateMerger struct{}

func (EvaluationGateMerger) Kind() domain.ResourceKind { return domain.ResourceEvaluationGate }

func (EvaluationGateMerger) Merge(_ context.Context, inherited []byte, override domain.ScopedBinding) ([]byte, error) {
	if override.MergeMode != domain.MergeReplace && override.MergeMode != "" {
		return nil, fmt.Errorf("%w: evaluation_gate supports replace only", domain.ErrValidation)
	}
	parent, bound, err := decodeEvaluationGate(inherited)
	if err != nil {
		return nil, err
	}
	child, err := DecodeEvaluationGate(override.Value)
	if err != nil {
		return nil, err
	}
	if bound {
		if child.Metric != parent.Metric {
			return nil, fmt.Errorf("%w: evaluation_gate may not change the inherited metric %q", domain.ErrAuthorityExceeded, parent.Metric)
		}
		child.MinScoreBP, child.MinSamples = max(child.MinScoreBP, parent.MinScoreBP), max(child.MinSamples, parent.MinSamples)
		if parent.MaxRegressionBP != nil && (child.MaxRegressionBP == nil || *child.MaxRegressionBP > *parent.MaxRegressionBP) {
			regression := *parent.MaxRegressionBP
			child.MaxRegressionBP = &regression
		}
		for _, scope := range parent.RequiredScopes {
			if !slices.Contains(child.RequiredScopes, scope) {
				child.RequiredScopes = append(child.RequiredScopes, scope)
			}
		}
		slices.Sort(child.RequiredScopes)
	}
	return json.Marshal(child)
}

// checkEvaluationGate refuses a candidate gate that admits anything its inherited gate refuses.
func checkEvaluationGate(kind domain.ResourceKind, inherited, candidate []byte) error {
	parent, bound, err := decodeEvaluationGate(inherited)
	if err != nil || !bound {
		return err
	}
	child, err := DecodeEvaluationGate(candidate)
	if err != nil {
		return err
	}
	switch {
	case child.Metric != parent.Metric:
		return fmt.Errorf("%w: %s may not change the inherited metric %q", domain.ErrAuthorityExceeded, kind, parent.Metric)
	case child.MinScoreBP < parent.MinScoreBP:
		return fmt.Errorf("%w: %s min_score_bp may not be lowered below %d", domain.ErrAuthorityExceeded, kind, parent.MinScoreBP)
	case child.MinSamples < parent.MinSamples:
		return fmt.Errorf("%w: %s min_samples may not be lowered below %d", domain.ErrAuthorityExceeded, kind, parent.MinSamples)
	case parent.MaxRegressionBP != nil && (child.MaxRegressionBP == nil || *child.MaxRegressionBP > *parent.MaxRegressionBP):
		return fmt.Errorf("%w: %s max_regression_bp may not be raised above %d or unset", domain.ErrAuthorityExceeded, kind, *parent.MaxRegressionBP)
	}
	for _, scope := range parent.RequiredScopes {
		if !slices.Contains(child.RequiredScopes, scope) {
			return fmt.Errorf("%w: %s must keep required scope %q", domain.ErrAuthorityExceeded, kind, scope)
		}
	}
	return nil
}

var _ contract.ResourceMerger = EvaluationGateMerger{}

// WithoutOperationalSettings returns release without its evaluation_gate and health_alerts
// resources, re-digested. They govern whether a release may be published and when its health
// alerts, not how it runs, so no release freezes them.
func WithoutOperationalSettings(release domain.EffectiveRelease) domain.EffectiveRelease {
	release.Resources = slices.DeleteFunc(slices.Clone(release.Resources), func(resource domain.EffectiveResource) bool {
		return resource.ResourceKind == domain.ResourceEvaluationGate || resource.ResourceKind == domain.ResourceHealthAlerts
	})
	release.Digest = Digest(release)
	return release
}

// EvaluationGateValue returns the evaluation_gate value among resources; nil when none is bound.
func EvaluationGateValue(resources []domain.EffectiveResource) []byte {
	for _, resource := range resources {
		if resource.ResourceKind == domain.ResourceEvaluationGate {
			return resource.Value
		}
	}
	return nil
}

// EvaluationGateLoosened reports whether after admits anything before refuses: a gate removed, or
// one with a lower score, sample count, or scope set, a higher or unset regression bound, or another metric.
func EvaluationGateLoosened(before, after []byte) (bool, error) {
	if len(bytes.TrimSpace(before)) == 0 {
		return false, nil
	}
	if len(bytes.TrimSpace(after)) == 0 {
		return true, nil
	}
	err := checkEvaluationGate(domain.ResourceEvaluationGate, before, after)
	if errors.Is(err, domain.ErrAuthorityExceeded) {
		return true, nil
	}
	return false, err
}
