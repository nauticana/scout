package guardrail

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// EvidenceValidator checks each claim of an answer against its evidence and
// reports the unsupported ones individually, so the caller can keep the rest.
// Evidence verifies only as a digest-checked object, a tool result of the turn,
// a resource link such a tool returned, or a fact the tenant verified. With
// Claims set, every claim it detects in the answer must be a supported claim,
// and one citing facts must be stated by one of them.
type EvidenceValidator struct {
	Objects contract.EvidenceObjectVerifier
	Facts   contract.FactSource
	Claims  contract.ClaimDetector
}

func (validator *EvidenceValidator) Validate(ctx context.Context, scope domain.EvidenceScope, answer domain.EvidencedAnswer) (domain.EvidenceReport, error) {
	if scope.TenantID <= 0 {
		return domain.EvidenceReport{}, fmt.Errorf("%w: evidence scope needs a tenant", domain.ErrValidation)
	}
	rejected := make(map[string]string, len(answer.Evidence))
	known := make(map[string]struct{}, len(answer.Evidence))
	facts := map[string]string{}
	for _, evidence := range answer.Evidence {
		if _, duplicate := known[evidence.ID]; duplicate {
			rejected[evidence.ID] = "evidence id is duplicated"
			continue
		}
		known[evidence.ID] = struct{}{}
		reason, err := validator.verify(ctx, scope, evidence, facts)
		if err != nil {
			return domain.EvidenceReport{}, err
		}
		if reason != "" {
			rejected[evidence.ID] = reason
		}
	}
	report := domain.EvidenceReport{}
	for index, claim := range answer.Claims {
		if reason := unsupported(claim, known, rejected); reason != "" {
			report.Unsupported = append(report.Unsupported, domain.ClaimFinding{ClaimIndex: index, Claim: claim.Text, Reason: reason})
			continue
		}
		report.Supported = append(report.Supported, claim)
	}
	if validator.Claims == nil {
		return report, nil
	}
	detected, err := validator.Claims.Detect(ctx, scope.TenantID, answer.Answer)
	if err != nil {
		return domain.EvidenceReport{}, fmt.Errorf("detect claims: %w", err)
	}
	for _, span := range detected {
		if !slices.ContainsFunc(report.Supported, func(claim domain.Claim) bool { return states(claim, span, facts) }) {
			report.Unsupported = append(report.Unsupported, domain.ClaimFinding{ClaimIndex: -1, Claim: span, Reason: "claim is stated without supported evidence"})
		}
	}
	return report, nil
}

// states reports whether a supported claim carries span and, when it cites facts, one of them states it.
func states(claim domain.Claim, span string, facts map[string]string) bool {
	if !containsFold(claim.Text, span) {
		return false
	}
	cited := false
	for _, id := range claim.EvidenceIDs {
		if value, fact := facts[id]; fact {
			if containsFold(value, span) {
				return true
			}
			cited = true
		}
	}
	return !cited
}

func containsFold(text, part string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(part))
}

func unsupported(claim domain.Claim, known map[string]struct{}, rejected map[string]string) string {
	if len(claim.EvidenceIDs) == 0 {
		return "claim cites no evidence"
	}
	for _, id := range claim.EvidenceIDs {
		if _, ok := known[id]; !ok {
			return fmt.Sprintf("evidence %q is not part of the answer", id)
		}
		if reason, bad := rejected[id]; bad {
			return fmt.Sprintf("evidence %q: %s", id, reason)
		}
	}
	return ""
}

// verify returns why the evidence does not count; an error means verification itself failed.
func (validator *EvidenceValidator) verify(ctx context.Context, scope domain.EvidenceScope, evidence domain.EvidenceRef, facts map[string]string) (string, error) {
	if strings.TrimSpace(evidence.ID) == "" {
		return "evidence has no id", nil
	}
	switch evidence.Source {
	case domain.EvidenceObject:
		if len(evidence.Object.Digest) != 64 || evidence.Object.URI == "" {
			return "object reference has no digest", nil
		}
		if validator.Objects == nil {
			return "", fmt.Errorf("%w: no object verifier is composed", domain.ErrDegraded)
		}
		if err := validator.Objects.VerifyObject(ctx, scope.TenantID, evidence.Object); err != nil {
			if ctx.Err() != nil {
				return "", err
			}
			return "object does not verify against its digest", nil
		}
	case domain.EvidenceToolResult:
		if !slices.ContainsFunc(scope.ToolResults, func(result domain.ToolEvidence) bool { return result.CallID == evidence.CallID }) {
			return "no tool call of this turn returned it", nil
		}
	case domain.EvidenceMCPResource:
		if !slices.ContainsFunc(scope.ToolResults, func(result domain.ToolEvidence) bool {
			return result.CallID == evidence.CallID && slices.Contains(result.ResourceURIs, evidence.URI)
		}) {
			return "no tool call of this turn returned that resource link", nil
		}
	case domain.EvidenceFact:
		if strings.TrimSpace(evidence.FactKey) == "" {
			return "fact reference has no key", nil
		}
		if validator.Facts == nil {
			return "", fmt.Errorf("%w: no fact source is composed", domain.ErrDegraded)
		}
		value, ok, err := validator.Facts.VerifiedFact(ctx, scope.TenantID, evidence.FactKey)
		if err != nil {
			return "", fmt.Errorf("read fact %q: %w", evidence.FactKey, err)
		}
		if !ok {
			return "fact is not verified for this tenant", nil
		}
		facts[evidence.ID] = value
	default:
		return fmt.Sprintf("source %q is not verifiable", evidence.Source), nil
	}
	return "", nil
}

var _ contract.EvidenceValidator = (*EvidenceValidator)(nil)
