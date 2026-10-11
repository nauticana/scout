package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func billingGovernance() domain.AgentGovernance {
	return domain.AgentGovernance{
		Purpose:              "Answer billing questions",
		Accountable:          &domain.CharterObjectRef{Kind: "Position", ID: "POS-BILLING-LEAD"},
		ResponsibilityIDs:    []domain.CharterRef{{ID: "RESP-BILLING-ANSWERS"}},
		RequiredInputs:       []string{"customer account"},
		ExpectedOutcomes:     []string{"answer with sources"},
		CapabilityIDs:        []domain.CharterRef{{Namespace: "acme.example", ID: "CAP-READ-INVOICE"}},
		Policies:             []string{"refund policy"},
		ConfidenceBoundaries: []string{"amount above the refund limit"},
		EscalationConditions: []string{"customer disputes a charge"},
		TriggerChannels: []domain.AgentTrigger{
			{Trigger: "customer asks in the app", Channel: domain.TriggerConversation},
			{Trigger: "customer asks in the app", Channel: domain.TriggerMCP},
			{Trigger: "nightly invoice review", Channel: domain.TriggerSchedule, Binding: "sched-17"},
		},
	}
}

func enterpriseSnapshot() domain.GovernanceSnapshot {
	return domain.GovernanceSnapshot{
		Document: domain.CharterObjectRef{Kind: "AgentDefinition", ID: "AGENTDEF-BILLING", Namespace: "acme.example"},
		Version:  "4", Digest: "sha256:4f1c", Link: "https://charter.acme.example/agents/AGENTDEF-BILLING",
		Governance: domain.AgentGovernance{
			Purpose:     "Resolve billing disputes",
			Accountable: &domain.CharterObjectRef{Kind: "OrganizationUnit", ID: "OU-FINANCE"},
			Triggers:    []string{"billing dispute opened"},
		},
	}
}

func storedGovernance(t *testing.T, governance domain.AgentGovernance) [][]any {
	t.Helper()
	encoded, err := json.Marshal(governance)
	if err != nil {
		t.Fatal(err)
	}
	return [][]any{{string(encoded)}}
}

type governanceFixture struct {
	service  *StudioGovernance
	query    *studioQueryFake
	actions  []domain.StudioAction
	verified []domain.AgentTrigger
}

// newGovernanceFixture serves agent writer-a at draft revision 4 and proves tenant 8's bindings of it.
func newGovernanceFixture(rows map[string][][]any) *governanceFixture {
	fixture := &governanceFixture{query: &studioQueryFake{rows: rows, args: map[string][]any{}}}
	if rows[qGovernanceAgent] == nil {
		rows[qGovernanceAgent] = [][]any{{"active", int64(4)}}
	}
	if rows[qStudioBumpDraft] == nil {
		rows[qStudioBumpDraft] = [][]any{{int64(5)}}
	}
	fixture.service = &StudioGovernance{
		DB: studioDBFake{qs: fixture.query},
		Authorizer: studioAuthorizerFunc(func(_ context.Context, _ domain.StudioActor, action domain.StudioAction, _ string) error {
			fixture.actions = append(fixture.actions, action)
			return nil
		}),
		Bindings: fake.TriggerBindingVerifier{VerifyFunc: func(_ context.Context, tenantID int64, agentID string, trigger domain.AgentTrigger) (bool, error) {
			if tenantID != 8 || agentID != "writer-a" {
				return false, nil
			}
			fixture.verified = append(fixture.verified, trigger)
			return true, nil
		}},
	}
	return fixture
}

func (f *governanceFixture) owning(snapshot domain.GovernanceSnapshot) *governanceFixture {
	f.service.Source = fake.GovernanceSource{SnapshotFunc: func(_ context.Context, tenantID int64, agentID string) (domain.GovernanceSnapshot, bool, error) {
		return snapshot, tenantID == 8 && agentID == "writer-a", nil
	}}
	return f
}

func governanceChange(governance domain.AgentGovernance) domain.AgentGovernanceChange {
	return domain.AgentGovernanceChange{AgentID: "writer-a", Governance: governance, ExpectedDraftRevision: 4}
}

func fieldsOf(t *testing.T, err error) []string {
	t.Helper()
	var validation *StudioValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %v, want field errors", err)
	}
	fields := make([]string, 0, len(validation.Fields))
	for _, field := range validation.Fields {
		fields = append(fields, field.Field)
	}
	return fields
}

func TestSaveGovernanceStoresTheDraftBehindTheDraftRevision(t *testing.T) {
	fixture := newGovernanceFixture(map[string][][]any{})
	view, err := fixture.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(billingGovernance()))
	if err != nil {
		t.Fatalf("SaveGovernance: %v", err)
	}
	put := fixture.query.args[qGovernancePut]
	if put[0] != int64(8) || put[1] != "writer-a" || put[3] != int64(9) {
		t.Fatalf("put = %v", put)
	}
	var stored domain.AgentGovernance
	if err = json.Unmarshal([]byte(put[2].(string)), &stored); err != nil {
		t.Fatal(err)
	}
	encoded := put[2].(string)
	if !strings.Contains(encoded, `"responsibilityIds":["RESP-BILLING-ANSWERS"]`) || strings.Contains(encoded, `"triggers"`) ||
		len(stored.TriggerChannels) != 3 || stored.CapabilityIDs[0].Namespace != "acme.example" {
		t.Fatalf("stored = %s", encoded)
	}
	if bump := fixture.query.args[qStudioBumpDraft]; bump[0] != int64(9) || bump[1] != int64(8) || bump[3] != int64(4) {
		t.Fatalf("bump = %v", bump)
	}
	if audit := fixture.query.args[qStudioAudit]; audit[2] != "GOVERNANCE" || !strings.HasPrefix(audit[3].(string), "changed purpose, accountable, responsibilityIds, triggers") {
		t.Fatalf("audit = %v", audit)
	}
	if fixture.query.commits != 1 || !slices.Equal(fixture.actions, []domain.StudioAction{domain.StudioEdit}) {
		t.Fatalf("commits = %d, actions = %v", fixture.query.commits, fixture.actions)
	}
	if len(fixture.verified) != 1 || fixture.verified[0].Binding != "sched-17" {
		t.Fatalf("verified = %+v, want only the schedule", fixture.verified)
	}
	if view.DraftRevision != 5 || len(view.Problems) != 0 || view.Effective.Enterprise != nil ||
		!slices.Equal(view.Effective.Triggers, []string{"customer asks in the app", "nightly invoice review"}) || len(view.Draft.Triggers) != 0 {
		t.Fatalf("view = %+v", view)
	}
}

func TestSaveGovernanceRefusesAMalformedDraftUnwritten(t *testing.T) {
	cases := map[string]struct {
		change func(*domain.AgentGovernance)
		field  string
	}{
		"role accountable":    {func(g *domain.AgentGovernance) { g.Accountable.Kind = "Role" }, "accountable.kind"},
		"padded purpose":      {func(g *domain.AgentGovernance) { g.Purpose = " Answer" }, "purpose"},
		"long purpose":        {func(g *domain.AgentGovernance) { g.Purpose = strings.Repeat("a", 501) }, "purpose"},
		"spaced identifier":   {func(g *domain.AgentGovernance) { g.ResponsibilityIDs[0].ID = "RESP 1" }, "responsibilityIds[0].id"},
		"padded namespace":    {func(g *domain.AgentGovernance) { g.CapabilityIDs[0].Namespace = " acme" }, "capabilityIds[0].namespace"},
		"duplicate reference": {func(g *domain.AgentGovernance) { g.CapabilityIDs = append(g.CapabilityIDs, g.CapabilityIDs[0]) }, "capabilityIds[1]"},
		"blank policy":        {func(g *domain.AgentGovernance) { g.Policies = []string{""} }, "policies[0]"},
		"duplicate outcome":   {func(g *domain.AgentGovernance) { g.ExpectedOutcomes = []string{"x", "x"} }, "expectedOutcomes[1]"},
		"too many inputs":     {func(g *domain.AgentGovernance) { g.RequiredInputs = make([]string, 33) }, "requiredInputs"},
		"unknown channel":     {func(g *domain.AgentGovernance) { g.TriggerChannels[0].Channel = "email" }, "triggerChannels[0].channel"},
		"unbound schedule":    {func(g *domain.AgentGovernance) { g.TriggerChannels[2].Binding = "" }, "triggerChannels[2].binding"},
		"bound conversation":  {func(g *domain.AgentGovernance) { g.TriggerChannels[0].Binding = "room-1" }, "triggerChannels[0].binding"},
		"control in binding":  {func(g *domain.AgentGovernance) { g.TriggerChannels[2].Binding = "sched\n17" }, "triggerChannels[2].binding"},
		"repeated channel": {func(g *domain.AgentGovernance) {
			g.TriggerChannels = append(g.TriggerChannels, g.TriggerChannels[0])
		}, "triggerChannels[3]"},
		"shared schedule": {func(g *domain.AgentGovernance) {
			g.TriggerChannels = append(g.TriggerChannels, domain.AgentTrigger{Trigger: "weekly digest", Channel: domain.TriggerSchedule, Binding: "sched-17"})
		}, "triggerChannels[3].binding"},
		"derived triggers":   {func(g *domain.AgentGovernance) { g.Triggers = []string{"customer asks in the app"} }, "triggers"},
		"claimed enterprise": {func(g *domain.AgentGovernance) { g.Enterprise = &domain.GovernanceSnapshotRef{Version: "1"} }, "enterprise"},
	}
	for name, test := range cases {
		fixture := newGovernanceFixture(map[string][][]any{})
		governance := billingGovernance()
		accountable := *governance.Accountable
		governance.Accountable = &accountable
		test.change(&governance)
		_, err := fixture.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(governance))
		if fields := fieldsOf(t, err); !slices.Contains(fields, test.field) || !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: fields = %v, want %s", name, fields, test.field)
		}
		if slices.Contains(fixture.query.queries, qStudioBumpDraft) || slices.Contains(fixture.query.queries, qGovernancePut) || len(fixture.verified) != 0 {
			t.Fatalf("%s: a refused draft was written or verified: %v", name, fixture.query.queries)
		}
	}
	change := governanceChange(billingGovernance())
	change.ExpectedDraftRevision = 0
	_, err := newGovernanceFixture(map[string][][]any{}).service.SaveGovernance(context.Background(), humanActor(8, 9), change)
	if fields := fieldsOf(t, err); !slices.Equal(fields, []string{"expected_agent_revision"}) {
		t.Fatalf("fields = %v", fields)
	}
}

// A schedule or event the product cannot prove is refused, and so is every one when nothing can prove it.
func TestSaveGovernanceFailsClosedOnTriggerBindings(t *testing.T) {
	unverified := newGovernanceFixture(map[string][][]any{})
	unverified.service.Bindings = nil
	_, err := unverified.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(billingGovernance()))
	if fields := fieldsOf(t, err); !slices.Equal(fields, []string{"triggerChannels[2].binding"}) || slices.Contains(unverified.query.queries, qGovernancePut) {
		t.Fatalf("fields = %v, queries = %v", fields, unverified.query.queries)
	}
	conversational := billingGovernance()
	conversational.TriggerChannels = conversational.TriggerChannels[:2]
	if _, err = unverified.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(conversational)); err != nil {
		t.Fatalf("a draft without schedules or events needs no verifier: %v", err)
	}

	unknown := newGovernanceFixture(map[string][][]any{})
	unknown.service.Bindings = fake.TriggerBindingVerifier{VerifyFunc: func(context.Context, int64, string, domain.AgentTrigger) (bool, error) {
		return false, nil
	}}
	_, err = unknown.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(billingGovernance()))
	if fields := fieldsOf(t, err); !slices.Equal(fields, []string{"triggerChannels[2].binding"}) {
		t.Fatalf("fields = %v", fields)
	}

	down := errors.New("schedule store down")
	failing := newGovernanceFixture(map[string][][]any{})
	failing.service.Bindings = fake.TriggerBindingVerifier{VerifyFunc: func(context.Context, int64, string, domain.AgentTrigger) (bool, error) {
		return false, down
	}}
	_, err = failing.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(billingGovernance()))
	var validation *StudioValidationError
	if !errors.Is(err, down) || errors.As(err, &validation) || slices.Contains(failing.query.queries, qGovernancePut) {
		t.Fatalf("a verifier failure must surface as a failure, got %v", err)
	}
}

func TestSaveGovernanceRefusesWhatTheEnterpriseOwns(t *testing.T) {
	for name, change := range map[string]func(*domain.AgentGovernance){
		"purpose": func(g *domain.AgentGovernance) { g.Purpose = "Something else" },
		"accountable": func(g *domain.AgentGovernance) {
			g.Accountable = &domain.CharterObjectRef{Kind: "Position", ID: "POS-1"}
		},
		"triggerChannels": func(g *domain.AgentGovernance) { g.TriggerChannels = billingGovernance().TriggerChannels },
	} {
		fixture := newGovernanceFixture(map[string][][]any{}).owning(enterpriseSnapshot())
		governance := billingGovernance()
		governance.Purpose, governance.Accountable, governance.TriggerChannels = "", nil, nil
		change(&governance)
		_, err := fixture.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(governance))
		if fields := fieldsOf(t, err); !slices.Equal(fields, []string{name}) || slices.Contains(fixture.query.queries, qGovernancePut) {
			t.Fatalf("%s: fields = %v", name, fields)
		}
	}

	fixture := newGovernanceFixture(map[string][][]any{}).owning(enterpriseSnapshot())
	governance := billingGovernance()
	governance.Purpose, governance.Accountable, governance.TriggerChannels = "", nil, nil
	view, err := fixture.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(governance))
	if err != nil {
		t.Fatalf("SaveGovernance: %v", err)
	}
	if stored := fixture.query.args[qGovernancePut][2].(string); strings.Contains(stored, "Resolve billing disputes") || strings.Contains(stored, "OU-FINANCE") {
		t.Fatalf("enterprise-owned values must never be stored: %s", stored)
	}
	effective := view.Effective
	if effective.Purpose != "Resolve billing disputes" || effective.Accountable.ID != "OU-FINANCE" || effective.ResponsibilityIDs[0].ID != "RESP-BILLING-ANSWERS" ||
		!slices.Equal(effective.Triggers, []string{"billing dispute opened"}) || len(effective.TriggerChannels) != 0 {
		t.Fatalf("effective = %+v", effective)
	}
	want := []domain.GovernanceField{domain.GovernancePurpose, domain.GovernanceAccountable, domain.GovernanceTriggers}
	if effective.Enterprise == nil || effective.Enterprise.Version != "4" || effective.Enterprise.Digest != "sha256:4f1c" || !slices.Equal(effective.Enterprise.Fields, want) {
		t.Fatalf("enterprise = %+v", effective.Enterprise)
	}
	if view.Link != enterpriseSnapshot().Link || len(view.Problems) != 0 || view.Draft.Purpose != "" {
		t.Fatalf("view = %+v", view)
	}
}

func TestGovernanceHidesStaleDraftValuesTheEnterpriseNowOwns(t *testing.T) {
	stale := billingGovernance()
	fixture := newGovernanceFixture(map[string][][]any{qGovernanceGet: storedGovernance(t, stale)}).owning(enterpriseSnapshot())
	view, err := fixture.service.Governance(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatalf("Governance: %v", err)
	}
	if view.Draft.Purpose != "" || view.Draft.Accountable != nil || len(view.Draft.TriggerChannels) != 0 || view.Draft.Policies[0] != "refund policy" {
		t.Fatalf("draft = %+v", view.Draft)
	}
	if view.Effective.Purpose != "Resolve billing disputes" || len(view.Effective.TriggerChannels) != 0 || view.DraftRevision != 4 {
		t.Fatalf("effective = %+v", view.Effective)
	}
	if !slices.Equal(fixture.actions, []domain.StudioAction{domain.StudioView}) || slices.Contains(fixture.query.queries, qGovernancePut) {
		t.Fatalf("actions = %v, queries = %v", fixture.actions, fixture.query.queries)
	}
}

func TestGovernanceReportsWhatPublicationWouldRefuse(t *testing.T) {
	partial := domain.AgentGovernance{Purpose: "Answer billing questions", TriggerChannels: billingGovernance().TriggerChannels}
	fixture := newGovernanceFixture(map[string][][]any{qGovernanceGet: storedGovernance(t, partial)})
	fixture.service.Bindings = nil
	view, err := fixture.service.Governance(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatalf("Governance: %v", err)
	}
	var problems []string
	for _, problem := range view.Problems {
		problems = append(problems, problem.Field)
	}
	want := []string{"accountable", "responsibilityIds", "requiredInputs", "expectedOutcomes", "capabilityIds", "policies",
		"confidenceBoundaries", "escalationConditions", "triggerChannels[2].binding"}
	if !slices.Equal(problems, want) {
		t.Fatalf("problems = %v, want %v", problems, want)
	}
}

func TestGovernanceFailsClosedOnAnUnusableSnapshot(t *testing.T) {
	for name, change := range map[string]func(*domain.GovernanceSnapshot){
		"no digest":      func(s *domain.GovernanceSnapshot) { s.Digest = "" },
		"no version":     func(s *domain.GovernanceSnapshot) { s.Version = "" },
		"script link":    func(s *domain.GovernanceSnapshot) { s.Link = "javascript:alert(1)" },
		"scout channels": func(s *domain.GovernanceSnapshot) { s.Governance.TriggerChannels = billingGovernance().TriggerChannels },
		"blank policy":   func(s *domain.GovernanceSnapshot) { s.Governance.Policies = []string{""} },
		"repeated ref": func(s *domain.GovernanceSnapshot) {
			s.Governance.CapabilityIDs = []domain.CharterRef{{ID: "CAP-1"}, {ID: "CAP-1"}}
		},
		"role owner": func(s *domain.GovernanceSnapshot) { s.Governance.Accountable.Kind = "Role" },
	} {
		snapshot := enterpriseSnapshot()
		change(&snapshot)
		fixture := newGovernanceFixture(map[string][][]any{}).owning(snapshot)
		if _, err := fixture.service.Governance(context.Background(), humanActor(8, 9), "writer-a"); err == nil || errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: error = %v, want a failure", name, err)
		}
	}
	down := errors.New("directory down")
	fixture := newGovernanceFixture(map[string][][]any{})
	fixture.service.Source = fake.GovernanceSource{SnapshotFunc: func(context.Context, int64, string) (domain.GovernanceSnapshot, bool, error) {
		return domain.GovernanceSnapshot{}, false, down
	}}
	if _, err := fixture.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(billingGovernance())); !errors.Is(err, down) {
		t.Fatalf("error = %v, want the source failure", err)
	}
}

func TestSaveGovernanceChecksTheAgentAndItsRevision(t *testing.T) {
	stale := newGovernanceFixture(map[string][][]any{qStudioBumpDraft: {}})
	if _, err := stale.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(billingGovernance())); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("error = %v, want revision conflict", err)
	}
	if slices.Contains(stale.query.queries, qGovernancePut) || stale.query.commits != 0 {
		t.Fatalf("a stale save must not write: %v", stale.query.queries)
	}
	archived := newGovernanceFixture(map[string][][]any{qGovernanceAgent: {{"archived", int64(4)}}})
	if _, err := archived.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(billingGovernance())); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	missing := newGovernanceFixture(map[string][][]any{qGovernanceAgent: {}})
	if _, err := missing.service.Governance(context.Background(), humanActor(8, 9), "nobody"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
}

// The tenant comes from the actor: the store, the source, and the verifier are all asked for it.
func TestGovernanceScopesEveryCallToTheActorsTenant(t *testing.T) {
	fixture := newGovernanceFixture(map[string][][]any{}).owning(enterpriseSnapshot())
	view, err := fixture.service.Governance(context.Background(), humanActor(9, 9), "writer-a")
	if err != nil {
		t.Fatalf("Governance: %v", err)
	}
	if fixture.query.args[qGovernanceAgent][0] != int64(9) || fixture.query.args[qGovernanceGet][0] != int64(9) || view.Effective.Enterprise != nil {
		t.Fatalf("args = %v, enterprise = %+v", fixture.query.args, view.Effective.Enterprise)
	}
	_, err = fixture.service.SaveGovernance(context.Background(), humanActor(9, 9), governanceChange(billingGovernance()))
	if fields := fieldsOf(t, err); !slices.Equal(fields, []string{"triggerChannels[2].binding"}) || len(fixture.verified) != 0 {
		t.Fatalf("another tenant's schedule must not prove this one: %v", fields)
	}
}

func TestGovernanceAuthorizesBeforeReading(t *testing.T) {
	fixture := newGovernanceFixture(map[string][][]any{})
	fixture.service.Authorizer = studioAuthorizerFunc(func(context.Context, domain.StudioActor, domain.StudioAction, string) error {
		return domain.ErrForbidden
	})
	if _, err := fixture.service.Governance(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("view error = %v", err)
	}
	if _, err := fixture.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(billingGovernance())); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("save error = %v", err)
	}
	if len(fixture.query.queries) != 0 || len(fixture.verified) != 0 {
		t.Fatalf("an unauthorized call touched the store: %v", fixture.query.queries)
	}
	unwired := &StudioGovernance{DB: studioDBFake{qs: fixture.query}}
	if _, err := unwired.Governance(context.Background(), humanActor(8, 9), "writer-a"); err == nil {
		t.Fatal("a governance service without an authorizer must refuse")
	}
}

func TestGovernanceSectionFreezesTheEffectiveGovernance(t *testing.T) {
	empty := newGovernanceFixture(map[string][][]any{})
	if raw, err := empty.service.Section(context.Background(), 8, domain.AgentDraft{AgentID: "writer-a"}); err != nil || raw != nil {
		t.Fatalf("an agent without governance contributes no section: %s, %v", raw, err)
	}
	owned := newGovernanceFixture(map[string][][]any{qGovernanceGet: storedGovernance(t, billingGovernance())}).owning(enterpriseSnapshot())
	raw, err := owned.service.Section(context.Background(), 8, domain.AgentDraft{AgentID: "writer-a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"purpose":"Resolve billing disputes"`, `"triggers":["billing dispute opened"]`, `"responsibilityIds":["RESP-BILLING-ANSWERS"]`,
		`"enterprise":{"document":{"kind":"AgentDefinition","id":"AGENTDEF-BILLING","namespace":"acme.example"},"version":"4","digest":"sha256:4f1c","fields":["purpose","accountable","triggers"]}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("section lacks %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "triggerChannels") || strings.Contains(string(raw), "enterprise_link") || strings.Contains(string(raw), "https://") {
		t.Fatalf("an enterprise-owned section freezes neither Studio's channels nor the link: %s", raw)
	}
	if owned.service.Name() != domain.GovernanceSection {
		t.Fatalf("name = %q", owned.service.Name())
	}
}

func frozenCandidate(t *testing.T, governance *domain.AgentGovernance, restoredFrom string) domain.PublishCandidate {
	t.Helper()
	candidate := domain.PublishCandidate{Actor: humanActor(8, 9), Definition: domain.AgentDefinition{AgentID: "writer-a"}, RestoredFrom: restoredFrom}
	if governance != nil {
		raw, err := json.Marshal(governance)
		if err != nil {
			t.Fatal(err)
		}
		candidate.Definition.Sections = map[string]json.RawMessage{domain.GovernanceSection: raw}
	}
	return candidate
}

func TestGovernanceGuardRequiresTheActiveCharterProfile(t *testing.T) {
	fixture := newGovernanceFixture(map[string][][]any{})
	err := fixture.service.CheckPublish(context.Background(), frozenCandidate(t, nil, ""))
	var want []string
	for _, field := range domain.CharterGovernanceFields() {
		want = append(want, string(field))
	}
	if fields := fieldsOf(t, err); !slices.Equal(fields, want) {
		t.Fatalf("fields = %v, want every Charter field", fields)
	}

	complete := effectiveGovernance(billingGovernance(), nil)
	if err = fixture.service.CheckPublish(context.Background(), frozenCandidate(t, &complete, "")); err != nil {
		t.Fatalf("complete governance: %v", err)
	}

	fixture.service.Required = []domain.GovernanceField{domain.GovernanceAccountable, domain.GovernanceEscalationConditions}
	if fields := fieldsOf(t, fixture.service.CheckPublish(context.Background(), frozenCandidate(t, &domain.AgentGovernance{Purpose: "x"}, ""))); !slices.Equal(fields, []string{"accountable", "escalationConditions"}) {
		t.Fatalf("fields = %v", fields)
	}
	fixture.service.Required = []domain.GovernanceField{}
	if err = fixture.service.CheckPublish(context.Background(), frozenCandidate(t, nil, "")); err != nil {
		t.Fatalf("a profile requiring nothing admits no governance: %v", err)
	}
	fixture.service.Required = []domain.GovernanceField{"owner"}
	if err = fixture.service.CheckPublish(context.Background(), frozenCandidate(t, &complete, "")); err == nil || errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unknown required field is a composition fault, got %v", err)
	}
}

func TestGovernanceGuardRefusesTamperedOrUnprovenGovernance(t *testing.T) {
	fixture := newGovernanceFixture(map[string][][]any{})
	tampered := effectiveGovernance(billingGovernance(), nil)
	tampered.Triggers = []string{"something no channel declares"}
	if fields := fieldsOf(t, fixture.service.CheckPublish(context.Background(), frozenCandidate(t, &tampered, "3"))); !slices.Equal(fields, []string{"triggers"}) {
		t.Fatalf("fields = %v", fields)
	}
	complete := effectiveGovernance(billingGovernance(), nil)
	fixture.service.Bindings = fake.TriggerBindingVerifier{VerifyFunc: func(context.Context, int64, string, domain.AgentTrigger) (bool, error) {
		return false, nil
	}}
	if fields := fieldsOf(t, fixture.service.CheckPublish(context.Background(), frozenCandidate(t, &complete, "3"))); !slices.Equal(fields, []string{"triggerChannels[2].binding"}) {
		t.Fatalf("a restore re-proves its bindings, fields = %v", fields)
	}
	malformed := frozenCandidate(t, nil, "")
	malformed.Definition.Sections = map[string]json.RawMessage{domain.GovernanceSection: json.RawMessage(`{"purpose":7}`)}
	if err := fixture.service.CheckPublish(context.Background(), malformed); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v", err)
	}
}

// A release frozen before governance existed stays restorable; the profile binds what is published now.
func TestGovernanceGuardRestoresAReleaseThatPredatesGovernance(t *testing.T) {
	fixture := newGovernanceFixture(map[string][][]any{})
	if err := fixture.service.CheckPublish(context.Background(), frozenCandidate(t, nil, "7")); err != nil {
		t.Fatalf("restore of a release without governance: %v", err)
	}
	if fields := fieldsOf(t, fixture.service.CheckPublish(context.Background(), frozenCandidate(t, &domain.AgentGovernance{Purpose: "x"}, "7"))); len(fields) != 9 {
		t.Fatalf("a restored release that froze governance still meets the profile, fields = %v", fields)
	}
	tampered := domain.AgentGovernance{Triggers: []string{"undeclared"}}
	fixture.service.Required = []domain.GovernanceField{}
	if fields := fieldsOf(t, fixture.service.CheckPublish(context.Background(), frozenCandidate(t, &tampered, "7"))); !slices.Equal(fields, []string{"triggers"}) {
		t.Fatalf("fields = %v", fields)
	}
	owned := newGovernanceFixture(map[string][][]any{}).owning(enterpriseSnapshot())
	if err := owned.service.CheckPublish(context.Background(), frozenCandidate(t, nil, "7")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a release without the enterprise's governance stays refused: %v", err)
	}
}

// A restore may not reactivate enterprise governance the source has since replaced.
func TestGovernanceGuardHoldsAReleaseToTheCurrentEnterpriseSnapshot(t *testing.T) {
	current := enterpriseSnapshot()
	draft := billingGovernance()
	draft.Purpose, draft.Accountable, draft.TriggerChannels = "", nil, nil
	frozen := effectiveGovernance(draft, &current)

	fixture := newGovernanceFixture(map[string][][]any{}).owning(current)
	if err := fixture.service.CheckPublish(context.Background(), frozenCandidate(t, &frozen, "3")); err != nil {
		t.Fatalf("the current snapshot: %v", err)
	}
	superseded := current
	superseded.Version, superseded.Digest = "5", "sha256:9e0d"
	if err := newGovernanceFixture(map[string][][]any{}).owning(superseded).service.CheckPublish(context.Background(), frozenCandidate(t, &frozen, "3")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("superseded snapshot: %v", err)
	}
	studioOwned := effectiveGovernance(billingGovernance(), nil)
	if err := fixture.service.CheckPublish(context.Background(), frozenCandidate(t, &studioOwned, "2")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("Studio governance under an enterprise owner: %v", err)
	}
	released := newGovernanceFixture(map[string][][]any{})
	if err := released.service.CheckPublish(context.Background(), frozenCandidate(t, &frozen, "3")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("an enterprise snapshot the source no longer supplies: %v", err)
	}
}

// Enterprise-owned values meet Charter's schema alone: Studio's item cap, distinct statements, and
// trimming would refuse a release nobody in Studio can correct.
func TestGovernanceHoldsEnterpriseValuesToCharterBoundsOnly(t *testing.T) {
	snapshot := enterpriseSnapshot()
	inputs := make([]string, 40)
	for index := range inputs {
		inputs[index] = fmt.Sprintf("input %d", index)
	}
	snapshot.Governance.RequiredInputs = inputs
	snapshot.Governance.Policies = []string{"refund policy", "refund policy", " padded policy"}
	draft := billingGovernance()
	draft.Purpose, draft.Accountable, draft.TriggerChannels, draft.RequiredInputs, draft.Policies = "", nil, nil, nil, nil

	fixture := newGovernanceFixture(map[string][][]any{qGovernanceGet: storedGovernance(t, draft)}).owning(snapshot)
	view, err := fixture.service.Governance(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || len(view.Problems) != 0 {
		t.Fatalf("problems = %+v, err = %v", view.Problems, err)
	}
	frozen := effectiveGovernance(draft, &snapshot)
	if err = fixture.service.CheckPublish(context.Background(), frozenCandidate(t, &frozen, "")); err != nil {
		t.Fatalf("publish under a Charter-valid snapshot: %v", err)
	}
	if _, err = fixture.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(draft)); err != nil {
		t.Fatalf("save beside a Charter-valid snapshot: %v", err)
	}

	studio := effectiveGovernance(billingGovernance(), nil)
	studio.RequiredInputs = inputs
	studio.Policies = []string{"refund policy", "refund policy"}
	fields := fieldsOf(t, newGovernanceFixture(map[string][][]any{}).service.CheckPublish(context.Background(), frozenCandidate(t, &studio, "")))
	if !slices.Equal(fields, []string{"requiredInputs", "policies[1]"}) {
		t.Fatalf("Studio-authored values keep Studio's bounds, fields = %v", fields)
	}
}

func governedPublishService(t *testing.T, stored domain.AgentGovernance) (*StudioService, *studioQueryFake) {
	t.Helper()
	service, query := publishService()
	query.rows[qGovernanceGet] = storedGovernance(t, stored)
	governance := &StudioGovernance{
		DB: studioDBFake{qs: query}, Authorizer: service,
		Bindings: fake.TriggerBindingVerifier{VerifyFunc: func(context.Context, int64, string, domain.AgentTrigger) (bool, error) {
			return true, nil
		}},
	}
	service.Sections = []contract.DefinitionSection{governance}
	service.Guards = []contract.PublishGuard{governance}
	return service, query
}

func TestPublishFreezesGovernanceAsACharterFragment(t *testing.T) {
	service, query := governedPublishService(t, billingGovernance())
	request := domain.AgentPublishRequest{AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2}
	if _, err := service.Publish(context.Background(), humanActor(8, 9), request); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	encoded := query.args[qStudioInsertVersion][3].(string)
	for _, want := range []string{
		`"sections":{"governance":{"accountable":{"id":"POS-BILLING-LEAD","kind":"Position"}`,
		`"capabilityIds":[{"id":"CAP-READ-INVOICE","namespace":"acme.example"}]`,
		`"triggers":["customer asks in the app","nightly invoice review"]`,
		`{"binding":"sched-17","channel":"schedule","trigger":"nightly invoice review"}`,
	} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("definition lacks %s: %s", want, encoded)
		}
	}

	incomplete := billingGovernance()
	incomplete.Accountable, incomplete.EscalationConditions = nil, nil
	refused, refusedQuery := governedPublishService(t, incomplete)
	_, err := refused.Publish(context.Background(), humanActor(8, 9), request)
	if fields := fieldsOf(t, err); !slices.Equal(fields, []string{"accountable", "escalationConditions"}) || slices.Contains(refusedQuery.queries, qStudioInsertVersion) {
		t.Fatalf("fields = %v, queries = %v", fields, refusedQuery.queries)
	}
}

func TestGovernanceChangeDriftsFromTheActiveRelease(t *testing.T) {
	service, query := governedPublishService(t, billingGovernance())
	active := domain.AgentDefinition{AgentID: "writer-a", Sections: map[string]json.RawMessage{domain.GovernanceSection: json.RawMessage(`{"purpose":"Old purpose"}`)}}
	encoded, err := json.Marshal(active)
	if err != nil {
		t.Fatal(err)
	}
	query.rows[qStudioActiveDefinition] = [][]any{{"3", string(encoded)}}
	draft, err := service.GetDraft(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if draft.Drift == nil || !slices.Contains(draft.Drift.Causes, "section:governance") {
		t.Fatalf("drift = %+v", draft.Drift)
	}
}
