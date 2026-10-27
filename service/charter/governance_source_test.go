package charter

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/charter/sdk/agent"
	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
)

const (
	oecAgent      = "AGENT-ORDER-EXCEPTION-COORDINATOR"
	oecDefinition = "AGENTDEF-ORDER-EXCEPTION-COORDINATOR-1"
	otherNS       = "other.example"
)

var governanceNow = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// governedCorpus is harbor without its coordinator definition, plus one copy of that definition per
// override, each override naming at least the copy's id.
func governedCorpus(t *testing.T, overrides ...map[string]any) *corpus.Corpus {
	t.Helper()
	source := harbor(t)
	out := corpus.New()
	for _, doc := range source.Documents() {
		if doc.Namespace == harborNS && doc.ID == oecDefinition {
			continue
		}
		if err := out.Add(doc); err != nil {
			t.Fatal(err)
		}
	}
	original, _ := source.Get(harborNS, oecDefinition)
	for _, override := range overrides {
		addVariant(t, out, original, override)
	}
	return out
}

// addVariant adds a copy of doc with the given members replaced.
func addVariant(t *testing.T, c *corpus.Corpus, doc *model.Document, set map[string]any) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(doc.Raw, &body); err != nil {
		t.Fatal(err)
	}
	maps.Copy(body, set)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := corpus.Parser{}.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Add(parsed); err != nil {
		t.Fatal(err)
	}
}

func harborGovernance(c corpus.Source) *GovernanceSource {
	return &GovernanceSource{
		Documents: c,
		Namespace: func(tenantID int64) string {
			switch tenantID {
			case 7:
				return harborNS
			case 8:
				return otherNS
			}
			return ""
		},
		Link: func(definition model.AgentDefinition) string {
			return "https://charter.example/" + definition.Namespace + "/" + definition.ID
		},
		Now: func() time.Time { return governanceNow },
	}
}

func TestGovernanceSourceSuppliesTheCharterDefinitionInForce(t *testing.T) {
	c := harbor(t)
	source := harborGovernance(c)
	snapshot, ok, err := source.Snapshot(context.Background(), 7, oecAgent)
	if err != nil || !ok {
		t.Fatalf("snapshot = %v, %v", ok, err)
	}
	if snapshot.Document != (domain.CharterObjectRef{Kind: "AgentDefinition", ID: oecDefinition}) || snapshot.Version != "1" {
		t.Fatalf("governing document %+v version %q", snapshot.Document, snapshot.Version)
	}
	if snapshot.Link != "https://charter.example/harbor.example/"+oecDefinition {
		t.Fatalf("link %q", snapshot.Link)
	}
	if !strings.HasPrefix(snapshot.Digest, "sha256:") || len(snapshot.Digest) != len("sha256:")+64 {
		t.Fatalf("digest %q", snapshot.Digest)
	}
	doc, _ := c.Get(harborNS, oecDefinition)
	var want domain.AgentGovernance
	if err = json.Unmarshal(doc.Raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Governance, want) {
		t.Fatalf("governance differs from the Charter document:\n got %+v\nwant %+v", snapshot.Governance, want)
	}
	if snapshot.Governance.TriggerChannels != nil || snapshot.Governance.Enterprise != nil {
		t.Fatalf("a snapshot carries Charter fields only: %+v", snapshot.Governance)
	}
	again, _, err := source.Snapshot(context.Background(), 7, oecAgent)
	if err != nil || again.Digest != snapshot.Digest {
		t.Fatalf("the digest of unchanged governance moved: %q then %q (%v)", snapshot.Digest, again.Digest, err)
	}
	source.Link = nil
	if unlinked, _, err := source.Snapshot(context.Background(), 7, oecAgent); err != nil || unlinked.Link != "" {
		t.Fatalf("no link function names no link: %q, %v", unlinked.Link, err)
	}
}

func TestGovernanceSourceChoosesTheOneDefinitionInForce(t *testing.T) {
	v1 := map[string]any{"id": oecDefinition}
	v2 := map[string]any{"id": "AGENTDEF-ORDER-EXCEPTION-COORDINATOR-2", "definitionVersion": "2", "purpose": "Resolve blocked customer orders"}
	with := func(base map[string]any, set map[string]any) map[string]any {
		out := map[string]any{}
		maps.Copy(out, base)
		maps.Copy(out, set)
		return out
	}
	original, _, err := harborGovernance(harbor(t)).Snapshot(context.Background(), 7, oecAgent)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		defs    []map[string]any
		at      time.Time
		version string
		err     error
	}{
		{"a proposed successor does not govern", []map[string]any{v1, with(v2, map[string]any{"lifecycleState": "proposed"})}, governanceNow, "1", nil},
		{"a retired predecessor does not govern", []map[string]any{with(v1, map[string]any{"lifecycleState": "retired"}), with(v2, map[string]any{"lifecycleState": "active"})}, governanceNow, "2", nil},
		{"validity hands over", []map[string]any{
			with(v1, map[string]any{"validity": map[string]any{"from": "2026-01-01", "to": "2026-03-31"}}),
			with(v2, map[string]any{"validity": map[string]any{"from": "2026-04-01"}}),
		}, governanceNow, "2", nil},
		{"before the handover", []map[string]any{
			with(v1, map[string]any{"validity": map[string]any{"from": "2026-01-01", "to": "2026-03-31"}}),
			with(v2, map[string]any{"validity": map[string]any{"from": "2026-04-01"}}),
		}, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), "1", nil},
		{"two versions in force are ambiguous", []map[string]any{v1, v2}, governanceNow, "", domain.ErrConflict},
		{"one version declared twice is ambiguous", []map[string]any{v1, with(v1, map[string]any{"id": "AGENTDEF-COPY"})}, governanceNow, "", domain.ErrConflict},
		{"nothing in force", []map[string]any{with(v1, map[string]any{"lifecycleState": "suspended"})}, governanceNow, "", domain.ErrValidation},
		{"nothing declared", nil, governanceNow, "", domain.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := harborGovernance(governedCorpus(t, tc.defs...))
			at := tc.at
			source.Now = func() time.Time { return at }
			snapshot, ok, err := source.Snapshot(context.Background(), 7, oecAgent)
			if tc.err != nil {
				if !errors.Is(err, tc.err) || ok {
					t.Fatalf("want %v and no snapshot, got %v, %v", tc.err, ok, err)
				}
				return
			}
			if err != nil || !ok || snapshot.Version != tc.version {
				t.Fatalf("want version %s, got %q, %v, %v", tc.version, snapshot.Version, ok, err)
			}
			if tc.version == "2" && (snapshot.Governance.Purpose != "Resolve blocked customer orders" || snapshot.Digest == original.Digest) {
				t.Fatalf("version 2 must supply its own values and digest: %q %q", snapshot.Governance.Purpose, snapshot.Digest)
			}
		})
	}
}

func TestGovernanceSourceFailsClosed(t *testing.T) {
	c := harbor(t)
	ctx := context.Background()
	cases := []struct {
		name    string
		source  *GovernanceSource
		tenant  int64
		agentID string
		want    error
	}{
		{"no documents", &GovernanceSource{Namespace: harborGovernance(c).Namespace}, 7, oecAgent, domain.ErrNotReady},
		{"no namespace mapping", &GovernanceSource{Documents: c}, 7, oecAgent, domain.ErrNotReady},
		{"an unmapped tenant", harborGovernance(c), 9, oecAgent, domain.ErrNotReady},
		{"an unknown agent", harborGovernance(c), 7, "AGENT-NOWHERE", domain.ErrValidation},
		{"a blank agent id", harborGovernance(c), 7, "", domain.ErrValidation},
		{"an id naming another kind", harborGovernance(c), 7, "POS-SALES-OPERATIONS-MANAGER", domain.ErrValidation},
	}
	for _, tc := range cases {
		snapshot, ok, err := tc.source.Snapshot(ctx, tc.tenant, tc.agentID)
		if !errors.Is(err, tc.want) || ok || !reflect.DeepEqual(snapshot, domain.GovernanceSnapshot{}) {
			t.Errorf("%s: want %v and nothing, got %+v, %v, %v", tc.name, tc.want, snapshot, ok, err)
		}
	}
	failing := &GovernanceSource{Documents: failingSource{}, Namespace: harborGovernance(c).Namespace}
	if _, ok, err := failing.Snapshot(ctx, 7, oecAgent); err == nil || ok || errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a store failure is an error, not a refusal: %v, %v", ok, err)
	}
}

// Only the tenant's own namespace speaks for its agents: another namespace's definition naming the
// identity explicitly neither governs it, makes it ambiguous, nor declares a version for it.
func TestGovernanceSourceKeepsToTheTenantsNamespace(t *testing.T) {
	c := harbor(t)
	identityDoc, _ := c.Get(harborNS, oecAgent)
	definitionDoc, _ := c.Get(harborNS, oecDefinition)
	addVariant(t, c, definitionDoc, map[string]any{
		"namespace": otherNS, "id": "AGENTDEF-FOREIGN", "definitionVersion": "9", "purpose": "Act for another tenant",
		"agentIdentityId": map[string]any{"namespace": harborNS, "id": oecAgent},
	})
	ctx := context.Background()
	source := harborGovernance(c)
	snapshot, _, err := source.Snapshot(ctx, 7, oecAgent)
	if err != nil || snapshot.Version != "1" || snapshot.Document.ID != oecDefinition {
		t.Fatalf("another namespace's definition reached tenant 7: %+v, %v", snapshot, err)
	}
	binding := &IdentityBinding{Agents: agent.NewBaseProvider(c), Namespace: source.Namespace}
	if err = binding.WriteRelease(ctx, nil, 7, domain.AgentDefinition{AgentID: oecAgent, Version: "9"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("another namespace declared a version for tenant 7: %v", err)
	}
	if _, _, err = source.Snapshot(ctx, 8, oecAgent); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("tenant 8 has no identity of its own yet: %v", err)
	}

	addVariant(t, c, identityDoc, map[string]any{"namespace": otherNS})
	addVariant(t, c, definitionDoc, map[string]any{"namespace": otherNS, "id": "AGENTDEF-OTHER", "definitionVersion": "3", "purpose": "Serve the other tenant"})
	other, _, err := source.Snapshot(ctx, 8, oecAgent)
	if err != nil || other.Document.ID != "AGENTDEF-OTHER" || other.Governance.Purpose != "Serve the other tenant" {
		t.Fatalf("tenant 8 must read its own namespace: %+v, %v", other, err)
	}
	if mine, _, err := source.Snapshot(ctx, 7, oecAgent); err != nil || mine.Document.ID != oecDefinition {
		t.Fatalf("tenant 7 must keep its own definition: %+v, %v", mine, err)
	}
}

// Studio's governance guard accepts a release frozen from the source's snapshot, and refuses it once
// another definition comes into force.
func TestStudioGovernanceAcceptsTheCharterSnapshot(t *testing.T) {
	c := governedCorpus(t,
		map[string]any{"id": oecDefinition, "validity": map[string]any{"from": "2026-01-01", "to": "2026-06-30"}},
		map[string]any{"id": "AGENTDEF-ORDER-EXCEPTION-COORDINATOR-2", "definitionVersion": "2", "validity": map[string]any{"from": "2026-07-01"}},
	)
	source := harborGovernance(c)
	ctx := context.Background()
	snapshot, _, err := source.Snapshot(ctx, 7, oecAgent)
	if err != nil {
		t.Fatal(err)
	}
	frozen := snapshot.Governance
	frozen.Enterprise = &domain.GovernanceSnapshotRef{
		Document: snapshot.Document, Version: snapshot.Version, Digest: snapshot.Digest, Fields: domain.CharterGovernanceFields(),
	}
	raw, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	candidate := domain.PublishCandidate{
		Actor:      domain.StudioActor{TenantID: 7},
		Definition: domain.AgentDefinition{AgentID: oecAgent, Sections: map[string]json.RawMessage{domain.GovernanceSection: raw}},
	}
	guard := &controlplane.StudioGovernance{Source: source}
	if err = guard.CheckPublish(ctx, candidate); err != nil {
		t.Fatalf("the snapshot's own governance must pass the guard: %v", err)
	}
	source.Now = func() time.Time { return time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC) }
	if err = guard.CheckPublish(ctx, candidate); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a release frozen from version 1 after version 2 took over: want ErrConflict, got %v", err)
	}
}

// A malformed definition stays the problem of the namespace that owns it.
func TestGovernanceSourceDecodesOnlyTheTenantsDefinitions(t *testing.T) {
	c := harbor(t)
	definitionDoc, _ := c.Get(harborNS, oecDefinition)
	addVariant(t, c, definitionDoc, map[string]any{"namespace": otherNS, "id": "AGENTDEF-MALFORMED", "definitionVersion": 2, "triggers": "x"})
	ctx := context.Background()
	source := harborGovernance(c)
	if snapshot, ok, err := source.Snapshot(ctx, 7, oecAgent); err != nil || !ok || snapshot.Version != "1" {
		t.Fatalf("another namespace's malformed definition failed tenant 7: %+v, %v, %v", snapshot, ok, err)
	}
	binding := &IdentityBinding{Agents: agent.NewBaseProvider(c), Namespace: source.Namespace}
	if err := binding.WriteRelease(ctx, nil, 7, domain.AgentDefinition{AgentID: oecAgent, Version: "1"}); err != nil {
		t.Fatalf("another namespace's malformed definition failed tenant 7's binding: %v", err)
	}
	addVariant(t, c, definitionDoc, map[string]any{"id": "AGENTDEF-MALFORMED-HERE", "definitionVersion": 2})
	if _, ok, err := source.Snapshot(ctx, 7, oecAgent); err == nil || ok || errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a malformed definition in the tenant's namespace is a store error: %v, %v", ok, err)
	}
}

// In-force status is decided on the UTC calendar date, whatever zone the clock reports.
func TestGovernanceSourceReadsValidityInUTC(t *testing.T) {
	c := governedCorpus(t,
		map[string]any{"id": oecDefinition, "validity": map[string]any{"from": "2026-01-01", "to": "2026-06-30"}},
		map[string]any{"id": "AGENTDEF-ORDER-EXCEPTION-COORDINATOR-2", "definitionVersion": "2", "validity": map[string]any{"from": "2026-07-01"}},
	)
	source := harborGovernance(c)
	eastern := time.FixedZone("UTC-5", -5*60*60)
	source.Now = func() time.Time { return time.Date(2026, 6, 30, 22, 0, 0, 0, eastern) }
	snapshot, _, err := source.Snapshot(context.Background(), 7, oecAgent)
	if err != nil || snapshot.Version != "2" {
		t.Fatalf("2026-07-01 03:00 UTC is in version 2's validity: %q, %v", snapshot.Version, err)
	}
	source.Now = func() time.Time { return time.Date(2026, 7, 1, 1, 0, 0, 0, time.FixedZone("UTC+3", 3*60*60)) }
	if snapshot, _, err = source.Snapshot(context.Background(), 7, oecAgent); err != nil || snapshot.Version != "1" {
		t.Fatalf("2026-06-30 22:00 UTC is in version 1's validity: %q, %v", snapshot.Version, err)
	}
}

type emptyGovernanceDB struct{ keelport.DatabaseRepository }

func (emptyGovernanceDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return emptyQueries{}
}

type emptyQueries struct{}

func (emptyQueries) Query(context.Context, string, ...any) (*keelmodel.QueryResult, error) {
	return &keelmodel.QueryResult{}, nil
}

func (emptyQueries) GenID() int64 { return 0 }

// The release a publish binds to a Charter definition carries that definition's governance: Section and
// the guard freeze the definition in force, and IdentityBinding refuses a version that definition does not declare.
func TestPublishBindsTheDefinitionItsGovernanceFroze(t *testing.T) {
	v2 := "AGENTDEF-ORDER-EXCEPTION-COORDINATOR-2"
	ctx := context.Background()
	publish := func(t *testing.T, c *corpus.Corpus, version string) error {
		t.Helper()
		source := harborGovernance(c)
		governance := &controlplane.StudioGovernance{DB: emptyGovernanceDB{}, Source: source}
		raw, err := governance.Section(ctx, 7, domain.AgentDraft{AgentID: oecAgent})
		if err != nil {
			t.Fatal(err)
		}
		definition := domain.AgentDefinition{AgentID: oecAgent, Sections: map[string]json.RawMessage{domain.GovernanceSection: raw}}
		if err = governance.CheckPublish(ctx, domain.PublishCandidate{Actor: domain.StudioActor{TenantID: 7}, Definition: definition}); err != nil {
			t.Fatalf("guard: %v", err)
		}
		definition.Version = version
		binding := &IdentityBinding{Agents: agent.NewBaseProvider(c), Namespace: source.Namespace}
		return binding.WriteRelease(ctx, nil, 7, definition)
	}
	proposed := governedCorpus(t,
		map[string]any{"id": oecDefinition},
		map[string]any{"id": v2, "definitionVersion": "2", "lifecycleState": "proposed"},
	)
	if err := publish(t, proposed, "2"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("release 2 froze definition 1 while a proposed definition declares 2: want ErrConflict, got %v", err)
	}
	if err := publish(t, proposed, "1"); err != nil {
		t.Fatalf("release 1 froze the definition declaring 1: %v", err)
	}
	active := governedCorpus(t,
		map[string]any{"id": oecDefinition, "lifecycleState": "retired"},
		map[string]any{"id": v2, "definitionVersion": "2", "lifecycleState": "active"},
	)
	if err := publish(t, active, "2"); err != nil {
		t.Fatalf("release 2 froze the definition in force that declares 2: %v", err)
	}
	if err := publish(t, active, "1"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("release 1 froze definition 2: want ErrConflict, got %v", err)
	}
}

func TestIdentityBindingChecksTheFrozenCharterDocument(t *testing.T) {
	binding := &IdentityBinding{Agents: agent.NewBaseProvider(harbor(t)), Namespace: func(int64) string { return harborNS }}
	section := func(enterprise *domain.GovernanceSnapshotRef) map[string]json.RawMessage {
		raw, err := json.Marshal(domain.AgentGovernance{Purpose: "p", Enterprise: enterprise})
		if err != nil {
			t.Fatal(err)
		}
		return map[string]json.RawMessage{domain.GovernanceSection: raw}
	}
	ref := func(kind, namespace, id, version string) *domain.GovernanceSnapshotRef {
		return &domain.GovernanceSnapshotRef{Document: domain.CharterObjectRef{Kind: kind, ID: id, Namespace: namespace}, Version: version, Digest: "sha256:x"}
	}
	cases := []struct {
		name     string
		sections map[string]json.RawMessage
		want     error
	}{
		{"no governance section", nil, nil},
		{"governance without an enterprise snapshot", section(nil), nil},
		{"another kind of enterprise document", section(ref("Directory", "", "DIR-1", "7")), nil},
		{"the declaring definition", section(ref("AgentDefinition", "", oecDefinition, "1")), nil},
		{"the declaring definition, namespaced", section(ref("AgentDefinition", harborNS, oecDefinition, "1")), nil},
		{"another definition", section(ref("AgentDefinition", "", "AGENTDEF-OTHER", "1")), domain.ErrConflict},
		{"another snapshot version", section(ref("AgentDefinition", "", oecDefinition, "2")), domain.ErrConflict},
		{"another namespace", section(ref("AgentDefinition", otherNS, oecDefinition, "1")), domain.ErrConflict},
		{"a malformed section", map[string]json.RawMessage{domain.GovernanceSection: json.RawMessage(`{"purpose":1}`)}, domain.ErrValidation},
	}
	for _, tc := range cases {
		err := binding.WriteRelease(context.Background(), nil, 7, domain.AgentDefinition{AgentID: oecAgent, Version: "1", Sections: tc.sections})
		if (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: want %v, got %v", tc.name, tc.want, err)
		}
	}
}
