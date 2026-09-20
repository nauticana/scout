package toolgateway

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestReleaseGuardrailConfigsReadsThePinnedRelease(t *testing.T) {
	var gotTenant int64
	var gotAgent, gotVersion string
	resolver := &ReleaseGuardrailConfigs{Configs: &fake.GuardrailConfigRepository{
		GetFunc: func(_ context.Context, tenantID int64, agentID, agentVersion string) (domain.GuardrailConfig, error) {
			gotTenant, gotAgent, gotVersion = tenantID, agentID, agentVersion
			return domain.GuardrailConfig{Version: "g1"}, nil
		},
	}}
	call := domain.ToolCall{
		TenantContext: domain.TenantContext{TenantID: 7},
		Principal:     domain.Principal{Kind: domain.PrincipalAgent, ID: "helper", TenantID: 7, Release: "v3"},
	}
	config, err := resolver.GuardrailConfig(context.Background(), call)
	if err != nil || config.Version != "g1" || gotTenant != 7 || gotAgent != "helper" || gotVersion != "v3" {
		t.Fatalf("config=%+v err=%v lookup=(%d,%q,%q)", config, err, gotTenant, gotAgent, gotVersion)
	}
}

func TestReleaseGuardrailConfigsFailsClosed(t *testing.T) {
	resolver := &ReleaseGuardrailConfigs{Configs: &fake.GuardrailConfigRepository{
		GetFunc: func(context.Context, int64, string, string) (domain.GuardrailConfig, error) {
			t.Fatal("repository must not be read")
			return domain.GuardrailConfig{}, nil
		},
	}}
	tenant := domain.TenantContext{TenantID: 7}
	unpinned := domain.ToolCall{TenantContext: tenant, Principal: domain.Principal{Kind: domain.PrincipalAgent, ID: "helper", TenantID: 7}}
	if _, err := resolver.GuardrailConfig(context.Background(), unpinned); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unpinned agent: %v", err)
	}
	foreign := domain.ToolCall{TenantContext: tenant, Principal: domain.Principal{Kind: domain.PrincipalAgent, ID: "helper", TenantID: 8, Release: "v3"}}
	if _, err := resolver.GuardrailConfig(context.Background(), foreign); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("foreign tenant: %v", err)
	}
	human := domain.ToolCall{TenantContext: tenant, Principal: domain.Principal{Kind: domain.PrincipalHuman, ID: "42", TenantID: 7}}
	if config, err := resolver.GuardrailConfig(context.Background(), human); err != nil || config.Version != "" {
		t.Fatalf("human principal: %+v %v", config, err)
	}
}
