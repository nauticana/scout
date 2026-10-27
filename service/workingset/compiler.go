// Package workingset compiles a turn's authority-aware working set without a model call.
package workingset

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	// DefaultCandidates and DefaultTokens apply to zero limits; MaxCandidates and MaxTokens are hard ceilings.
	DefaultCandidates = 64
	DefaultTokens     = 8000
	MaxCandidates     = 1024
	MaxTokens         = 1 << 20
)

// Compiler is the deterministic WorkingSetCompiler. It relies on the source's standing of each
// candidate, such as Charter's CurrentFacts, and never re-judges freshness, supersession, or rank.
// Only current and conflicted candidates are eligible. Required, controlling, and conflicted ones,
// and every current one of a required key, are kept or the compilation fails; the rest fill the
// token budget by authority rank, then reliability, then cost.
type Compiler struct {
	Source contract.WorkingSetCandidateSource
	Limits domain.WorkingSetLimits
}

var _ contract.WorkingSetCompiler = (*Compiler)(nil)

func (compiler *Compiler) limits() (domain.WorkingSetLimits, error) {
	limits := compiler.Limits
	if limits.MaxCandidates < 0 || limits.MaxCandidates > MaxCandidates || limits.MaxTokens < 0 || limits.MaxTokens > MaxTokens {
		return limits, fmt.Errorf("%w: working set limits must lie within 0..%d candidates and 0..%d tokens", domain.ErrValidation, MaxCandidates, MaxTokens)
	}
	limits.MaxCandidates = cmp.Or(limits.MaxCandidates, DefaultCandidates)
	limits.MaxTokens = cmp.Or(limits.MaxTokens, DefaultTokens)
	return limits, nil
}

// Compile reads the turn's candidates and selects its working set.
func (compiler *Compiler) Compile(ctx context.Context, request domain.WorkingSetRequest) (domain.WorkingSet, error) {
	if compiler.Source == nil {
		return domain.WorkingSet{}, fmt.Errorf("%w: working set compiler needs a candidate source", domain.ErrNotReady)
	}
	limits, err := compiler.limits()
	if err != nil {
		return domain.WorkingSet{}, err
	}
	if request.TenantContext.TenantID <= 0 || request.Principal.TenantID != request.TenantContext.TenantID || strings.TrimSpace(request.RequestID) == "" {
		return domain.WorkingSet{}, fmt.Errorf("%w: a working set needs the tenant, its principal, and the request", domain.ErrValidation)
	}
	offered, err := compiler.Source.Candidates(ctx, request)
	if err != nil {
		return domain.WorkingSet{}, fmt.Errorf("working set candidates: %w", err)
	}
	if len(offered.Candidates) > limits.MaxCandidates {
		return domain.WorkingSet{}, fmt.Errorf("%w: %d working set candidates exceed %d", domain.ErrExecutionLimit, len(offered.Candidates), limits.MaxCandidates)
	}
	required, err := requiredKeys(offered)
	if err != nil {
		return domain.WorkingSet{}, err
	}
	return selectSet(offered.Candidates, required, limits.MaxTokens)
}

// requiredKeys validates the offer whole and returns the keys the turn needs: those declared
// and those of every required or controlling candidate.
func requiredKeys(offered domain.WorkingSetCandidates) (map[string]bool, error) {
	required := make(map[string]bool, len(offered.RequiredKeys))
	for _, key := range offered.RequiredKeys {
		if strings.TrimSpace(key) == "" || key != strings.TrimSpace(key) || required[key] {
			return nil, fmt.Errorf("%w: required working set keys must be distinct, trimmed, and non-blank", domain.ErrValidation)
		}
		required[key] = true
	}
	seen := make(map[string]bool, len(offered.Candidates))
	for _, candidate := range offered.Candidates {
		if err := validCandidate(candidate); err != nil {
			return nil, err
		}
		if seen[candidate.ID] {
			return nil, fmt.Errorf("%w: working set candidate %q is offered twice", domain.ErrValidation, candidate.ID)
		}
		seen[candidate.ID] = true
		if candidate.Required || candidate.Controlling {
			required[key(candidate)] = true
		}
	}
	return required, nil
}

func validCandidate(candidate domain.WorkingSetCandidate) error {
	switch {
	case strings.TrimSpace(candidate.ID) == "" || candidate.ID != strings.TrimSpace(candidate.ID) || len(candidate.ID) > domain.MaxWorkingSetCandidateIDBytes:
		return fmt.Errorf("%w: a working set candidate id is non-blank, trimmed, and at most %d bytes", domain.ErrValidation, domain.MaxWorkingSetCandidateIDBytes)
	case candidate.Key != strings.TrimSpace(candidate.Key):
		return fmt.Errorf("%w: working set candidate %q has an untrimmed key", domain.ErrValidation, candidate.ID)
	case strings.TrimSpace(candidate.Provenance) == "":
		return fmt.Errorf("%w: working set candidate %q has no provenance", domain.ErrValidation, candidate.ID)
	case strings.TrimSpace(candidate.Content) == "" && (candidate.Reference == nil || strings.TrimSpace(candidate.Reference.URI) == "" || strings.TrimSpace(candidate.Reference.Digest) == ""):
		return fmt.Errorf("%w: working set candidate %q has neither content nor a digested reference", domain.ErrValidation, candidate.ID)
	case !candidate.Standing.Valid():
		return fmt.Errorf("%w: working set candidate %q has unknown standing %q", domain.ErrValidation, candidate.ID, candidate.Standing)
	case candidate.AuthorityRank < 0 || candidate.TokenCost < 0 || candidate.ReliabilityPercent < 0 || candidate.ReliabilityPercent > domain.MaxWorkingSetReliability:
		return fmt.Errorf("%w: working set candidate %q needs a non-negative rank and cost and a reliability within 0..%d", domain.ErrValidation, candidate.ID, domain.MaxWorkingSetReliability)
	}
	return nil
}

func key(candidate domain.WorkingSetCandidate) string {
	return cmp.Or(candidate.Key, candidate.ID)
}

// preferred orders candidates by authority rank, then reliability, then cost, then id.
func preferred(a, b domain.WorkingSetCandidate) int {
	return cmp.Or(cmp.Compare(a.AuthorityRank, b.AuthorityRank), cmp.Compare(b.ReliabilityPercent, a.ReliabilityPercent),
		cmp.Compare(a.TokenCost, b.TokenCost), strings.Compare(a.ID, b.ID))
}

func selectSet(candidates []domain.WorkingSetCandidate, required map[string]bool, budget int64) (domain.WorkingSet, error) {
	ordered := slices.Clone(candidates)
	slices.SortFunc(ordered, preferred)
	keep := make([]bool, len(ordered))
	satisfied, conflicted := map[string]bool{}, false
	var spent int64
	for i, candidate := range ordered {
		current := candidate.Standing == domain.StandingCurrent
		if current {
			satisfied[key(candidate)] = true
		}
		conflict := candidate.Standing == domain.StandingConflicted
		conflicted = conflicted || conflict
		if !(current && (candidate.Required || candidate.Controlling || required[key(candidate)]) || conflict) {
			continue
		}
		if candidate.TokenCost > budget-spent {
			return domain.WorkingSet{}, fmt.Errorf("%w: the required working set exceeds its %d-token budget at candidate %q", domain.ErrExecutionLimit, budget, candidate.ID)
		}
		keep[i], spent = true, spent+candidate.TokenCost
	}
	dropped := false
	for i, candidate := range ordered {
		if keep[i] || candidate.Standing != domain.StandingCurrent {
			continue
		}
		if candidate.TokenCost > budget-spent {
			dropped = true
			continue
		}
		keep[i], spent = true, spent+candidate.TokenCost
	}
	set := domain.WorkingSet{TokenCost: spent}
	for i, candidate := range ordered {
		if keep[i] {
			set.Selected = append(set.Selected, candidate)
		} else {
			set.Excluded = append(set.Excluded, candidate.ID)
		}
	}
	set.Assurance = assurance(conflicted, required, satisfied, dropped)
	return set, nil
}

// assurance ranks a conflict above missing evidence, and missing evidence above anything a budget did.
func assurance(conflicted bool, required, satisfied map[string]bool, dropped bool) domain.WorkingSetAssurance {
	if conflicted {
		return domain.AssuranceConflicted
	}
	for key := range required {
		if !satisfied[key] {
			return domain.AssuranceInsufficient
		}
	}
	switch {
	case len(required) == 0:
		return domain.AssuranceUnverified
	case dropped:
		return domain.AssuranceBounded
	}
	return domain.AssuranceVerified
}
