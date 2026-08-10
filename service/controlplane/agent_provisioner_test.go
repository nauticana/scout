package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func provisionerFake() (*studioQueryFake, *AgentProvisioner) {
	qs := &studioQueryFake{rows: map[string][][]any{}, args: map[string][]any{}}
	return qs, &AgentProvisioner{DB: studioDBFake{qs: qs}}
}

func textSeed(agentID, kind string) domain.AgentSeed {
	return domain.AgentSeed{
		AgentID: agentID, AgentKind: kind, AliasID: kind, DisplayName: agentID, Enabled: true,
		Models: domain.AgentModelSelection{Text: &domain.ModelReference{ProviderID: "p", ModelID: "m"}},
	}
}

func TestProvisionSeedsTenantProfileDraftAndAlias(t *testing.T) {
	qs, provisioner := provisionerFake()
	err := provisioner.Provision(context.Background(), 3,
		domain.TenantIdentity{TenantKey: "partner-3", HomeRegion: "eu"},
		[]domain.AgentSeed{textSeed("Writer", "BL")})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	want := []string{qProvisionTenant, qProvisionProfile, qProvisionDraft, qProvisionAlias}
	if len(qs.queries) != len(want) {
		t.Fatalf("queries = %v, want %v", qs.queries, want)
	}
	for i, name := range want {
		if qs.queries[i] != name {
			t.Fatalf("query[%d] = %s, want %s", i, qs.queries[i], name)
		}
	}
	if qs.args[qProvisionAlias][1] != "BL" || qs.args[qProvisionAlias][3] != "Writer" {
		t.Fatalf("alias args = %v", qs.args[qProvisionAlias])
	}
}

// A seed without an alias registers the agent but claims no logical kind.
func TestProvisionWithoutAliasSkipsAlias(t *testing.T) {
	qs, provisioner := provisionerFake()
	seed := textSeed("Writer", "BL")
	seed.AliasID = ""
	if err := provisioner.Provision(context.Background(), 3, domain.TenantIdentity{}, []domain.AgentSeed{seed}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	for _, name := range qs.queries {
		if name == qProvisionAlias {
			t.Fatal("no alias must be written when the seed declares none")
		}
	}
}

func TestProvisionRejectsIncompleteSeeds(t *testing.T) {
	cases := map[string]func(*domain.AgentSeed){
		"missing agent id":     func(s *domain.AgentSeed) { s.AgentID = "" },
		"missing kind":         func(s *domain.AgentSeed) { s.AgentKind = "" },
		"missing display name": func(s *domain.AgentSeed) { s.DisplayName = "" },
		"missing text model":   func(s *domain.AgentSeed) { s.Models.Text = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			qs, provisioner := provisionerFake()
			seed := textSeed("Writer", "BL")
			mutate(&seed)
			err := provisioner.Provision(context.Background(), 3, domain.TenantIdentity{}, []domain.AgentSeed{seed})
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
			if len(qs.queries) != 0 {
				t.Fatalf("invalid seeds must be rejected before any write, got %v", qs.queries)
			}
		})
	}
}

func TestProvisionRequiresTenant(t *testing.T) {
	_, provisioner := provisionerFake()
	err := provisioner.Provision(context.Background(), 0, domain.TenantIdentity{}, nil)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}
