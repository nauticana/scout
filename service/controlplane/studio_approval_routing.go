package controlplane

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

// maxCheckedReferences bounds the roles and positions one publication checks with the resolver.
const maxCheckedReferences = 256

// StudioApprovalRouting refuses a release that can ask a person for approval of a tool call no
// frozen route serves, and reports an agent's effective routing. A product composes it as a
// StudioService Guard and mounts its view; it must share the resolver the runtime router uses.
type StudioApprovalRouting struct {
	Studio *StudioService
	// Resolver checks every role and position the routes name; nil refuses a release that can ask.
	Resolver contract.ApproverResolver
	// Tools exempts read-only tools from the operating mode's asks; nil counts every tool.
	Tools contract.ToolRegistry
	// Releases reads the settings a restore copies; nil reads them through Studio's database.
	Releases contract.EffectiveReleaseRepository
	// Baseline and Restrictions are the guardrail layers the runtime enforcer adds to every
	// release; their approval rules ask too. A restriction layer changed after publication is not
	// checked against releases already published.
	Baseline     domain.GuardrailRuleSet
	Restrictions contract.RestrictionLayerReader
	// Fallback mirrors a runtime ReleaseRouter.Fallback: a release that binds no approval_routing
	// is routed by it, so its asks need no route.
	Fallback bool
	// MaxRoutes bounds the effective routes a release may freeze; zero takes domain.MaxApprovalRoutes,
	// which it may only lower.
	MaxRoutes int
}

var (
	_ contract.PublishGuard                 = (*StudioApprovalRouting)(nil)
	_ contract.StudioApprovalRoutingBackend = (*StudioApprovalRouting)(nil)
)

// CheckPublish refuses a publish, restore, or activation whose frozen release can ask for an
// approval it cannot route: an ask no route serves, a route an earlier route hides, a role or
// position the product does not know, or no resolver to name its holders.
func (g *StudioApprovalRouting) CheckPublish(ctx context.Context, candidate domain.PublishCandidate) error {
	resources := candidate.Settings.Resources
	if candidate.RestoredFrom != "" && len(resources) == 0 {
		releases := g.Releases
		if releases == nil {
			if g.Studio == nil {
				return fmt.Errorf("studio approval routing: a restore needs the effective release repository or the studio service")
			}
			releases = &scope.TableEffectiveReleaseStore{DB: g.Studio.DB}
		}
		restored, err := releases.Get(ctx, candidate.Actor.TenantID, candidate.Definition.AgentID, candidate.RestoredFrom)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("load restored settings: %w", err)
		}
		resources = restored.Resources
	}
	_, problems, err := g.analyze(ctx, candidate.Actor.TenantID, candidate.Definition, resources)
	if err != nil {
		return err
	}
	if len(problems) == 0 {
		return nil
	}
	fields := make([]domain.AgentFieldError, 0, len(problems))
	for _, problem := range problems {
		fields = append(fields, domain.AgentFieldError{Field: string(domain.ResourceApprovalRouting), Message: problem})
	}
	return &StudioValidationError{Fields: fields}
}

// checkSavedRouting holds an approval_routing value Studio writes to its ApprovalRoutingLimits.
func (s *StudioService) checkSavedRouting(value []byte) error {
	if _, err := s.ApprovalRoutingLimits.Resolve(); err != nil {
		return fmt.Errorf("studio service: %v", err)
	}
	if err := scope.CheckApprovalRoutingLimits(value, s.ApprovalRoutingLimits); err != nil {
		return validationError("value", err.Error())
	}
	return nil
}

// ApprovalRouting returns the agent's effective routes with the scope declaring each, the asks
// its draft can make with the route serving each, and what publication would refuse.
func (g *StudioApprovalRouting) ApprovalRouting(ctx context.Context, actor domain.StudioActor, agentID string) (domain.ApprovalRoutingView, error) {
	if g.Studio == nil {
		return domain.ApprovalRoutingView{}, fmt.Errorf("studio approval routing: studio service is required")
	}
	if err := g.Studio.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return domain.ApprovalRoutingView{}, err
	}
	settings, err := g.Studio.Settings(ctx, actor, agentID)
	if err != nil {
		return domain.ApprovalRoutingView{}, err
	}
	view := domain.ApprovalRoutingView{AgentID: agentID, ResolverComposed: g.Resolver != nil}
	if settings.Problem != "" {
		view.Problems = append(view.Problems, settings.Problem)
		return view, nil
	}
	if view.Routes, err = g.origins(ctx, actor.TenantID, settings); err != nil {
		return domain.ApprovalRoutingView{}, err
	}
	draft, err := g.Studio.draft(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.ApprovalRoutingView{}, err
	}
	definition, release, err := g.Studio.frozenDraft(ctx, actor.TenantID, draft, nil)
	if settingsProblem(err) || errors.Is(err, domain.ErrNoPrompts) {
		view.Problems = append(view.Problems, err.Error())
		return view, nil
	}
	if err != nil {
		return domain.ApprovalRoutingView{}, err
	}
	coverage, problems, err := g.analyze(ctx, actor.TenantID, definition, release.Resources)
	if errors.Is(err, domain.ErrNotReady) {
		problems = append(problems, err.Error())
	} else if err != nil {
		return domain.ApprovalRoutingView{}, err
	}
	view.Coverage, view.Problems = coverage, append(view.Problems, problems...)
	return view, nil
}

// origins pairs each effective route with the widest scope on the chain whose binding declares it.
func (g *StudioApprovalRouting) origins(ctx context.Context, tenantID int64, settings domain.AgentSettings) ([]domain.ApprovalRouteOrigin, error) {
	var effective []domain.EffectiveResource
	for _, explained := range settings.Effective {
		effective = append(effective, domain.EffectiveResource{ResourceKind: explained.ResourceKind, ResourceID: explained.ResourceID, Value: explained.Value})
	}
	routing, bound, err := scope.ApprovalRoutingOf(effective)
	if err != nil || !bound {
		return nil, err
	}
	chain, err := g.Studio.scopeRepository().Chain(ctx, tenantID, settings.Scopes.AgentScopeID)
	if err != nil {
		return nil, err
	}
	rank := make(map[string]int, len(chain))
	for index, node := range chain {
		rank[node.ScopeID] = index
	}
	declared := map[string]string{}
	for _, binding := range settings.Bindings {
		if binding.ResourceKind != domain.ResourceApprovalRouting {
			continue
		}
		var value domain.ApprovalRouting
		if err := json.Unmarshal(binding.Value, &value); err != nil {
			return nil, fmt.Errorf("%w: approval routing bound at %q: %v", domain.ErrConflict, binding.ScopeID, err)
		}
		for _, route := range value.Routes {
			if origin, seen := declared[route.ID]; !seen || rank[binding.ScopeID] < rank[origin] {
				declared[route.ID] = binding.ScopeID
			}
		}
	}
	origins := make([]domain.ApprovalRouteOrigin, 0, len(routing.Routes))
	for _, route := range routing.Routes {
		origin := declared[route.ID]
		origins = append(origins, domain.ApprovalRouteOrigin{Route: route, ScopeID: origin, Inherited: origin != settings.Scopes.AgentScopeID})
	}
	return origins, nil
}

// analyze lists every ask the frozen release can make with the route serving it, and the
// problems that would refuse it. A release that asks nothing needs no resolver.
func (g *StudioApprovalRouting) analyze(ctx context.Context, tenantID int64, definition domain.AgentDefinition, resources []domain.EffectiveResource) ([]domain.ApprovalCoverage, []string, error) {
	limits, err := domain.ApprovalRoutingLimits{MaxRoutes: g.MaxRoutes}.Resolve()
	if err != nil {
		return nil, nil, fmt.Errorf("studio approval routing: %v", err)
	}
	var problems []string
	routing, bound, err := scope.ApprovalRoutingOf(resources)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if len(routing.Routes) > limits.MaxRoutes {
		problems = append(problems, fmt.Sprintf("the agent's effective approval routing holds %d routes, the limit is %d", len(routing.Routes), limits.MaxRoutes))
	}
	fallback := g.Fallback && !bound && err == nil
	asks, err := g.asks(ctx, tenantID, definition, resources)
	if err != nil {
		return nil, nil, err
	}
	coverage := make([]domain.ApprovalCoverage, 0, len(asks))
	for _, ask := range asks {
		if route, found := scope.MatchApprovalRoute(routing, ask.ToolID, ask.Source, ask.RiskTier); found {
			ask.RouteID = route.ID
		} else if !fallback {
			problems = append(problems, fmt.Sprintf("tool %q can ask for approval (%s, %s risk) and no route serves it", ask.ToolID, ask.Source, ask.RiskTier))
		}
		coverage = append(coverage, ask)
	}
	for _, id := range scope.UnreachableRoutes(routing) {
		problems = append(problems, fmt.Sprintf("approval route %q is unreachable behind an earlier route", id))
	}
	if len(asks) == 0 || fallback {
		return coverage, problems, nil
	}
	if g.Resolver == nil {
		return nil, nil, fmt.Errorf("%w: approval routing needs an approver resolver", domain.ErrNotReady)
	}
	references := routeReferences(routing)
	if len(references) > maxCheckedReferences {
		return nil, nil, fmt.Errorf("%w: approval routing names more than %d roles and positions", domain.ErrValidation, maxCheckedReferences)
	}
	for _, reference := range references {
		err := g.Resolver.Check(ctx, tenantID, reference)
		if errors.Is(err, domain.ErrNotFound) {
			problems = append(problems, fmt.Sprintf("unknown role or position %s:%s", reference.Kind, reference.Ref))
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("check %s %q: %w", reference.Kind, reference.Ref, err)
		}
	}
	return coverage, problems, nil
}

// asks lists the distinct approvals the release can request: its operating mode's for every
// tool that acts, its policy's at each tier, and its frozen guardrail approval rules'.
func (g *StudioApprovalRouting) asks(ctx context.Context, tenantID int64, definition domain.AgentDefinition, resources []domain.EffectiveResource) ([]domain.ApprovalCoverage, error) {
	var asks []domain.ApprovalCoverage
	add := func(ask domain.ApprovalCoverage) {
		if !slices.Contains(asks, ask) {
			asks = append(asks, ask)
		}
	}
	mode, windowed, err := scope.AutonomyOf(resources)
	if err != nil {
		return nil, err
	}
	autonomyAsks := mode == domain.AutonomyExecuteWithApproval || (mode == domain.AutonomyBounded && windowed)
	var tools []domain.ToolReference
	for _, tool := range definition.Tools {
		if !slices.ContainsFunc(tools, func(seen domain.ToolReference) bool { return seen.ToolID == tool.ToolID }) {
			tools = append(tools, tool)
		}
	}
	for _, tool := range tools {
		if autonomyAsks {
			readOnly := false
			if g.Tools != nil {
				registered, err := g.Tools.Get(ctx, tenantID, tool.ToolID, tool.Version)
				if err != nil {
					return nil, fmt.Errorf("load tool %q: %w", tool.ToolID, err)
				}
				readOnly = registered.ReadOnly
			}
			if !readOnly {
				add(domain.ApprovalCoverage{ToolID: tool.ToolID, Source: domain.ApprovalSourceAutonomy, RiskTier: domain.DefaultApprovalRiskTier})
			}
		}
		tiers, err := scope.PolicyApprovalRisks(resources, tool.ToolID)
		if err != nil {
			return nil, err
		}
		for _, tier := range tiers {
			add(domain.ApprovalCoverage{ToolID: tool.ToolID, Source: domain.ApprovalSourcePolicy, RiskTier: tier})
		}
	}
	rules, err := g.approvalRules(ctx, tenantID, definition)
	if err != nil {
		return nil, err
	}
	for _, rule := range rules {
		var params struct {
			Tools []string `json:"tools"`
		}
		if len(rule.Params) > 0 {
			if err := json.Unmarshal(rule.Params, &params); err != nil {
				return nil, fmt.Errorf("%w: guardrail rule %q params: %v", domain.ErrConflict, rule.ID, err)
			}
		}
		for _, tool := range tools {
			if len(params.Tools) == 0 || slices.ContainsFunc(params.Tools, func(named string) bool { return strings.TrimSpace(named) == tool.ToolID }) {
				add(domain.ApprovalCoverage{ToolID: tool.ToolID, Source: domain.ApprovalSourceGuardrail, RiskTier: domain.DefaultApprovalRiskTier})
			}
		}
	}
	slices.SortFunc(asks, func(a, b domain.ApprovalCoverage) int {
		return cmp.Or(cmp.Compare(a.ToolID, b.ToolID), cmp.Compare(a.Source, b.Source), cmp.Compare(a.RiskTier, b.RiskTier))
	})
	return asks, nil
}

// approvalRules returns the irreversible_tool_approval rules of every guardrail layer the release
// runs under: the baseline, the platform and tenant restrictions in force, and its own frozen rules.
func (g *StudioApprovalRouting) approvalRules(ctx context.Context, tenantID int64, definition domain.AgentDefinition) ([]domain.GuardrailRule, error) {
	_, frozen, _, err := releaseGuardrails(definition)
	if err != nil {
		return nil, err
	}
	layers := [][]domain.GuardrailRule{g.Baseline.Rules}
	if g.Restrictions != nil {
		restrictions, err := g.Restrictions.Layers(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("read restriction layers: %w", err)
		}
		layers = append(layers, restrictions.Platform.Guardrails, restrictions.Tenant.Guardrails)
	}
	var rules []domain.GuardrailRule
	for _, layer := range append(layers, frozen) {
		for _, rule := range layer {
			if rule.Kind == domain.GuardrailKindIrreversibleToolApproval {
				rules = append(rules, rule)
			}
		}
	}
	return rules, nil
}

func routeReferences(routing domain.ApprovalRouting) []domain.ApproverReference {
	var references []domain.ApproverReference
	add := func(reference domain.ApproverReference) {
		if !slices.Contains(references, reference) {
			references = append(references, reference)
		}
	}
	for _, route := range routing.Routes {
		add(route.Approver)
		for _, step := range route.Escalation {
			if step.Approver != nil {
				add(*step.Approver)
			}
		}
	}
	return references
}
