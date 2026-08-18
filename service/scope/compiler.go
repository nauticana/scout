package scope

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Compiler folds a scope chain, widest scope first, into one immutable effective
// release at publication time. Each fold checks the merged value against what it
// inherited, so a broadening override fails whatever merge mode produced it.
type Compiler struct {
	Scopes   contract.ScopeRepository
	Mergers  contract.ResourceMergerRegistry
	Checker  contract.NarrowingChecker
	MaxDepth int
	Now      func() time.Time
}

// NewCompiler validates dependencies and applies the default depth ceiling.
func NewCompiler(scopes contract.ScopeRepository, mergers contract.ResourceMergerRegistry, checker contract.NarrowingChecker, maxDepth int) (*Compiler, error) {
	if scopes == nil || mergers == nil || checker == nil {
		return nil, fmt.Errorf("scope compiler: repository, mergers, and narrowing checker are required")
	}
	if maxDepth < 0 {
		return nil, fmt.Errorf("scope compiler: max depth cannot be negative")
	}
	if maxDepth == 0 {
		maxDepth = DefaultMaxScopeDepth
	}
	return &Compiler{Scopes: scopes, Mergers: mergers, Checker: checker, MaxDepth: maxDepth}, nil
}

// DefaultMaxScopeDepth bounds a scope chain when no flag value is supplied.
const DefaultMaxScopeDepth = 8

// Compile resolves the chain, folds every bound resource, and digests the result.
func (c *Compiler) Compile(ctx context.Context, request domain.CompileRequest) (domain.EffectiveRelease, error) {
	if request.TenantID <= 0 || strings.TrimSpace(request.AgentID) == "" || strings.TrimSpace(request.AgentVersion) == "" {
		return domain.EffectiveRelease{}, fmt.Errorf("%w: tenant, agent, and agent version are required", domain.ErrValidation)
	}
	asOf := request.AsOf
	if asOf.IsZero() {
		asOf = c.now()
	}
	chain, err := c.Scopes.Chain(ctx, request.TenantID, request.ScopeID)
	if err != nil {
		return domain.EffectiveRelease{}, err
	}
	if len(chain) == 0 {
		return domain.EffectiveRelease{}, fmt.Errorf("%w: scope %q has no chain", domain.ErrNotFound, request.ScopeID)
	}
	if len(chain) > c.MaxDepth {
		return domain.EffectiveRelease{}, fmt.Errorf("%w: scope chain is %d deep, the limit is %d", domain.ErrValidation, len(chain), c.MaxDepth)
	}

	rank := make(map[string]int, len(chain))
	kindOf := make(map[string]string, len(chain))
	ids := make([]string, 0, len(chain))
	for index, node := range chain {
		rank[node.ScopeID] = index
		kindOf[node.ScopeID] = node.ScopeKind
		ids = append(ids, node.ScopeID)
	}

	bindings, err := c.Scopes.Bindings(ctx, request.TenantID, ids, asOf)
	if err != nil {
		return domain.EffectiveRelease{}, err
	}
	groups, order, err := groupBindings(bindings, rank)
	if err != nil {
		return domain.EffectiveRelease{}, err
	}

	compiledAt := c.now()
	release := domain.EffectiveRelease{
		TenantID: request.TenantID, AgentID: request.AgentID, AgentVersion: request.AgentVersion,
		ScopeID: request.ScopeID, CompiledBy: request.CompiledBy, CompiledAt: compiledAt,
		Resources: make([]domain.EffectiveResource, 0, len(order)),
	}
	for _, key := range order {
		resource, err := c.fold(ctx, groups[key], kindOf, compiledAt)
		if err != nil {
			return domain.EffectiveRelease{}, err
		}
		release.Resources = append(release.Resources, resource)
	}
	release.Digest = Digest(release)
	return release, nil
}

func (c *Compiler) fold(ctx context.Context, bindings []domain.ScopedBinding, kindOf map[string]string, compiledAt time.Time) (domain.EffectiveResource, error) {
	first := bindings[0]
	merger, err := c.Mergers.MergerFor(ctx, first.ResourceKind)
	if err != nil {
		return domain.EffectiveResource{}, err
	}
	resource := domain.EffectiveResource{ResourceKind: first.ResourceKind, ResourceID: first.ResourceID}
	var sealedBy *domain.ScopedBinding
	for index := range bindings {
		binding := bindings[index]
		if sealedBy != nil {
			return domain.EffectiveResource{}, fmt.Errorf("%w: scope %q cannot override %s %q sealed at scope %q",
				domain.ErrSealed, binding.ScopeID, binding.ResourceKind, binding.ResourceID, sealedBy.ScopeID)
		}
		merged, err := merger.Merge(ctx, resource.Value, binding)
		if err != nil {
			return domain.EffectiveResource{}, err
		}
		if err := c.Checker.CheckNarrowing(ctx, binding.ResourceKind, resource.Value, merged); err != nil {
			return domain.EffectiveResource{}, fmt.Errorf("scope %q: %w", binding.ScopeID, err)
		}
		if resource.Source.ScopeID != "" {
			resource.Superseded = append(resource.Superseded, resource.Source)
		}
		resource.Value = merged
		resource.Source = domain.Provenance{
			ScopeID: binding.ScopeID, ScopeKind: kindOf[binding.ScopeID], ResourceKind: binding.ResourceKind,
			ResourceID: binding.ResourceID, ResourceVersion: binding.ResourceVersion, MergeMode: binding.MergeMode,
			Sealed: binding.Sealed, Approver: binding.BoundBy, CompiledAt: compiledAt,
		}
		if binding.Sealed {
			sealedBy = &bindings[index]
		}
	}
	return resource, nil
}

func (c *Compiler) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

type resourceKey struct {
	kind domain.ResourceKind
	id   string
}

func groupBindings(bindings []domain.ScopedBinding, rank map[string]int) (map[resourceKey][]domain.ScopedBinding, []resourceKey, error) {
	groups := make(map[resourceKey][]domain.ScopedBinding)
	for _, binding := range bindings {
		if _, known := rank[binding.ScopeID]; !known {
			return nil, nil, fmt.Errorf("%w: binding scope %q is outside the resolved chain", domain.ErrValidation, binding.ScopeID)
		}
		key := resourceKey{kind: binding.ResourceKind, id: binding.ResourceID}
		groups[key] = append(groups[key], binding)
	}
	order := make([]resourceKey, 0, len(groups))
	for key, group := range groups {
		sort.SliceStable(group, func(i, j int) bool { return rank[group[i].ScopeID] < rank[group[j].ScopeID] })
		order = append(order, key)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].kind != order[j].kind {
			return order[i].kind < order[j].kind
		}
		return order[i].id < order[j].id
	})
	return groups, order, nil
}

var _ contract.EffectiveConfigCompiler = (*Compiler)(nil)
