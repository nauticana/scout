package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nauticana/keel/config"
	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type restrictionBackend struct {
	overview domain.RestrictionOverview
	err      error
	calls    int
	actor    domain.StudioActor
	agentID  string
	layer    domain.RestrictionLayer
	expected string
}

func (backend *restrictionBackend) Restrictions(_ context.Context, actor domain.StudioActor, agentID string) (domain.RestrictionOverview, error) {
	backend.calls++
	backend.actor, backend.agentID = actor, agentID
	return backend.overview, backend.err
}

func (backend *restrictionBackend) ReplaceTenant(_ context.Context, actor domain.StudioActor, layer domain.RestrictionLayer, expectedDigest string) (domain.RestrictionLayer, error) {
	backend.calls++
	backend.actor, backend.layer, backend.expected = actor, layer, expectedDigest
	if backend.err != nil {
		return domain.RestrictionLayer{}, backend.err
	}
	layer.Digest = "next"
	return layer, nil
}

func newRestrictionHandler(backend *restrictionBackend) http.HandlerFunc {
	handler := &StudioRestrictionHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Backend: backend}
	return handler.Routes()[api.StudioRestrictionsPath]
}

// decodeRestrictionData reads the data member of keel's response envelope.
func decodeRestrictionData(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	envelope := struct {
		Data any `json:"data"`
	}{Data: target}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
}

var restrictionPerson = domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}

func TestStudioRestrictionsReadTheLayersAndAnAgentsOverrides(t *testing.T) {
	backend := &restrictionBackend{overview: domain.RestrictionOverview{
		Layers: domain.RestrictionLayers{
			Platform: domain.RestrictionLayer{Digest: "p1", Denials: []domain.PolicyStatement{{ID: "no-refunds", Effect: domain.PolicyDeny,
				Actions: []string{domain.PolicyActionToolInvoke}, Resources: []string{"payments.refund"}, Conditions: []byte(`{"shift":"night"}`)}}},
		},
		AgentID: "writer-a",
		Overrides: []domain.RestrictionOverride{{Layer: domain.RestrictionPlatform, Kind: domain.RestrictionDenial, RestrictionID: "no-refunds", Conditional: true,
			Settings: []domain.RestrictedSetting{{ResourceKind: domain.ResourceTool, ResourceID: "tools", Members: []string{"payments.refund"}}}}},
	}}
	recorder := httptest.NewRecorder()
	newRestrictionHandler(backend)(recorder, authenticatedRequest(http.MethodGet, api.StudioRestrictionsPath+"?agent_name=writer-a", nil))

	if recorder.Code != http.StatusOK || backend.actor != restrictionPerson || backend.agentID != "writer-a" {
		t.Fatalf("status = %d, actor = %+v, agent = %q, body = %s", recorder.Code, backend.actor, backend.agentID, recorder.Body.String())
	}
	var body api.StudioRestrictions
	decodeRestrictionData(t, recorder, &body)
	if len(body.Platform.Denials) != 1 || len(body.Overrides) != 1 {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	denial := body.Platform.Denials[0]
	if body.Platform.Digest != "p1" || denial.ID != "no-refunds" || string(denial.Conditions) != `{"shift":"night"}` || body.AgentName != "writer-a" ||
		len(body.Overrides) != 1 || body.Overrides[0].Layer != "platform" || body.Overrides[0].Kind != "denial" || !body.Overrides[0].Conditional ||
		body.Overrides[0].Settings[0].ResourceKind != "tool" || body.Overrides[0].Settings[0].Members[0] != "payments.refund" {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	if raw := recorder.Body.String(); !strings.Contains(raw, `"tenant":{"digest":"","denials":[],"guardrails":[]}`) || strings.Contains(raw, `"effect"`) {
		t.Fatalf("an unwritten layer is empty lists and a denial carries no effect: %s", raw)
	}
}

func TestStudioRestrictionsReplaceMapsTheWholeLayer(t *testing.T) {
	configureConversationRequestLimit(t)
	backend := &restrictionBackend{}
	recorder := httptest.NewRecorder()
	newRestrictionHandler(backend)(recorder, authenticatedRequest(http.MethodPost, api.StudioRestrictionsPath, []byte(`{
		"expected_digest":"d1",
		"denials":[{"id":"no-refunds","actions":["tool:invoke"],"resources":["payments.refund"],"conditions":null,"reason":"finance freeze"}],
		"guardrails":[{"id":"only-search","kind":"tool_allowlist","stages":["tool_input"],"action":"block","severity":"hard","params":{"tools":["search"]}}]
	}`)))

	if recorder.Code != http.StatusOK || backend.actor != restrictionPerson || backend.expected != "d1" {
		t.Fatalf("status = %d, actor = %+v, expected = %q, body = %s", recorder.Code, backend.actor, backend.expected, recorder.Body.String())
	}
	denial, rule := backend.layer.Denials[0], backend.layer.Guardrails[0]
	if denial.Effect != domain.PolicyDeny || denial.Conditions != nil || denial.Reason != "finance freeze" || denial.Resources[0] != "payments.refund" {
		t.Fatalf("a restriction denial always denies and a null condition is none: %+v", denial)
	}
	if rule.Kind != domain.GuardrailKindToolAllowlist || rule.Stages[0] != domain.GuardrailStageToolInput || rule.Action != domain.GuardrailActionBlock ||
		string(rule.Params) != `{"tools":["search"]}` {
		t.Fatalf("rule = %+v", rule)
	}
	var written api.RestrictionLayer
	decodeRestrictionData(t, recorder, &written)
	if written.Digest != "next" || len(written.Guardrails) != 1 || written.Guardrails[0].ID != "only-search" || written.Denials[0].Reason != "finance freeze" {
		t.Fatalf("written = %s", recorder.Body.String())
	}
}

func TestStudioRestrictionsMapRefusals(t *testing.T) {
	configureConversationRequestLimit(t)
	for name, test := range map[string]struct {
		method string
		err    error
		status int
	}{
		"stale digest":   {http.MethodPost, fmt.Errorf("%w: restriction layer is at %q", domain.ErrConflict, "d2"), http.StatusConflict},
		"not restricted": {http.MethodPost, fmt.Errorf("%w: restriction statement %q must deny", domain.ErrValidation, "a"), http.StatusBadRequest},
		"no grant":       {http.MethodGet, fmt.Errorf("%w: AGENT_RESTRICTION VIEW on tenant", domain.ErrForbidden), http.StatusForbidden},
		"unknown agent":  {http.MethodGet, fmt.Errorf("%w: agent", domain.ErrNotFound), http.StatusNotFound},
	} {
		backend := &restrictionBackend{err: test.err}
		recorder := httptest.NewRecorder()
		newRestrictionHandler(backend)(recorder, authenticatedRequest(test.method, api.StudioRestrictionsPath, []byte(`{"expected_digest":"d1"}`)))
		if recorder.Code != test.status || backend.calls != 1 {
			t.Fatalf("%s: status = %d, calls = %d, body = %s", name, recorder.Code, backend.calls, recorder.Body.String())
		}
	}

	backend := &restrictionBackend{}
	recorder := httptest.NewRecorder()
	newRestrictionHandler(backend)(recorder, authenticatedRequest(http.MethodPut, api.StudioRestrictionsPath, []byte(`{}`)))
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "GET, POST" || backend.calls != 0 {
		t.Fatalf("PUT: status = %d, allow = %q", recorder.Code, recorder.Header().Get("Allow"))
	}
}

func TestStudioRestrictionsBoundTheReplaceBody(t *testing.T) {
	previous := config.Config()
	config.SetConfig(&config.KeelConfig{MaxRequestSize: 64})
	t.Cleanup(func() { config.SetConfig(previous) })
	backend := &restrictionBackend{}
	recorder := httptest.NewRecorder()
	body := `{"expected_digest":"","denials":[{"id":"` + strings.Repeat("a", 128) + `","actions":["*"],"resources":["*"]}]}`
	newRestrictionHandler(backend)(recorder, authenticatedRequest(http.MethodPost, api.StudioRestrictionsPath, []byte(body)))
	if recorder.Code != http.StatusBadRequest || backend.calls != 0 {
		t.Fatalf("an oversized body: status = %d, calls = %d", recorder.Code, backend.calls)
	}
}

func TestStudioRestrictionsFeatureNamesItsAuthorizationObject(t *testing.T) {
	feature := (&StudioRestrictionHandler{}).Feature()
	if feature.ID != StudioFeatureRestrictions || feature.Object != "AGENT_RESTRICTION" || strings.Join(feature.Actions, ",") != "VIEW,REPLACE_TENANT" {
		t.Fatalf("feature = %+v", feature)
	}
	if routes := (&StudioRestrictionHandler{}).Routes(); len(routes) != 1 || routes[api.StudioRestrictionsPath] == nil {
		t.Fatalf("routes = %v", routes)
	}
}
