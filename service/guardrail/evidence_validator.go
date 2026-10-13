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
	// Normalize, when set, rewrites claim text and fact values before matching, such as
	// dropping thousands separators; matching always folds case.
	Normalize func(string) string
}

func (validator *EvidenceValidator) Validate(ctx context.Context, scope domain.EvidenceScope, answer domain.EvidencedAnswer) (domain.EvidenceReport, error) {
	if scope.TenantID <= 0 {
		return domain.EvidenceReport{}, fmt.Errorf("%w: evidence scope needs a tenant", domain.ErrValidation)
	}
	var verified map[string]domain.VerifiedFact
	if slices.ContainsFunc(answer.Evidence, func(evidence domain.EvidenceRef) bool {
		return evidence.Source == domain.EvidenceFact && strings.TrimSpace(evidence.FactKey) != ""
	}) {
		facts, err := validator.verifiedFacts(ctx, scope.TenantID)
		if err != nil {
			return domain.EvidenceReport{}, err
		}
		verified = make(map[string]domain.VerifiedFact, len(facts))
		for _, fact := range facts {
			verified[fact.Key] = fact
		}
	}
	rejected := make(map[string]string, len(answer.Evidence))
	known := make(map[string]struct{}, len(answer.Evidence))
	cited := map[string]domain.VerifiedFact{}
	for _, evidence := range answer.Evidence {
		if _, duplicate := known[evidence.ID]; duplicate {
			rejected[evidence.ID] = "evidence id is duplicated"
			continue
		}
		known[evidence.ID] = struct{}{}
		reason, err := validator.verify(ctx, scope, evidence, verified, cited)
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
	detected, err := validator.detectClaims(ctx, scope.TenantID, answer.Answer)
	if err != nil {
		return domain.EvidenceReport{}, err
	}
	for _, span := range detected {
		if !slices.ContainsFunc(report.Supported, func(claim domain.Claim) bool { return validator.states(claim, span, cited) }) {
			report.Unsupported = append(report.Unsupported, undeclared(span))
		}
	}
	return report, nil
}

// ValidateText checks text that declares no claims, such as generated copy: every claim Claims
// detects must be stated by a verified fact of its kind. A supported claim cites the keys of
// the facts that state it.
func (validator *EvidenceValidator) ValidateText(ctx context.Context, scope domain.EvidenceScope, text string) (domain.EvidenceReport, error) {
	if scope.TenantID <= 0 {
		return domain.EvidenceReport{}, fmt.Errorf("%w: evidence scope needs a tenant", domain.ErrValidation)
	}
	if validator.Claims == nil {
		return domain.EvidenceReport{}, fmt.Errorf("%w: no claim detector is composed", domain.ErrDegraded)
	}
	detected, err := validator.detectClaims(ctx, scope.TenantID, text)
	if err != nil || len(detected) == 0 {
		return domain.EvidenceReport{}, err
	}
	facts, err := validator.verifiedFacts(ctx, scope.TenantID)
	if err != nil {
		return domain.EvidenceReport{}, err
	}
	report := domain.EvidenceReport{}
	for _, span := range detected {
		claim := domain.Claim{Text: span.Text}
		for _, fact := range facts {
			if validator.stated(fact, span) {
				claim.EvidenceIDs = append(claim.EvidenceIDs, fact.Key)
			}
		}
		if len(claim.EvidenceIDs) == 0 {
			report.Unsupported = append(report.Unsupported, undeclared(span))
			continue
		}
		report.Supported = append(report.Supported, claim)
	}
	return report, nil
}

func (validator *EvidenceValidator) detectClaims(ctx context.Context, tenantID int64, text string) ([]domain.DetectedClaim, error) {
	detected, err := validator.Claims.Detect(ctx, tenantID, text)
	if err != nil {
		return nil, fmt.Errorf("detect claims: %w", err)
	}
	for _, claim := range detected {
		if strings.TrimSpace(claim.Text) == "" {
			return nil, fmt.Errorf("%w: claim detector returned an empty claim", domain.ErrValidation)
		}
	}
	return detected, nil
}

func (validator *EvidenceValidator) verifiedFacts(ctx context.Context, tenantID int64) ([]domain.VerifiedFact, error) {
	if validator.Facts == nil {
		return nil, fmt.Errorf("%w: no fact source is composed", domain.ErrDegraded)
	}
	facts, err := validator.Facts.VerifiedFacts(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("read verified facts: %w", err)
	}
	seen := make(map[string]struct{}, len(facts))
	for _, fact := range facts {
		if fact.Key == "" || fact.Key != strings.TrimSpace(fact.Key) {
			return nil, fmt.Errorf("%w: verified fact needs a key", domain.ErrValidation)
		}
		if _, duplicate := seen[fact.Key]; duplicate {
			return nil, fmt.Errorf("%w: verified fact %q is duplicated", domain.ErrConflict, fact.Key)
		}
		seen[fact.Key] = struct{}{}
	}
	return facts, nil
}

func undeclared(span domain.DetectedClaim) domain.ClaimFinding {
	return domain.ClaimFinding{ClaimIndex: -1, Claim: span.Text, Reason: "claim is stated without supported evidence"}
}

// states reports whether a supported claim carries span and, when it cites facts, one of them states it.
func (validator *EvidenceValidator) states(claim domain.Claim, span domain.DetectedClaim, cited map[string]domain.VerifiedFact) bool {
	if !validator.contains(claim.Text, span.Text) {
		return false
	}
	citesFacts := false
	for _, id := range claim.EvidenceIDs {
		if fact, ok := cited[id]; ok {
			if validator.stated(fact, span) {
				return true
			}
			citesFacts = true
		}
	}
	return !citesFacts
}

func (validator *EvidenceValidator) stated(fact domain.VerifiedFact, span domain.DetectedClaim) bool {
	return (span.Kind == "" || fact.Kind == span.Kind) && validator.contains(fact.Value, span.Text)
}

func (validator *EvidenceValidator) contains(text, part string) bool {
	if validator.Normalize != nil {
		text, part = validator.Normalize(text), validator.Normalize(part)
	}
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
func (validator *EvidenceValidator) verify(ctx context.Context, scope domain.EvidenceScope, evidence domain.EvidenceRef, verified, cited map[string]domain.VerifiedFact) (string, error) {
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
		fact, ok := verified[evidence.FactKey]
		if !ok {
			return "fact is not verified for this tenant", nil
		}
		cited[evidence.ID] = fact
	default:
		return fmt.Sprintf("source %q is not verifiable", evidence.Source), nil
	}
	return "", nil
}

var _ contract.EvidenceValidator = (*EvidenceValidator)(nil)
