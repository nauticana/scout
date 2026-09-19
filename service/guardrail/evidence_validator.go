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
// or a resource link such a tool returned.
type EvidenceValidator struct {
	Objects contract.EvidenceObjectVerifier
}

func (validator *EvidenceValidator) Validate(ctx context.Context, scope domain.EvidenceScope, answer domain.EvidencedAnswer) (domain.EvidenceReport, error) {
	if scope.TenantID <= 0 {
		return domain.EvidenceReport{}, fmt.Errorf("%w: evidence scope needs a tenant", domain.ErrValidation)
	}
	rejected := make(map[string]string, len(answer.Evidence))
	known := make(map[string]struct{}, len(answer.Evidence))
	for _, evidence := range answer.Evidence {
		if _, duplicate := known[evidence.ID]; duplicate {
			rejected[evidence.ID] = "evidence id is duplicated"
			continue
		}
		known[evidence.ID] = struct{}{}
		reason, err := validator.verify(ctx, scope, evidence)
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
	return report, nil
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
func (validator *EvidenceValidator) verify(ctx context.Context, scope domain.EvidenceScope, evidence domain.EvidenceRef) (string, error) {
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
	default:
		return fmt.Sprintf("source %q is not verifiable", evidence.Source), nil
	}
	return "", nil
}

var _ contract.EvidenceValidator = (*EvidenceValidator)(nil)
