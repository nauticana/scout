package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// RestrictedResolver adds the platform's and the tenant's standing denials to the
// policy a principal's release was published with. Layers are read at decision
// time, so a new prohibition needs no republish; an unreadable layer is an error,
// which the evaluator turns into a denial.
type RestrictedResolver struct {
	Release      contract.PolicyResolver
	Restrictions contract.RestrictionLayerReader
}

var _ contract.PolicyResolver = (*RestrictedResolver)(nil)

// Policies returns the release set with the layer denials appended. Its version
// names the release and both layers, so a decision record pins all three.
func (r *RestrictedResolver) Policies(ctx context.Context, principal domain.Principal) (domain.PolicySet, error) {
	if r.Release == nil || r.Restrictions == nil {
		return domain.PolicySet{}, fmt.Errorf("restricted policy resolver: release resolver and restriction layers are required")
	}
	set, err := r.Release.Policies(ctx, principal)
	if err != nil {
		return domain.PolicySet{}, err
	}
	layers, err := r.Restrictions.Layers(ctx, principal.TenantID)
	if err != nil {
		return domain.PolicySet{}, err
	}
	if layers.Platform.Digest == "" && layers.Tenant.Digest == "" {
		return set, nil
	}
	statements := make([]domain.PolicyStatement, 0, len(set.Statements)+len(layers.Platform.Denials)+len(layers.Tenant.Denials))
	statements = append(statements, set.Statements...)
	statements = append(statements, layers.Platform.Denials...)
	set.Statements = append(statements, layers.Tenant.Denials...)
	version := sha256.Sum256([]byte(set.Version + "\n" + layers.Platform.Digest + "\n" + layers.Tenant.Digest))
	set.Version = hex.EncodeToString(version[:])
	return set, nil
}
