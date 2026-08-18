package policy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ReleaseResolver reads the policy set frozen into a principal's effective
// release, so a decision uses the policy the work was published with rather than
// whatever is bound at the scope right now.
type ReleaseResolver struct {
	Releases contract.EffectiveReleaseRepository
}

// Policies returns the merged policy set for the principal's pinned release.
func (r *ReleaseResolver) Policies(ctx context.Context, principal domain.Principal) (domain.PolicySet, error) {
	if r.Releases == nil {
		return domain.PolicySet{}, fmt.Errorf("policy resolver: effective release repository is required")
	}
	if principal.Release == "" {
		return domain.PolicySet{}, fmt.Errorf("%w: principal %q is not pinned to a release", domain.ErrForbidden, principal.ID)
	}
	release, err := r.Releases.Get(ctx, principal.TenantID, principal.ID, principal.Release)
	if err != nil {
		return domain.PolicySet{}, err
	}
	set := domain.PolicySet{PolicyID: release.AgentID, Version: release.Digest, SchemaVersion: 1}
	for _, resource := range release.Resources {
		if resource.ResourceKind != domain.ResourcePolicy {
			continue
		}
		var statements []domain.PolicyStatement
		if err := json.Unmarshal(resource.Value, &statements); err != nil {
			return domain.PolicySet{}, fmt.Errorf("%w: policy %q in release %s is not a statement list: %v",
				domain.ErrValidation, resource.ResourceID, release.Digest, err)
		}
		set.Statements = append(set.Statements, statements...)
	}
	return set, nil
}

var _ contract.PolicyResolver = (*ReleaseResolver)(nil)
