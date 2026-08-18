package scope

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Explainer answers "why is this the value?" from the provenance frozen into a
// release. It never recompiles: the losing candidates were kept at publication
// precisely so the answer survives a later binding change.
type Explainer struct {
	Releases contract.EffectiveReleaseRepository
	Checker  *LatticeChecker
}

// Explain returns every effective resource with the binding that won and those it beat.
func (e *Explainer) Explain(ctx context.Context, tenantID int64, agentID, agentVersion string) ([]domain.ResourceExplanation, error) {
	release, err := e.release(ctx, tenantID, agentID, agentVersion)
	if err != nil {
		return nil, err
	}
	explanations := make([]domain.ResourceExplanation, 0, len(release.Resources))
	for _, resource := range release.Resources {
		explanations = append(explanations, domain.ResourceExplanation{
			ResourceKind: resource.ResourceKind, ResourceID: resource.ResourceID, Value: resource.Value,
			Winner: resource.Source, Superseded: resource.Superseded, Sealed: resource.Source.Sealed,
		})
	}
	return explanations, nil
}

// Diff compares two compiled releases of one agent, resource by resource.
func (e *Explainer) Diff(ctx context.Context, tenantID int64, agentID, fromVersion, toVersion string) ([]domain.ResourceDiff, error) {
	from, err := e.release(ctx, tenantID, agentID, fromVersion)
	if err != nil {
		return nil, err
	}
	to, err := e.release(ctx, tenantID, agentID, toVersion)
	if err != nil {
		return nil, err
	}
	before, after := index(from), index(to)
	keys := make([]resourceKey, 0, len(before)+len(after))
	for key := range before {
		keys = append(keys, key)
	}
	for key := range after {
		if _, both := before[key]; !both {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].kind != keys[j].kind {
			return keys[i].kind < keys[j].kind
		}
		return keys[i].id < keys[j].id
	})

	diffs := make([]domain.ResourceDiff, 0, len(keys))
	for _, key := range keys {
		old, existed := before[key]
		current, exists := after[key]
		switch {
		case existed && exists && bytes.Equal(old.Value, current.Value):
			continue
		case existed && !exists:
			diffs = append(diffs, domain.ResourceDiff{
				ResourceKind: key.kind, ResourceID: key.id, Change: domain.ResourceRemoved,
				From: old.Value, FromSource: old.Source,
			})
		case !existed && exists:
			diffs = append(diffs, domain.ResourceDiff{
				ResourceKind: key.kind, ResourceID: key.id, Change: domain.ResourceAdded,
				To: current.Value, ToSource: current.Source,
			})
		default:
			diffs = append(diffs, domain.ResourceDiff{
				ResourceKind: key.kind, ResourceID: key.id, Change: domain.ResourceModified,
				From: old.Value, To: current.Value, FromSource: old.Source, ToSource: current.Source,
			})
		}
	}
	return diffs, nil
}

func (e *Explainer) release(ctx context.Context, tenantID int64, agentID, version string) (domain.EffectiveRelease, error) {
	if e.Releases == nil {
		return domain.EffectiveRelease{}, fmt.Errorf("explainer: effective release repository is required")
	}
	return e.Releases.Get(ctx, tenantID, agentID, version)
}

func index(release domain.EffectiveRelease) map[resourceKey]domain.EffectiveResource {
	indexed := make(map[resourceKey]domain.EffectiveResource, len(release.Resources))
	for _, resource := range release.Resources {
		indexed[resourceKey{kind: resource.ResourceKind, id: resource.ResourceID}] = resource
	}
	return indexed
}

var _ contract.EffectiveConfigExplainer = (*Explainer)(nil)
