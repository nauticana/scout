package charter

import (
	"context"
	"testing"

	"github.com/nauticana/charter/sdk/agent"
	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"
)

type identityProbe struct {
	agent.Provider
	asked model.Ref
}

func (p *identityProbe) Identity(_ context.Context, _ string, ref model.Ref) (model.AgentIdentity, error) {
	p.asked = ref
	return model.AgentIdentity{}, corpus.ErrNotFound
}

// An agent id that is not a Charter identifier resolves to the identity its export names.
func TestDeclaredDefinitionsLooksUpTheExportedIdentity(t *testing.T) {
	for _, agentID := range []string{"writer-a", "writer a/2"} {
		probe := &identityProbe{}
		_, _, _ = declaredDefinitions(context.Background(), probe, func(int64) string { return "acme" }, 7, agentID)
		if probe.asked.ID != charterAgentID(agentID) || probe.asked.Namespace != "acme" {
			t.Fatalf("%q looked up %+v, want the exported id %q", agentID, probe.asked, charterAgentID(agentID))
		}
	}
	if charterAgentID("writer a/2") == "writer a/2" {
		t.Fatal("the fixture needs an id that is not a Charter identifier")
	}
}
