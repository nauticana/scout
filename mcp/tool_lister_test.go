package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	"github.com/nauticana/keel/oauth/connect"

	"github.com/nauticana/scout/domain"
)

type tenantsFake struct {
	tenants []Tenant
	err     error
}

func (f tenantsFake) Tenants(context.Context, domain.MCPCaller) ([]Tenant, error) {
	return f.tenants, f.err
}

type grantsFake struct {
	byRole map[string][][3]string
	reads  int
	err    error
}

func (f *grantsFake) ActionGrants(_ context.Context, principal keelmodel.Principal) (keelmodel.GrantSet, error) {
	f.reads++
	var set keelmodel.GrantSet
	for _, grant := range f.byRole[fmt.Sprint(principal.ID)] {
		set.Add(grant[0], grant[1], grant[2], false)
	}
	return set, f.err
}

type sourcesFake struct {
	byTenant map[int64]connect.SourceReadiness
	reads    int
}

func (f *sourcesFake) Resolve(_ context.Context, partnerID int64) (connect.SourceReadiness, error) {
	f.reads++
	return f.byTenant[partnerID], nil
}

func listedNames(definitions []domain.MCPToolDefinition) []string {
	names := make([]string, len(definitions))
	for i, definition := range definitions {
		names[i] = definition.Name
	}
	slices.Sort(names)
	return names
}

func tenantTool(name string, policy domain.MCPToolPolicy) domain.MCPToolDefinition {
	policy.Tenant = true
	return domain.MCPToolDefinition{Name: name, Policy: policy}
}

var remoteCaller = domain.MCPCaller{Authenticated: true, ActorID: 5}

func TestToolListerAdmitsAToolSomeTenantAllows(t *testing.T) {
	read := domain.MCPGrant{Object: "REPORT", Action: "READ", Scope: "seo"}
	publish := domain.MCPGrant{Object: "CONTENT", Action: "PUBLISH", Scope: "seo"}
	billing := domain.MCPGrant{Object: "BILLING", Action: "VIEW", Scope: "billing"}
	definitions := []domain.MCPToolDefinition{
		{Name: "list_tenants"},
		tenantTool("read_report", domain.MCPToolPolicy{Grants: []domain.MCPGrant{read}}),
		tenantTool("publish", domain.MCPToolPolicy{ActionLevel: 3, Grants: []domain.MCPGrant{read, publish}}),
		tenantTool("agency_book", domain.MCPToolPolicy{OwnTenantOnly: true}),
		tenantTool("ads", domain.MCPToolPolicy{Sources: []string{"ads"}}),
		tenantTool("decide", domain.MCPToolPolicy{AnyGrant: true, Grants: []domain.MCPGrant{billing, publish}}),
		tenantTool("invoices", domain.MCPToolPolicy{Grants: []domain.MCPGrant{billing}}),
	}
	grants := &grantsFake{byRole: map[string][][3]string{
		"5":        {{"REPORT", "READ", "seo"}, {"BILLING", "VIEW", "billing"}},
		"DELEGATE": {{"REPORT", "READ", "seo"}, {"CONTENT", "PUBLISH", "seo"}, {"BILLING", "VIEW", "billing"}},
	}}
	sources := &sourcesFake{byTenant: map[int64]connect.SourceReadiness{
		1: {"ads": connect.SourceNotConnected},
		2: {"ads": connect.SourceReady},
	}}
	cases := map[string]struct {
		tenants []Tenant
		want    []string
	}{
		"own tenant without publish grant or ready ads": {
			tenants: []Tenant{{ID: 1, Principal: keelmodel.UserPrincipal(5), ActionLevel: 3}},
			want:    []string{"agency_book", "decide", "invoices", "list_tenants", "read_report"},
		},
		"delegated tenant: no own-only tool, billing never delegated": {
			tenants: []Tenant{{ID: 2, Principal: keelmodel.RolePrincipal("DELEGATE"), Delegated: true, ActionLevel: 3}},
			want:    []string{"ads", "decide", "list_tenants", "publish", "read_report"},
		},
		"delegated view level": {
			tenants: []Tenant{{ID: 2, Principal: keelmodel.RolePrincipal("DELEGATE"), Delegated: true, ActionLevel: 0}},
			want:    []string{"ads", "decide", "list_tenants", "read_report"},
		},
		"no tenant": {want: []string{"list_tenants"}},
	}
	for name, c := range cases {
		lister := ToolLister{Tenants: tenantsFake{tenants: c.tenants}, Grants: grants, Sources: sources,
			Delegable: func(scope string) bool { return scope != "billing" }}
		listed, err := lister.List(context.Background(), remoteCaller, definitions)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := listedNames(listed); !slices.Equal(got, c.want) {
			t.Errorf("%s: listed %v, want %v", name, got, c.want)
		}
	}
}

func TestToolListerReadsEachPrincipalAndTenantOnce(t *testing.T) {
	grants := &grantsFake{byRole: map[string][][3]string{"5": {{"REPORT", "READ", "seo"}}}}
	sources := &sourcesFake{}
	tenants := []Tenant{
		{ID: 1, Principal: keelmodel.UserPrincipal(5)},
		{ID: 2, Principal: keelmodel.UserPrincipal(5)},
		{ID: 3, Principal: keelmodel.RolePrincipal("DELEGATE"), Delegated: true},
	}
	definitions := []domain.MCPToolDefinition{tenantTool("ads", domain.MCPToolPolicy{Sources: []string{"ads"}})}
	lister := ToolLister{Tenants: tenantsFake{tenants: tenants}, Grants: grants, Sources: sources}
	if _, err := lister.List(context.Background(), remoteCaller, definitions); err != nil {
		t.Fatal(err)
	}
	if grants.reads != 2 || sources.reads != 3 {
		t.Fatalf("grant reads %d, source reads %d; want 2 and 3", grants.reads, sources.reads)
	}
	sources.reads = 0
	plain := []domain.MCPToolDefinition{tenantTool("read", domain.MCPToolPolicy{})}
	if _, err := lister.List(context.Background(), remoteCaller, plain); err != nil || sources.reads != 0 {
		t.Fatalf("sources read %d times for tools that need none, err %v", sources.reads, err)
	}
}

func TestToolListerFallsBackToTheCatalog(t *testing.T) {
	definitions := []domain.MCPToolDefinition{
		tenantTool("read", domain.MCPToolPolicy{Grants: []domain.MCPGrant{{Object: "R", Action: "READ", Scope: "s"}}}),
	}
	broken := errors.New("database down")
	cases := map[string]ToolLister{
		"tenants fail":   {Tenants: tenantsFake{err: broken}, Grants: &grantsFake{}},
		"grants fail":    {Tenants: tenantsFake{tenants: []Tenant{{ID: 1}}}, Grants: &grantsFake{err: broken}},
		"not configured": {},
	}
	for name, lister := range cases {
		listed, err := lister.List(context.Background(), remoteCaller, definitions)
		if err == nil || len(listed) != 1 {
			t.Errorf("%s: listed %d with err %v; want the catalog and an error", name, len(listed), err)
		}
	}
	if _, err := (ToolLister{}).List(context.Background(), remoteCaller, nil); err != nil {
		t.Fatalf("a catalog without tenant tools needs no lookups: %v", err)
	}
	listed, err := (ToolLister{}).List(context.Background(), HostCaller(), definitions)
	if err != nil || len(listed) != 1 {
		t.Fatalf("host caller listed %d, err %v", len(listed), err)
	}
}
