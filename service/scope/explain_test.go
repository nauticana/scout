package scope

import (
	"context"
	"testing"

	"github.com/nauticana/scout/domain"
)

type releaseMap map[string]domain.EffectiveRelease

func (r releaseMap) Get(_ context.Context, _ int64, _, version string) (domain.EffectiveRelease, error) {
	release, found := r[version]
	if !found {
		return domain.EffectiveRelease{}, domain.ErrNotFound
	}
	return release, nil
}

func (r releaseMap) Put(context.Context, domain.EffectiveRelease) error { return nil }

func resourceOf(kind domain.ResourceKind, value, scope string, superseded ...string) domain.EffectiveResource {
	resource := domain.EffectiveResource{
		ResourceKind: kind, ResourceID: string(kind), Value: []byte(value),
		Source: domain.Provenance{ScopeID: scope, ResourceKind: kind},
	}
	for _, beaten := range superseded {
		resource.Superseded = append(resource.Superseded, domain.Provenance{ScopeID: beaten})
	}
	return resource
}

func TestExplainReportsTheWinnerAndWhatItBeat(t *testing.T) {
	explainer := &Explainer{Releases: releaseMap{"3": {Resources: []domain.EffectiveResource{
		resourceOf(domain.ResourceTool, `["search"]`, "agent", "tenant", "unit"),
	}}}}
	explanations, err := explainer.Explain(context.Background(), 7, "a", "3")
	if err != nil {
		t.Fatal(err)
	}
	if len(explanations) != 1 || explanations[0].Winner.ScopeID != "agent" {
		t.Fatalf("explanations = %+v", explanations)
	}
	if len(explanations[0].Superseded) != 2 {
		t.Fatalf("superseded = %+v, want the losing candidates kept", explanations[0].Superseded)
	}
}

func TestDiffClassifiesEveryChange(t *testing.T) {
	explainer := &Explainer{Releases: releaseMap{
		"3": {Resources: []domain.EffectiveResource{
			resourceOf(domain.ResourceTool, `["search","write"]`, "tenant"),
			resourceOf(domain.ResourceModel, `["opus"]`, "tenant"),
		}},
		"4": {Resources: []domain.EffectiveResource{
			resourceOf(domain.ResourceTool, `["search"]`, "agent"),
			resourceOf(domain.ResourceKnowledge, `["kb"]`, "agent"),
		}},
	}}
	diffs, err := explainer.Diff(context.Background(), 7, "a", "3", "4")
	if err != nil {
		t.Fatal(err)
	}
	changes := map[domain.ResourceKind]domain.ResourceChange{}
	for _, diff := range diffs {
		changes[diff.ResourceKind] = diff.Change
	}
	want := map[domain.ResourceKind]domain.ResourceChange{
		domain.ResourceTool:      domain.ResourceModified,
		domain.ResourceModel:     domain.ResourceRemoved,
		domain.ResourceKnowledge: domain.ResourceAdded,
	}
	for kind, change := range want {
		if changes[kind] != change {
			t.Fatalf("%s = %q, want %q", kind, changes[kind], change)
		}
	}
}

func TestDiffSkipsUnchangedResources(t *testing.T) {
	same := []domain.EffectiveResource{resourceOf(domain.ResourceTool, `["search"]`, "tenant")}
	explainer := &Explainer{Releases: releaseMap{"3": {Resources: same}, "4": {Resources: same}}}
	diffs, err := explainer.Diff(context.Background(), 7, "a", "3", "4")
	if err != nil || len(diffs) != 0 {
		t.Fatalf("diffs = %+v, error = %v", diffs, err)
	}
}
