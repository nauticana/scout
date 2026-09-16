package charter

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	charterkeel "github.com/nauticana/charter/sdk/adapter/keel"
	"github.com/nauticana/charter/sdk/agent"
	"github.com/nauticana/charter/sdk/binding"
	"github.com/nauticana/charter/sdk/capability"
	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/identity"
	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/charter/sdk/organization"
	"github.com/nauticana/charter/sdk/process"
	"github.com/nauticana/charter/sdk/validate"
	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

const harborNS = "harbor.example"

func charterDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/nauticana/charter").Output()
	if err != nil {
		t.Fatalf("locate charter module: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func harbor(t *testing.T) *corpus.Corpus {
	t.Helper()
	c, err := corpus.NewDirLoader(filepath.Join(charterDir(t), "examples", "harbor-manufacturing", "instances")).Load()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func requestCtx() context.Context {
	return context.WithValue(context.Background(), common.RequestID, "req-0042")
}

type callers struct{}

func (callers) Caller(context.Context) (domain.TenantContext, domain.Principal, error) {
	return domain.TenantContext{TenantID: 7}, domain.Principal{Kind: domain.PrincipalAgent, ID: "agent-1", TenantID: 7}, nil
}

type gatewayFunc func(context.Context, domain.ToolCall) (domain.ToolResult, error)

func (f gatewayFunc) Invoke(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
	return f(ctx, call)
}

func TestDefinitionEmbedRoundTripsWithoutTouchingCharterFields(t *testing.T) {
	def := model.AgentDefinition{Purpose: "coordinate", Triggers: []string{"order-blocked"}, Policies: []string{"p1"}}
	def.ID, def.Namespace = "AGENTDEF-1", harborNS
	scout := domain.AgentDefinition{AgentID: "a1", Version: "3", Enabled: true, PublishedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}

	embedded, err := Embed(def, scout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ExtensionKey, "scout:") {
		t.Fatalf("extension key %q must be namespaced (CHR-CONF-002)", ExtensionKey)
	}
	charterOnly := embedded
	charterOnly.Extensions = nil
	if !reflect.DeepEqual(charterOnly, def) {
		t.Fatalf("Charter fields changed: %+v", charterOnly)
	}
	got, ok, err := Extract(embedded)
	if err != nil || !ok || !reflect.DeepEqual(got, scout) {
		t.Fatalf("extract = %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := Extract(def); ok {
		t.Fatal("a definition without the extension must report none")
	}
}

func TestDirectoryDescribesRuntimeAtAnInstant(t *testing.T) {
	c := harbor(t)
	d := &Directory{Agents: agent.NewBaseProvider(c), Identities: identity.NewBaseResolver(c)}
	ctx := context.Background()

	desc, err := d.Describe(ctx, harborNS, model.Ref{ID: "RT-OEC-PROD-01"}, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if desc.Identity.ID != "AGENT-ORDER-EXCEPTION-COORDINATOR" || desc.Definition.ID != "AGENTDEF-ORDER-EXCEPTION-COORDINATOR-1" {
		t.Fatalf("resolved %s / %s", desc.Identity.ID, desc.Definition.ID)
	}
	if _, err := d.Describe(ctx, harborNS, model.Ref{ID: "RT-OEC-PROD-01"}, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("an identity not yet active must fail closed")
	}
}

// clone adds a copy of a harbor document under a new id with the given fields changed.
func clone(t *testing.T, c *corpus.Corpus, id, newID string, set map[string]any) {
	t.Helper()
	doc, ok := c.Get(harborNS, id)
	if !ok {
		t.Fatalf("harbor document %s missing", id)
	}
	var body map[string]any
	if err := json.Unmarshal(doc.Raw, &body); err != nil {
		t.Fatal(err)
	}
	body["id"] = newID
	for k, v := range set {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	copied := *doc
	copied.ID, copied.Raw, copied.Value = newID, raw, body
	if err := c.Add(&copied); err != nil {
		t.Fatal(err)
	}
}

// permittedInstance is a consistent admitted case: harbor's agent only ever "supports", while every task it performs
// demands more, so an assignment permitting "executes" is cloned for the real "executes" instance.
func permittedInstance(t *testing.T, c *corpus.Corpus) string {
	clone(t, c, "ASGN-OEC-ORDER-EXCEPTION-SUPPORT", "ASGN-TEST-EXECUTES", map[string]any{"participation": model.ParticipationExecutes})
	clone(t, c, "TASKINST-OE-0042-EXECUTE", "TASKINST-TEST-EXECUTES", map[string]any{"assignmentId": "ASGN-TEST-EXECUTES"})
	return "TASKINST-TEST-EXECUTES"
}

func TestAdmissionEvaluatesOneAssignmentContext(t *testing.T) {
	c := harbor(t)
	admission := &Admission{
		Contexts: &process.BaseContextResolver{Process: process.NewBaseProvider(c), Assignments: organization.NewBaseProvider(c)},
		Agents:   agent.NewBaseProvider(c),
		Base:     &agent.BaseAdmission{Agents: agent.NewBaseProvider(c), Assignments: organization.NewBaseProvider(c)},
	}
	work := Work{Namespace: harborNS, Identity: model.Ref{ID: "AGENT-ORDER-EXCEPTION-COORDINATOR"}, Runtime: model.Ref{ID: "RT-OEC-PROD-01"},
		At: time.Date(2026, 6, 18, 17, 12, 30, 0, time.UTC)}

	// The instance's participation is what the work demands; an assignment that does not permit it is a refusal.
	work.TaskInstance = model.Ref{ID: "TASKINST-OE-0042-EXECUTE"}
	if _, decision := admission.Admit(requestCtx(), work); decision.Result != agent.Refused || decision.Requirement != "CHR-PROC-006" {
		t.Fatalf("over-asking instance = %+v", decision)
	}

	work.TaskInstance = model.Ref{ID: permittedInstance(t, c)}
	ec, decision := admission.Admit(requestCtx(), work)
	if decision.Result != agent.Admitted {
		t.Fatalf("decision = %+v", decision)
	}
	if ec.ExecutionContextID != "req-0042" || ec.Assignment.ID != "ASGN-TEST-EXECUTES" || ec.DefinitionVersion != "1" || ec.Participation != model.ParticipationExecutes {
		t.Fatalf("context = %+v", ec)
	}
	// Outside a correlated request there is no execution context id: fail closed, never invent one.
	if _, decision := admission.Admit(context.Background(), work); decision.Result != agent.Error {
		t.Fatalf("uncorrelated admission = %+v", decision)
	}
}

func TestToolVendorMapsOutcomesAndRefusals(t *testing.T) {
	cb := model.CapabilityBinding{CapabilityID: model.Ref{ID: "CAP-1"}}
	cb.ID, cb.Namespace = "BIND-1", harborNS
	key := corpus.KeyOf(harborNS, model.Ref{Namespace: harborNS, ID: "BIND-1"})
	var seen domain.ToolCall
	gateway := gatewayFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		seen = call
		return domain.ToolResult{Output: []byte(`{"outcome":"reserved","outputs":{"reservation":"R-1"},"external_reference":"S4-9"}`)}, nil
	})
	vendor := &ToolVendor{Binding: cb, Endpoint: &GatewayEndpoint{Gateway: gateway},
		Binder: &MappedBinder{Tools: map[corpus.DocumentKey]domain.ToolReference{key: {ToolID: "reserve", Version: "v1"}}, Callers: callers{}}}
	req := binding.Request{Capability: model.Ref{ID: "CAP-1"}, Inputs: map[string]any{"order": "O-1"}, IdempotencyKey: "idem-1"}

	payload, err := vendor.MapRequest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	result, err := vendor.Call(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if seen.ToolID != "reserve" || seen.ToolVersion != "v1" || seen.RequestID != "idem-1" || seen.TenantContext.TenantID != 7 || string(seen.Arguments) != `{"order":"O-1"}` {
		t.Fatalf("tool call = %+v", seen)
	}
	resp, err := vendor.MapResponse(context.Background(), result)
	if err != nil || resp.Outcome != "reserved" || resp.ExternalReference != "S4-9" || string(resp.Outputs.(json.RawMessage)) != `{"reservation":"R-1"}` {
		t.Fatalf("response = %+v, %v", resp, err)
	}

	// Governance refusals provably ran nothing; a transport failure leaves the outcome unknown.
	refusing := &GatewayEndpoint{Gateway: gatewayFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, domain.ErrForbidden
	})}
	if _, err := refusing.Call(context.Background(), payload); !errors.Is(err, binding.ErrNotExecuted) || !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("refusal = %v", err)
	}
	failing := &GatewayEndpoint{Gateway: gatewayFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, errors.New("timeout")
	})}
	if _, err := failing.Call(context.Background(), payload); errors.Is(err, binding.ErrNotExecuted) {
		t.Fatal("a transport failure must not claim nothing happened")
	}
	unmapped := &MappedBinder{Tools: nil, Callers: callers{}}
	if _, err := unmapped.Bind(context.Background(), cb, req); !errors.Is(err, binding.ErrNotExecuted) {
		t.Fatalf("unmapped binding = %v", err)
	}
}

func TestTransportRealizesTheBindingARequestNames(t *testing.T) {
	c := harbor(t)
	key := corpus.KeyOf(harborNS, model.Ref{Namespace: harborNS, ID: "BIND-S4-RESERVE-ORDER-STOCK-1"})
	calls := 0
	transport := &Transport{Namespace: harborNS, Bindings: binding.NewBaseProvider(c),
		Binder: &MappedBinder{Tools: map[corpus.DocumentKey]domain.ToolReference{key: {ToolID: "reserve", Version: "v1"}}, Callers: callers{}},
		Endpoint: &GatewayEndpoint{Gateway: gatewayFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			calls++
			return domain.ToolResult{Output: []byte(`{"outcome":"reserved"}`)}, nil
		})}}

	resp, err := transport.Execute(context.Background(), binding.Request{Capability: model.Ref{ID: "CAP-RESERVE-ORDER-STOCK"}, ContractVersion: "1"})
	if err != nil || resp.Outcome != "reserved" || calls != 1 {
		t.Fatalf("execute = %+v, %v, calls %d", resp, err, calls)
	}
	if _, err := transport.Execute(context.Background(), binding.Request{Capability: model.Ref{ID: "CAP-NOPE"}}); !errors.Is(err, binding.ErrNotExecuted) {
		t.Fatalf("unknown capability = %v", err)
	}
	if _, err := (&Transport{}).Execute(context.Background(), binding.Request{}); !errors.Is(err, binding.ErrNotExecuted) {
		t.Fatalf("uncomposed transport = %v", err)
	}
}

// A missing collaborator must read as "nothing ran", never as a panic or an unknown outcome.
func TestUncomposedCollaboratorsProvablyRunNothing(t *testing.T) {
	ctx := context.Background()
	if _, err := (&MappedBinder{}).Bind(ctx, model.CapabilityBinding{}, binding.Request{}); !errors.Is(err, binding.ErrNotExecuted) {
		t.Errorf("binder without a caller resolver = %v", err)
	}
	if _, err := (&GatewayEndpoint{}).Call(ctx, domain.ToolCall{}); !errors.Is(err, binding.ErrNotExecuted) {
		t.Errorf("endpoint without a gateway = %v", err)
	}
	if _, err := (&Adapter{}).Realize(ctx, nil, harborNS, model.Ref{}, nil); !errors.Is(err, binding.ErrNotExecuted) {
		t.Errorf("adapter without a binder = %v", err)
	}
	if err := (&TriggerHandler{}).Handle(ctx, binding.Delivery{}); err == nil {
		t.Error("handler without agents or dispatch must fail")
	}
}

type dispatches struct{ ids []string }

func (d *dispatches) Dispatch(_ context.Context, def model.AgentDefinition, _ binding.Delivery) error {
	d.ids = append(d.ids, def.ID)
	return nil
}

func TestEventsDeliverToDeclaringDefinitionsOnce(t *testing.T) {
	c := harbor(t)
	sink := &dispatches{}
	consumer, err := (Runtime{Documents: c, Dispatcher: sink}).Events()
	if err != nil {
		t.Fatal(err)
	}
	defs, _ := agent.NewBaseProvider(c).Definitions(context.Background())
	trigger := ""
	for _, def := range defs {
		if len(def.Triggers) > 0 {
			trigger = def.Triggers[0]
		}
	}
	if trigger == "" {
		t.Fatal("harbor declares no triggers")
	}
	handler := consumer.Handler
	if err := handler.Handle(context.Background(), binding.Delivery{Trigger: trigger}); err != nil || len(sink.ids) == 0 {
		t.Fatalf("handle = %v, dispatched %v", err, sink.ids)
	}
	if err := handler.Handle(context.Background(), binding.Delivery{Trigger: "no such trigger"}); err == nil {
		t.Fatal("an undeclared trigger must be refused")
	}
	at := time.Now()
	first, err := consumer.Deliver(context.Background(), harborNS, model.Ref{ID: "EVTBIND-S4-ORDER-BLOCKED-1"}, "evt-1", nil, at)
	second, _ := consumer.Deliver(context.Background(), harborNS, model.Ref{ID: "EVTBIND-S4-ORDER-BLOCKED-1"}, "evt-1", nil, at)
	if err != nil || first.Status != binding.DeliveryAccepted || second.Status != binding.DeliveryDuplicate {
		t.Fatalf("deliveries = %+v / %+v, %v", first, second, err)
	}
}

// The conformance claim is the proof: Charter's own manifest run against Scout's runtime and adapter subjects.
func TestClaimAgainstCharterManifest(t *testing.T) {
	adapter := &Adapter{Binder: anyTool{}}
	claim, err := Claim(filepath.Join(charterDir(t), "conformance"), Runtime{}, adapter, validate.ClaimSpec{
		Namespace: "scout.example", ID: "CLAIM-SCOUT", Name: "Scout",
		Profile:        "agent-runtime",
		Implementation: model.ObjectRef{Kind: model.KindEnterpriseSystem, ID: "github.com-nauticana-scout"}, ImplementationVersion: "test",
		ResultDate: model.Date(time.Now().UTC().Format("2006-01-02")),
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claim.Result != model.ResultConforming {
		raw, _ := json.MarshalIndent(claim, "", "  ")
		t.Fatalf("claim result %s:\n%s", claim.Result, raw)
	}
}

// anyTool binds every scenario binding: under conformance the vendor is scripted, so the tool identity is moot.
type anyTool struct{}

func (anyTool) Bind(_ context.Context, cb model.CapabilityBinding, req binding.Request) (domain.ToolCall, error) {
	arguments, err := json.Marshal(req.Inputs)
	if err != nil {
		return domain.ToolCall{}, err
	}
	return domain.ToolCall{ToolID: cb.ID, ToolVersion: cb.BindingVersion, RequestID: req.IdempotencyKey, Arguments: arguments}, nil
}

var _ ToolBinder = anyTool{}

type noDB struct{ keelport.DatabaseRepository }

// The composition root's own decisions: which ledger, and the fail-closed wiring checks.
func TestRuntimeComposition(t *testing.T) {
	c := harbor(t)
	transport := &Transport{Namespace: harborNS, Bindings: binding.NewBaseProvider(c), Binder: anyTool{}}

	explicit := capability.NewBaseMemoryLedger()
	for name, tc := range map[string]struct {
		runtime Runtime
		want    capability.Ledger
		durable bool
	}{
		"explicit wins":       {Runtime{Ledger: explicit, Database: noDB{}}, explicit, false},
		"database is durable": {Runtime{Database: noDB{}}, nil, true},
		"neither is memory":   {Runtime{}, nil, false},
	} {
		got := tc.runtime.ledger()
		_, isDurable := got.(*charterkeel.Ledger)
		switch {
		case tc.want != nil && got != tc.want:
			t.Errorf("%s: explicit ledger was replaced", name)
		case isDurable != tc.durable:
			t.Errorf("%s: durable = %v, want %v", name, isDurable, tc.durable)
		}
	}

	// keel RBAC is all three collaborators or none, and a runtime needs documents and a transport.
	for name, r := range map[string]Runtime{
		"no documents": {Transport: transport},
		"no transport": {Documents: c},
		"partial rbac": {Documents: c, Transport: transport, Keel: nil, Identities: charterkeel.BaseClaimIdentityMap{}},
	} {
		if _, err := r.Compose(context.Background(), nil, nil); err == nil {
			t.Errorf("%s: expected a composition error", name)
		}
	}
	if _, err := (Runtime{Documents: c, Transport: transport}).Compose(context.Background(), nil, nil); err != nil {
		t.Fatalf("minimal composition: %v", err)
	}
	if _, err := (Runtime{Documents: c}).Events(); err == nil {
		t.Error("events without a dispatcher must fail")
	}
}

func TestDirectoryFailsClosed(t *testing.T) {
	c := harbor(t)
	d := &Directory{Agents: agent.NewBaseProvider(c), Identities: identity.NewBaseResolver(c)}
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	if _, err := (&Directory{}).Describe(context.Background(), harborNS, model.Ref{ID: "RT-OEC-PROD-01"}, at); err == nil {
		t.Error("an uncomposed directory must fail")
	}
	if _, err := d.Describe(context.Background(), harborNS, model.Ref{ID: "RT-NOWHERE"}, at); err == nil {
		t.Error("an unknown runtime must fail")
	}
	// Charter v1.0.5 lets the definition declare its own version, so drift is now detectable here.
	clone(t, c, "RT-OEC-PROD-01", "RT-DRIFT", map[string]any{"definitionVersion": "2"})
	if _, err := d.Describe(context.Background(), harborNS, model.Ref{ID: "RT-DRIFT"}, at); err == nil {
		t.Error("a runtime operating a version its definition does not declare must fail")
	}
}
