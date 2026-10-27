package scope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func route(id, match string, extra ...string) string {
	fields := []string{`"id":"` + id + `"`, `"match":` + match, `"approver":{"kind":"role","ref":"ops"}`, `"deadline_seconds":3600`}
	return "{" + strings.Join(append(fields, extra...), ",") + "}"
}

func routes(items ...string) string { return `{"routes":[` + strings.Join(items, ",") + `]}` }

func decodeCanonical(t *testing.T, raw string) domain.ApprovalRouting {
	t.Helper()
	canonical, err := CanonicalValue(domain.ResourceApprovalRouting, []byte(raw))
	if err != nil {
		t.Fatalf("CanonicalValue(%s): %v", raw, err)
	}
	var routing domain.ApprovalRouting
	if err := json.Unmarshal(canonical, &routing); err != nil {
		t.Fatal(err)
	}
	return routing
}

func TestApprovalRoutingCanonicalForm(t *testing.T) {
	raw := routes(
		route("payments", `{"tools":["wire","pay.*","wire"],"sources":["policy","autonomy","policy"],"min_risk_tier":"high"}`,
			`"escalation":[{"approver":{"kind":"position","ref":" cfo "},"extension_seconds":600},{"extension_seconds":60}]`),
		route("default", `{}`, `"on_exhausted":"hold"`),
	)
	canonical, err := CanonicalValue(domain.ResourceApprovalRouting, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"routes":[{"id":"payments","match":{"tools":["pay.*","wire"],"sources":["autonomy","policy"],"min_risk_tier":"high"},` +
		`"approver":{"kind":"role","ref":"ops"},"deadline_seconds":3600,"escalation":[{"approver":{"kind":"position","ref":"cfo"},"extension_seconds":600},{"extension_seconds":60}],"on_exhausted":"expire"},` +
		`{"id":"default","match":{},"approver":{"kind":"role","ref":"ops"},"deadline_seconds":3600,"on_exhausted":"hold"}]}`
	if string(canonical) != want {
		t.Fatalf("canonical =\n%s\nwant\n%s", canonical, want)
	}
	again, err := CanonicalValue(domain.ResourceApprovalRouting, canonical)
	if err != nil || string(again) != want {
		t.Fatalf("the canonical form is a fixed point: %s, %v", again, err)
	}
	if empty, err := CanonicalValue(domain.ResourceApprovalRouting, []byte(`{"routes":[]}`)); err != nil || string(empty) != `{"routes":[]}` {
		t.Fatalf("empty = %s, %v", empty, err)
	}
}

func TestApprovalRoutingRefusesMalformedValues(t *testing.T) {
	many := make([]string, 0, 33)
	for index := range 33 {
		many = append(many, route(fmt.Sprintf("r%d", index), fmt.Sprintf(`{"tools":["t%d"]}`, index)))
	}
	for name, raw := range map[string]string{
		"unknown field":      `{"routes":[],"extra":1}`,
		"trailing json":      `{"routes":[]} {}`,
		"duplicate id":       routes(route("a", `{"tools":["x"]}`), route("a", `{"tools":["y"]}`)),
		"bad id":             routes(route("Payments", `{}`)),
		"blank tool":         routes(route("a", `{"tools":[""]}`)),
		"inner star":         routes(route("a", `{"tools":["pay*.wire"]}`)),
		"two stars":          routes(route("a", `{"tools":["pay**"]}`)),
		"long tool":          routes(route("a", `{"tools":["`+strings.Repeat("t", 121)+`"]}`)),
		"too many tools":     routes(route("a", `{"tools":["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q"]}`)),
		"unknown source":     routes(route("a", `{"sources":["manual"]}`)),
		"unknown tier":       routes(route("a", `{"min_risk_tier":"extreme"}`)),
		"unknown kind":       `{"routes":[{"id":"a","match":{},"approver":{"kind":"team","ref":"x"},"deadline_seconds":3600}]}`,
		"blank ref":          `{"routes":[{"id":"a","match":{},"approver":{"kind":"role","ref":"  "},"deadline_seconds":3600}]}`,
		"long ref":           `{"routes":[{"id":"a","match":{},"approver":{"kind":"role","ref":"` + strings.Repeat("r", 129) + `"},"deadline_seconds":3600}]}`,
		"control ref":        `{"routes":[{"id":"a","match":{},"approver":{"kind":"role","ref":"a\u0007b"},"deadline_seconds":3600}]}`,
		"short deadline":     `{"routes":[{"id":"a","match":{},"approver":{"kind":"role","ref":"x"},"deadline_seconds":59}]}`,
		"long deadline":      `{"routes":[{"id":"a","match":{},"approver":{"kind":"role","ref":"x"},"deadline_seconds":2592001}]}`,
		"no deadline":        `{"routes":[{"id":"a","match":{},"approver":{"kind":"role","ref":"x"}}]}`,
		"short extension":    routes(route("a", `{}`, `"escalation":[{"extension_seconds":10}]`)),
		"window over 90d":    `{"routes":[{"id":"a","match":{},"approver":{"kind":"role","ref":"x"},"deadline_seconds":2592000,"escalation":[{"extension_seconds":2592000},{"extension_seconds":2592000},{"extension_seconds":60}]}]}`,
		"four steps":         routes(route("a", `{}`, `"escalation":[{"extension_seconds":60},{"extension_seconds":60},{"extension_seconds":60},{"extension_seconds":60}]`)),
		"bad step approver":  routes(route("a", `{}`, `"escalation":[{"approver":{"kind":"role","ref":""},"extension_seconds":60}]`)),
		"unknown exhaustion": routes(route("a", `{}`, `"on_exhausted":"approve"`)),
		"33 routes":          routes(many...),
		"unreachable":        routes(route("all", `{"tools":["pay.*"]}`), route("wire", `{"tools":["pay.wire"],"sources":["policy"]}`)),
	} {
		if _, err := CanonicalValue(domain.ResourceApprovalRouting, []byte(raw)); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: %v, want validation", name, err)
		}
	}
}

func TestUnreachableRoutesNeedEveryDimensionCovered(t *testing.T) {
	for name, test := range map[string]struct {
		earlier, later string
		hidden         bool
	}{
		"broader tools":         {`{"tools":["pay.*"]}`, `{"tools":["pay.wire","pay.card"]}`, true},
		"catch-all":             {`{}`, `{"tools":["x"],"sources":["policy"],"min_risk_tier":"critical"}`, true},
		"narrower tools":        {`{"tools":["pay.wire"]}`, `{"tools":["pay.*"]}`, false},
		"later matches any":     {`{"tools":["pay.*"]}`, `{}`, false},
		"fewer sources":         {`{"sources":["policy"]}`, `{"sources":["policy","guardrail"]}`, false},
		"later any source":      {`{"sources":["policy"]}`, `{}`, false},
		"riskier minimum first": {`{"min_risk_tier":"high"}`, `{"min_risk_tier":"medium"}`, false},
		"lower minimum first":   {`{"min_risk_tier":"medium"}`, `{"min_risk_tier":"critical"}`, true},
	} {
		routing, err := decodeRouting([]byte(routes(route("a", test.earlier), route("b", test.later))), maxRoutesPerBinding)
		if err != nil {
			t.Fatal(err)
		}
		if hidden := len(UnreachableRoutes(routing)) == 1; hidden != test.hidden {
			t.Errorf("%s: hidden = %t", name, hidden)
		}
	}
}

func TestMatchApprovalRouteTakesTheFirstMatch(t *testing.T) {
	routing := decodeCanonical(t, routes(
		route("critical-pay", `{"tools":["pay.*"],"min_risk_tier":"critical"}`),
		route("policy-pay", `{"tools":["pay.wire"],"sources":["policy"]}`),
		route("guardrails", `{"sources":["guardrail"]}`),
	))
	for name, test := range map[string]struct {
		tool   string
		source domain.ApprovalSource
		risk   domain.RiskTier
		want   string
	}{
		"critical first":       {"pay.wire", domain.ApprovalSourcePolicy, domain.RiskCritical, "critical-pay"},
		"policy at high":       {"pay.wire", domain.ApprovalSourcePolicy, domain.RiskHigh, "policy-pay"},
		"default risk is high": {"pay.wire", domain.ApprovalSourcePolicy, "", "policy-pay"},
		"guardrail anywhere":   {"search", domain.ApprovalSourceGuardrail, domain.RiskLow, "guardrails"},
		"autonomy unmatched":   {"pay.wire", domain.ApprovalSourceAutonomy, domain.RiskHigh, ""},
		"prefix is not a tool": {"pay", domain.ApprovalSourcePolicy, domain.RiskCritical, ""},
		"unknown risk":         {"pay.wire", domain.ApprovalSourcePolicy, "extreme", ""},
	} {
		matched, found := MatchApprovalRoute(routing, test.tool, test.source, test.risk)
		if found != (test.want != "") || matched.ID != test.want {
			t.Errorf("%s: matched %q (%t), want %q", name, matched.ID, found, test.want)
		}
	}
}

func TestRoutingMergerKeepsParentRoutesFirst(t *testing.T) {
	parent := routes(route("pay", `{"tools":["pay.*"]}`, `"escalation":[{"extension_seconds":600}]`), route("ops", `{}`))
	child := routes(route("own", `{"tools":["search"]}`), route("pay", `{"tools":["pay.*"]}`, `"deadline_seconds":600`, `"escalation":[{"extension_seconds":60}]`))
	child = strings.Replace(child, `"deadline_seconds":3600,"deadline_seconds":600`, `"deadline_seconds":600`, 1)
	inherited, err := CanonicalValue(domain.ResourceApprovalRouting, []byte(parent))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := RoutingMerger{}.Merge(context.Background(), inherited, domain.ScopedBinding{Value: []byte(child), MergeMode: domain.MergeReplace})
	if err != nil {
		t.Fatal(err)
	}
	var routing domain.ApprovalRouting
	if err := json.Unmarshal(merged, &routing); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, item := range routing.Routes {
		ids = append(ids, item.ID)
	}
	if strings.Join(ids, ",") != "pay,ops,own" || routing.Routes[0].DeadlineSeconds != 600 || routing.Routes[0].Escalation[0].ExtensionSeconds != 60 {
		t.Fatalf("merged = %s", merged)
	}
	if err := (&LatticeChecker{}).CheckNarrowing(context.Background(), domain.ResourceApprovalRouting, inherited, merged); err != nil {
		t.Fatalf("a tightened overlay narrows: %v", err)
	}
	if _, err := (RoutingMerger{}).Merge(context.Background(), inherited, domain.ScopedBinding{Value: []byte(child), MergeMode: domain.MergeIntersect}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("intersect = %v", err)
	}
}

func TestCheckRoutingRefusesEveryLoosening(t *testing.T) {
	parent := routes(
		route("pay", `{"tools":["pay.*"]}`, `"escalation":[{"approver":{"kind":"role","ref":"cfo"},"extension_seconds":600}]`),
		route("ops", `{}`, `"on_exhausted":"hold"`, `"authenticated_only":true`),
	)
	inherited, err := CanonicalValue(domain.ResourceApprovalRouting, []byte(parent))
	if err != nil {
		t.Fatal(err)
	}
	payStep := `"escalation":[{"approver":{"kind":"role","ref":"cfo"},"extension_seconds":600}]`
	many := []string{route("pay", `{"tools":["pay.*"]}`, payStep), route("ops", `{}`, `"on_exhausted":"hold"`, `"authenticated_only":true`)}
	for index := range domain.MaxApprovalRoutes - 1 {
		many = append(many, route(fmt.Sprintf("c%d", index), fmt.Sprintf(`{"tools":["c%d"]}`, index)))
	}
	checker := &LatticeChecker{}
	for name, candidate := range map[string]string{
		"child route first":    routes(route("own", `{"tools":["x"]}`), route("pay", `{"tools":["pay.*"]}`, payStep), route("ops", `{}`, `"on_exhausted":"hold"`, `"authenticated_only":true`)),
		"parent route dropped": routes(route("pay", `{"tools":["pay.*"]}`, payStep)),
		"match changed":        routes(route("pay", `{"tools":["pay.wire"]}`, payStep), route("ops", `{}`, `"on_exhausted":"hold"`, `"authenticated_only":true`)),
		"approver changed":     strings.Replace(routes(route("pay", `{"tools":["pay.*"]}`, payStep), route("ops", `{}`, `"on_exhausted":"hold"`, `"authenticated_only":true`)), `"ref":"ops"`, `"ref":"intern"`, 1),
		"step approver changed": routes(route("pay", `{"tools":["pay.*"]}`, `"escalation":[{"approver":{"kind":"role","ref":"cto"},"extension_seconds":600}]`),
			route("ops", `{}`, `"on_exhausted":"hold"`, `"authenticated_only":true`)),
		"step dropped":     routes(route("pay", `{"tools":["pay.*"]}`), route("ops", `{}`, `"on_exhausted":"hold"`, `"authenticated_only":true`)),
		"longer extension": routes(route("pay", `{"tools":["pay.*"]}`, `"escalation":[{"approver":{"kind":"role","ref":"cfo"},"extension_seconds":601}]`), route("ops", `{}`, `"on_exhausted":"hold"`, `"authenticated_only":true`)),
		"longer deadline":  strings.Replace(routes(route("pay", `{"tools":["pay.*"]}`, payStep), route("ops", `{}`, `"on_exhausted":"hold"`, `"authenticated_only":true`)), `"deadline_seconds":3600`, `"deadline_seconds":3601`, 1),
		"expire made hold": routes(route("pay", `{"tools":["pay.*"]}`, payStep, `"on_exhausted":"hold"`), route("ops", `{}`, `"on_exhausted":"hold"`, `"authenticated_only":true`)),
		"over sixty-four":  routes(many...),
		"links reopened":   routes(route("pay", `{"tools":["pay.*"]}`, payStep), route("ops", `{}`, `"on_exhausted":"hold"`)),
	} {
		if err := checker.CheckNarrowing(context.Background(), domain.ResourceApprovalRouting, inherited, []byte(candidate)); !errors.Is(err, domain.ErrAuthorityExceeded) {
			t.Errorf("%s: %v, want authority exceeded", name, err)
		}
	}
	tighter := strings.Replace(routes(route("pay", `{"tools":["pay.*"]}`, `"escalation":[{"approver":{"kind":"role","ref":"cfo"},"extension_seconds":60}]`),
		route("ops", `{}`, `"on_exhausted":"expire"`, `"authenticated_only":true`), route("own", `{"tools":["x"]}`)), `"deadline_seconds":3600`, `"deadline_seconds":600`, 1)
	if err := checker.CheckNarrowing(context.Background(), domain.ResourceApprovalRouting, inherited, []byte(tighter)); err != nil {
		t.Fatalf("shorter durations, hold made expire, and an added route narrow: %v", err)
	}
}

func TestCompiledRoutingKeepsTheParentsRoutePrecedence(t *testing.T) {
	release, err := compile(t, []domain.ScopedBinding{
		binding("unit", domain.ResourceApprovalRouting, routes(route("payments", `{"tools":["payments.*"]}`))),
		binding("agent", domain.ResourceApprovalRouting, routes(route("transfer", `{"tools":["payments.transfer"]}`))),
	})
	if err != nil {
		t.Fatal(err)
	}
	routing, bound, err := ApprovalRoutingOf(release.Resources)
	if err != nil || !bound {
		t.Fatalf("routing = %+v, %t, %v", routing, bound, err)
	}
	matched, found := MatchApprovalRoute(routing, "payments.transfer", domain.ApprovalSourcePolicy, "")
	if !found || matched.ID != "payments" {
		t.Fatalf("matched = %q, want the parent's route", matched.ID)
	}
	if unreachable := UnreachableRoutes(routing); len(unreachable) != 1 || unreachable[0] != "transfer" {
		t.Fatalf("unreachable = %v", unreachable)
	}
	_, err = compile(t, []domain.ScopedBinding{
		binding("unit", domain.ResourceApprovalRouting, routes(route("payments", `{"tools":["payments.*"]}`)), sealed),
		binding("agent", domain.ResourceApprovalRouting, routes(route("transfer", `{"tools":["payments.transfer"]}`))),
	})
	if !errors.Is(err, domain.ErrSealed) {
		t.Fatalf("a sealed routing: %v", err)
	}
}

func TestApprovalRoutingOfFailsClosedOnAMalformedFrozenValue(t *testing.T) {
	_, bound, err := ApprovalRoutingOf([]domain.EffectiveResource{{ResourceKind: domain.ResourceApprovalRouting, Value: []byte(`{"routes":[{"id":"x"}]}`)}})
	if !bound || !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("bound = %t, err = %v", bound, err)
	}
	if _, bound, err := ApprovalRoutingOf(nil); bound || err != nil {
		t.Fatalf("unbound = %t, %v", bound, err)
	}
}

func TestRequireApprovalParamsAreValidatedNotRewritten(t *testing.T) {
	for _, params := range []string{``, `{}`, `{"risk_tier":"critical"}`, `null`} {
		obligation := domain.Obligation{Kind: domain.ObligationRequireApproval, Params: json.RawMessage(params)}
		if _, err := ApprovalObligationRisk(obligation); err != nil {
			t.Errorf("%q: %v", params, err)
		}
	}
	for _, params := range []string{`{"risk_tier":"critical","approver":"cfo"}`, `{"risk_tier":"extreme"}`, `{"risk_tier":""}`, `[]`, `"high"`} {
		obligation := domain.Obligation{Kind: domain.ObligationRequireApproval, Params: json.RawMessage(params)}
		if _, err := ApprovalObligationRisk(obligation); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%q: %v, want validation", params, err)
		}
		statement := `[{"id":"a","effect":"allow","actions":["tool:invoke"],"resources":["pay"],"obligations":[{"kind":"require_approval","params":` + params + `}]}]`
		if _, err := CanonicalValue(domain.ResourcePolicy, []byte(statement)); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("policy with %q: %v, want validation", params, err)
		}
	}
	risk, err := ApprovalObligationRisk(domain.Obligation{Kind: domain.ObligationRequireApproval})
	if err != nil || risk != domain.RiskHigh {
		t.Fatalf("default risk = %q, %v", risk, err)
	}
	stored := `[{"id":"a","effect":"allow","actions":["tool:invoke"],"resources":["pay"],"obligations":[{"kind":"require_approval","params":{"risk_tier":"critical"}}]}]`
	canonical, err := CanonicalValue(domain.ResourcePolicy, []byte(stored))
	if err != nil || string(canonical) != stored {
		t.Fatalf("policy bytes changed: %s, %v", canonical, err)
	}
}

func TestPolicyApprovalRisksReadEveryMatchingAllow(t *testing.T) {
	resources := []domain.EffectiveResource{
		{ResourceKind: domain.ResourcePolicy, ResourceID: "base", Value: []byte(`[
			{"id":"a","effect":"allow","actions":["tool:*"],"resources":["pay.*"],"obligations":[{"kind":"require_approval","params":{"risk_tier":"critical"}}]},
			{"id":"b","effect":"allow","actions":["tool:invoke"],"resources":["pay.wire"],"obligations":[{"kind":"require_approval"}]},
			{"id":"c","effect":"deny","actions":["tool:invoke"],"resources":["pay.wire"],"obligations":[{"kind":"require_approval","params":{"risk_tier":"low"}}]},
			{"id":"d","effect":"allow","actions":["model:invoke"],"resources":["*"],"obligations":[{"kind":"require_approval","params":{"risk_tier":"low"}}]}]`)},
		{ResourceKind: domain.ResourcePolicy, ResourceID: "extra", Value: []byte(`[
			{"id":"e","effect":"allow","actions":["*"],"resources":["*"],"obligations":[{"kind":"require_approval","params":{"risk_tier":"medium"}}]}]`)},
	}
	tiers, err := PolicyApprovalRisks(resources, "pay.wire")
	if err != nil || fmt.Sprint(tiers) != "[medium high critical]" {
		t.Fatalf("tiers = %v, %v", tiers, err)
	}
	if tiers, err = PolicyApprovalRisks(resources, "search"); err != nil || fmt.Sprint(tiers) != "[medium]" {
		t.Fatalf("search tiers = %v, %v", tiers, err)
	}
}

func TestAutonomyOfDefaultsToHumanOnly(t *testing.T) {
	mode, windowed, err := AutonomyOf(nil)
	if err != nil || mode != domain.AutonomyHumanOnly || windowed {
		t.Fatalf("unbound = %q, %t, %v", mode, windowed, err)
	}
	mode, windowed, err = AutonomyOf([]domain.EffectiveResource{{ResourceKind: domain.ResourceAutonomy,
		Value: []byte(`{"mode":"bounded_autonomous","window_from_minute":480,"window_to_minute":1020}`)}})
	if err != nil || mode != domain.AutonomyBounded || !windowed {
		t.Fatalf("bounded = %q, %t, %v", mode, windowed, err)
	}
}
