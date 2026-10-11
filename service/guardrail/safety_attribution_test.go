package guardrail

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

// A safety event names the principal, the agent whose release ran, and the request, so
// Studio can page an agent's hits; before, the enforcer dropped the principal.
func TestSafetyEventsNameThePrincipalAgentAndRequest(t *testing.T) {
	h := newHarness(t, EnforcerConfig{})
	call := domain.ToolCall{
		TenantContext: domain.TenantContext{TenantID: 7}, RequestID: "r-9", ConversationID: "c-1", ToolID: "search",
		Principal: domain.Principal{Kind: domain.PrincipalAgent, ID: "writer-a", TenantID: 7, Release: "3"}, Arguments: []byte(`"TOPSECRET"`),
	}
	if _, err := h.enforcer.BeforeTool(context.Background(), rules(t), call); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v", err)
	}
	if len(h.events.Events) != 1 {
		t.Fatalf("events = %+v", h.events.Events)
	}
	event := h.events.Events[0]
	if event.AgentID != "writer-a" || event.RequestID != "r-9" || event.ReleaseVersion != "3" ||
		event.Principal != (domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer-a"}) {
		t.Fatalf("event = %+v", event)
	}

	person := domain.GuardrailSubject{TenantID: 7, RequestID: "r-10", Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}
	if _, err := h.enforcer.AfterModelChunk(context.Background(), rules(t), person, domain.ModelChunk{Payload: []byte("TOPSECRET")}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v", err)
	}
	if event = h.events.Events[1]; event.AgentID != "" || event.Principal.ID != "42" || event.RequestID != "r-10" {
		t.Fatalf("a person's hit belongs to no agent: %+v", event)
	}
}
