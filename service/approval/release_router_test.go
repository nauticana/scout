package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/scope"
)

const paymentsRouting = `{"routes":[
 {"id":"payments","match":{"tools":["wire"],"sources":["policy","autonomy"],"min_risk_tier":"high"},
  "approver":{"kind":"position","ref":"controller"},"deadline_seconds":3600,
  "escalation":[{"approver":{"kind":"role","ref":"cfo-office"},"extension_seconds":1800},{"extension_seconds":600}],
  "on_exhausted":"hold"},
 {"id":"default","match":{},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":7200}]}`

var (
	controller = domain.ApproverReference{Kind: domain.ApproverPosition, Ref: "controller"}
	cfoOffice  = domain.ApproverReference{Kind: domain.ApproverRole, Ref: "cfo-office"}
)

func person(id string) domain.EligibleApprover {
	return domain.EligibleApprover{Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: id}, Resolution: "holder/" + id}
}

// routerFixture keys its release by tenant, agent, and version, so a read under any other tenant finds nothing.
type routerFixture struct {
	routing   string
	holders   map[domain.ApproverReference][]domain.EligibleApprover
	queries   []domain.ApproverQuery
	forbidden map[string]error
	reads     []string
	releaseOf func(domain.EffectiveRelease) domain.EffectiveRelease
}

func newRouterFixture(t *testing.T, routing string) (*routerFixture, *ReleaseRouter) {
	t.Helper()
	f := &routerFixture{routing: routing, holders: map[domain.ApproverReference][]domain.EligibleApprover{}, forbidden: map[string]error{}}
	var resources []domain.EffectiveResource
	if routing != "" {
		value, err := scope.CanonicalValue(domain.ResourceApprovalRouting, []byte(routing))
		if err != nil {
			t.Fatal(err)
		}
		resources = []domain.EffectiveResource{{ResourceKind: domain.ResourceApprovalRouting, ResourceID: "approval_routing", Value: value}}
	}
	router := &ReleaseRouter{
		Releases: &fake.EffectiveReleaseRepository{GetFunc: func(_ context.Context, tenantID int64, agentID, version string) (domain.EffectiveRelease, error) {
			f.reads = append(f.reads, fmt.Sprintf("%d/%s/%s", tenantID, agentID, version))
			if tenantID != 7 || agentID != "agent" || version != "3" {
				return domain.EffectiveRelease{}, domain.ErrNotFound
			}
			release := domain.EffectiveRelease{TenantID: 7, AgentID: "agent", AgentVersion: "3", Resources: resources}
			if f.releaseOf != nil {
				release = f.releaseOf(release)
			}
			return release, nil
		}},
		Resolver: &fake.ApproverResolver{EligibleFunc: func(_ context.Context, query domain.ApproverQuery) ([]domain.EligibleApprover, error) {
			f.queries = append(f.queries, query)
			if query.Reference.Ref == "down" {
				return nil, errors.New("directory down")
			}
			return f.holders[query.Reference], nil
		}},
		Authorizer: fake.ApprovalAuthorizerFunc(func(_ context.Context, request domain.ApprovalRequest, principal domain.PrincipalRef) error {
			if request.Approver != principal {
				t.Fatalf("a candidate is authorized as the assigned approver: %+v for %+v", request.Approver, principal)
			}
			return f.forbidden[principal.ID]
		}),
		Now: func() time.Time { return gateNow },
	}
	return f, router
}

func routedRequest(step int, approver string, deadline time.Time) domain.ApprovalRequest {
	request := domain.ApprovalRequest{
		ID: 9, TenantID: 7, RequestID: "request", ExecutionStepID: 3, ProposedDigest: strings.Repeat("a", 64),
		Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "agent"}, ReleaseVersion: "3",
		Requester: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "5"},
		Resource:  "wire", Source: domain.ApprovalSourcePolicy, RiskTier: domain.RiskHigh, DeadlineAt: deadline,
		Route: domain.ApprovalRouteAuthority{RouteID: "payments", Reference: controller, Step: step},
	}
	if step > 0 {
		request.Route.Reference = cfoOffice
	}
	if approver != "" {
		request.Approver = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: approver}
	}
	return request
}

func newAsk() domain.ApprovalRequest {
	request := routedRequest(0, "", time.Time{})
	request.Route = domain.ApprovalRouteAuthority{}
	request.RiskTier = domain.RiskCritical
	return request
}

func TestReleaseRouterAssignsTheFirstEligibleHolder(t *testing.T) {
	f, router := newRouterFixture(t, paymentsRouting)
	f.holders[controller] = []domain.EligibleApprover{
		{Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "agent"}},
		person("5"),
		{Principal: domain.PrincipalRef{Kind: domain.PrincipalService, ID: "bot"}},
		{Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "alex"}},
		{Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "007"}},
		person("9"), person("11"), person("12"),
		{Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "77"}, Resolution: "holder\x00/77"},
	}
	f.forbidden["9"], f.forbidden["11"], f.forbidden["12"] = domain.ErrForbidden, domain.ErrNotFound, domain.ErrPrincipalUnknown
	assignment, err := router.Route(context.Background(), newAsk(), domain.PrincipalRef{})
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ApprovalRouteAuthority{RouteID: "payments", Reference: controller, Step: 0, Resolution: "holder/77"}
	if assignment.Approver.ID != "77" || assignment.Authority != want || assignment.Deadline != time.Hour {
		t.Fatalf("assignment = %+v", assignment)
	}
	if len(f.reads) != 1 || f.reads[0] != "7/agent/3" || len(f.queries) != 1 || f.queries[0].TenantID != 7 ||
		f.queries[0].Reference != controller || f.queries[0].Limit != domain.DefaultMaxApproverCandidates {
		t.Fatalf("reads = %v, queries = %+v", f.reads, f.queries)
	}
}

func TestReleaseRouterNeverTreatsAnOutageAsAbsence(t *testing.T) {
	f, router := newRouterFixture(t, paymentsRouting)
	f.holders[controller] = []domain.EligibleApprover{person("77")}
	f.forbidden["77"] = errors.New("grants unavailable")
	if _, err := router.Route(context.Background(), newAsk(), domain.PrincipalRef{}); err == nil || errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an authorizer outage must propagate: %v", err)
	}
	down, router := newRouterFixture(t, strings.ReplaceAll(paymentsRouting, `"ref":"controller"`, `"ref":"down"`))
	if assignment, err := router.Route(context.Background(), newAsk(), domain.PrincipalRef{}); err == nil || assignment.Authority.RouteID != "" {
		t.Fatalf("a resolver outage must propagate, never open vacant: %+v, %v (%d queries)", assignment, err, len(down.queries))
	}
}

func TestReleaseRouterConsidersABoundedCandidateList(t *testing.T) {
	f, router := newRouterFixture(t, paymentsRouting)
	for id := 100; id < 100+domain.DefaultMaxApproverCandidates+5; id++ {
		f.holders[controller] = append(f.holders[controller], person(fmt.Sprint(id)))
		if id < 100+domain.DefaultMaxApproverCandidates {
			f.forbidden[fmt.Sprint(id)] = domain.ErrForbidden
		}
	}
	assignment, err := router.Route(context.Background(), newAsk(), domain.PrincipalRef{})
	if err != nil || assignment.Approver.ID != "" || assignment.Authority.RouteID != "payments" {
		t.Fatalf("candidates past the limit are ignored: %+v, %v", assignment, err)
	}
}

func TestReleaseRouterPrefersAnEligibleEditorOnly(t *testing.T) {
	f, router := newRouterFixture(t, paymentsRouting)
	f.holders[controller] = []domain.EligibleApprover{person("77"), person("42")}
	assignment, err := router.Route(context.Background(), newAsk(), person("42").Principal)
	if err != nil || assignment.Approver.ID != "42" {
		t.Fatalf("an eligible editor comes first: %+v, %v", assignment, err)
	}
	assignment, err = router.Route(context.Background(), newAsk(), person("43").Principal)
	if err != nil || assignment.Approver.ID != "77" {
		t.Fatalf("an editor who holds no position is never inserted: %+v, %v", assignment, err)
	}
}

func TestReleaseRouterWalksTheEscalationWhenThePrimaryIsAbsent(t *testing.T) {
	f, router := newRouterFixture(t, paymentsRouting)
	f.holders[cfoOffice] = []domain.EligibleApprover{person("88")}
	assignment, err := router.Route(context.Background(), newAsk(), domain.PrincipalRef{})
	if err != nil || assignment.Approver.ID != "88" || assignment.Authority.Step != 1 || assignment.Authority.Reference != cfoOffice || assignment.Deadline != 30*time.Minute {
		t.Fatalf("assignment = %+v, %v", assignment, err)
	}
	empty, router := newRouterFixture(t, paymentsRouting)
	assignment, err = router.Route(context.Background(), newAsk(), domain.PrincipalRef{})
	if err != nil || assignment.Approver.ID != "" || assignment.Authority != (domain.ApprovalRouteAuthority{RouteID: "payments", Reference: controller}) || assignment.Deadline != time.Hour {
		t.Fatalf("nobody eligible opens vacant at step 0: %+v, %v", assignment, err)
	}
	if len(empty.queries) != 2 {
		t.Fatalf("each distinct reference is resolved once: %d queries", len(empty.queries))
	}
}

func TestReleaseRouterRefusesWhatItCannotRoute(t *testing.T) {
	onlyPayments := `{"routes":[{"id":"payments","match":{"tools":["wire"]},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":3600}]}`
	_, router := newRouterFixture(t, onlyPayments)
	unmatched := newAsk()
	unmatched.Resource = "search"
	if _, err := router.Route(context.Background(), unmatched, domain.PrincipalRef{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("no route: %v", err)
	}
	_, router = newRouterFixture(t, "")
	if _, err := router.Route(context.Background(), newAsk(), domain.PrincipalRef{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("no routing and no fallback: %v", err)
	}
	router.Fallback = fake.ApproverRouterFunc(func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) (domain.ApprovalAssignment, error) {
		return domain.ApprovalAssignment{Approver: person("1").Principal}, nil
	})
	if assignment, err := router.Route(context.Background(), newAsk(), domain.PrincipalRef{}); err != nil || assignment.Approver.ID != "1" {
		t.Fatalf("fallback = %+v, %v", assignment, err)
	}
	for name, mutate := range map[string]func(*domain.ApprovalRequest){
		"unpinned": func(r *domain.ApprovalRequest) { r.ReleaseVersion = "" },
		"human":    func(r *domain.ApprovalRequest) { r.Principal.Kind = domain.PrincipalHuman },
	} {
		_, router := newRouterFixture(t, paymentsRouting)
		request := newAsk()
		mutate(&request)
		if _, err := router.Route(context.Background(), request, domain.PrincipalRef{}); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, mutate := range map[string]func(domain.EffectiveRelease) domain.EffectiveRelease{
		"tenant": func(r domain.EffectiveRelease) domain.EffectiveRelease { r.TenantID = 8; return r },
		"agent":  func(r domain.EffectiveRelease) domain.EffectiveRelease { r.AgentID = "other"; return r },
	} {
		f, router := newRouterFixture(t, paymentsRouting)
		f.releaseOf = mutate
		if _, err := router.Route(context.Background(), newAsk(), domain.PrincipalRef{}); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("%s mismatch: %v", name, err)
		}
	}
	foreign := newAsk()
	foreign.TenantID = 8
	_, router = newRouterFixture(t, paymentsRouting)
	if _, err := router.Route(context.Background(), foreign, domain.PrincipalRef{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant's request reads its own tenant's release: %v", err)
	}
}

func TestEscalateHandsOverdueWorkToTheNextStep(t *testing.T) {
	f, router := newRouterFixture(t, paymentsRouting)
	f.holders[cfoOffice] = []domain.EligibleApprover{person("77"), person("88")}
	overdue := gateNow.Add(-time.Minute)
	step, err := router.Escalate(context.Background(), routedRequest(0, "77", overdue))
	if err != nil || step.Action != domain.EscalationReassign || step.Backup.ID != "88" || step.Extension != 30*time.Minute ||
		step.Route.Step != 1 || step.Route.Reference != cfoOffice || step.Route.RouteID != "payments" {
		t.Fatalf("a step naming an approver excludes the current one: %+v, %v", step, err)
	}
	f.holders[cfoOffice] = []domain.EligibleApprover{person("99"), person("88")}
	step, err = router.Escalate(context.Background(), routedRequest(1, "88", overdue))
	if err != nil || step.Action != domain.EscalationReassign || step.Backup.ID != "88" || step.Extension != 10*time.Minute || step.Route.Step != 2 {
		t.Fatalf("an extension keeps the eligible current approver: %+v, %v", step, err)
	}
}

func TestEscalateVacatesTheNextStepWhenNobodyIsEligible(t *testing.T) {
	_, router := newRouterFixture(t, paymentsRouting)
	step, err := router.Escalate(context.Background(), routedRequest(0, "77", gateNow))
	if err != nil || step.Action != domain.EscalationVacate || step.Backup.ID != "" || step.Route.Step != 1 ||
		step.Route.Reference != cfoOffice || step.Extension != 30*time.Minute {
		t.Fatalf("step = %+v, %v", step, err)
	}
}

func TestEscalateHoldsOrExpiresOnceExhausted(t *testing.T) {
	_, router := newRouterFixture(t, paymentsRouting)
	request := routedRequest(2, "88", gateNow.Add(-time.Second))
	step, err := router.Escalate(context.Background(), request)
	if err != nil || step.Action != domain.EscalationHold || step.Backup != request.Approver || step.Route != request.Route {
		t.Fatalf("hold = %+v, %v", step, err)
	}
	_, router = newRouterFixture(t, strings.Replace(paymentsRouting, `"hold"`, `"expire"`, 1))
	if step, err = router.Escalate(context.Background(), request); err != nil || step.Action != domain.EscalationExpire {
		t.Fatalf("expire = %+v, %v", step, err)
	}
}

func TestEscalateFillsAVacancyOrWaits(t *testing.T) {
	f, router := newRouterFixture(t, paymentsRouting)
	vacant := routedRequest(0, "", gateNow.Add(time.Hour))
	step, err := router.Escalate(context.Background(), vacant)
	if err != nil || step.Action != domain.EscalationWait {
		t.Fatalf("nobody yet: %+v, %v", step, err)
	}
	f.holders[controller] = []domain.EligibleApprover{person("77")}
	step, err = router.Escalate(context.Background(), vacant)
	if err != nil || step.Action != domain.EscalationReassign || step.Backup.ID != "77" || !step.KeepDeadline || step.Route.Step != 0 || step.Route.Reference != controller {
		t.Fatalf("fill = %+v, %v", step, err)
	}
	held := routedRequest(2, "", time.Time{})
	f.holders[cfoOffice] = []domain.EligibleApprover{person("88")}
	if step, err = router.Escalate(context.Background(), held); err != nil || step.Backup.ID != "88" || !step.KeepDeadline || step.Route.Step != 2 {
		t.Fatalf("a held vacancy is filled at its step: %+v, %v", step, err)
	}
}

func TestEscalateExpiresALostRouteAndPropagatesAnOutage(t *testing.T) {
	_, router := newRouterFixture(t, paymentsRouting)
	gone := routedRequest(0, "77", gateNow)
	gone.Route.RouteID = "retired"
	if step, err := router.Escalate(context.Background(), gone); err != nil || step.Action != domain.EscalationExpire {
		t.Fatalf("a route no longer frozen expires: %+v, %v", step, err)
	}
	router.Releases = &fake.EffectiveReleaseRepository{GetFunc: func(context.Context, int64, string, string) (domain.EffectiveRelease, error) {
		return domain.EffectiveRelease{}, errors.New("database down")
	}}
	if step, err := router.Escalate(context.Background(), routedRequest(0, "77", gateNow)); err == nil || step.Action == domain.EscalationExpire {
		t.Fatalf("an outage must not expire: %+v, %v", step, err)
	}
}

func TestEscalateHandsUnroutedWorkToItsPolicy(t *testing.T) {
	_, router := newRouterFixture(t, paymentsRouting)
	unrouted := owed("turn-1", "alex")
	if step, err := router.Escalate(context.Background(), unrouted); err != nil || step.Resolved() != domain.EscalationExpire {
		t.Fatalf("without a policy: %+v, %v", step, err)
	}
	router.Unrouted = fake.EscalationPolicyFunc(func(context.Context, domain.ApprovalRequest) (domain.EscalationStep, error) {
		return domain.EscalationStep{Backup: person("42").Principal}, nil
	})
	if step, err := router.Escalate(context.Background(), unrouted); err != nil || step.Backup.ID != "42" {
		t.Fatalf("with a policy: %+v, %v", step, err)
	}
}

func TestSanitizeResolutionKeepsWholeRunes(t *testing.T) {
	long := strings.Repeat("é", 200)
	cleaned := sanitizeResolution("a\nb" + long)
	if strings.ContainsRune(cleaned, '\n') || len(cleaned) > maxResolutionBytes || !strings.HasPrefix(cleaned, "ab") || !strings.HasSuffix(cleaned, "é") {
		t.Fatalf("cleaned = %q (%d bytes)", cleaned, len(cleaned))
	}
}
