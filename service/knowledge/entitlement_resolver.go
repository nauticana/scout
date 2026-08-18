package knowledge

import (
	"context"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ReleaseEntitlements derives a principal's retrieval labels from the effective
// release instead of trusting the request path. Compilation already checked the
// labels are a subset of every parent scope's, so a caller cannot widen them by
// asserting more.
type ReleaseEntitlements struct {
	Releases contract.EffectiveReleaseRepository
}

// Entitlements returns the canonical label array and its digest.
func (r *ReleaseEntitlements) Entitlements(ctx context.Context, principal domain.Principal) ([]byte, string, error) {
	if r.Releases == nil {
		return nil, "", fmt.Errorf("entitlement resolver: effective release repository is required")
	}
	if principal.Release == "" {
		return nil, "", fmt.Errorf("%w: principal %q is not pinned to a release", domain.ErrForbidden, principal.ID)
	}
	release, err := r.Releases.Get(ctx, principal.TenantID, principal.ID, principal.Release)
	if err != nil {
		return nil, "", err
	}
	for _, resource := range release.Resources {
		if resource.ResourceKind != domain.ResourceEntitlement {
			continue
		}
		labels, err := ParseEntitlements(resource.Value)
		if err != nil {
			return nil, "", err
		}
		if len(labels) == 0 {
			break
		}
		return resource.Value, EntitlementsDigest(resource.Value), nil
	}
	// Fail closed: an agent with no entitlement binding retrieves nothing.
	return nil, "", fmt.Errorf("%w: release %s binds no entitlements", domain.ErrForbidden, principal.Release)
}

// ResolveQuery fills a query's entitlements from the principal, replacing
// whatever the caller supplied.
func ResolveQuery(ctx context.Context, resolver contract.EntitlementResolver, query domain.KnowledgeQuery) (domain.KnowledgeQuery, error) {
	labels, digest, err := resolver.Entitlements(ctx, query.Principal)
	if err != nil {
		return domain.KnowledgeQuery{}, err
	}
	query.Entitlements, query.EntitlementsDigest = labels, digest
	return query, nil
}

var _ contract.EntitlementResolver = (*ReleaseEntitlements)(nil)
