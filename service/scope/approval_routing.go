package scope

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// The structural bounds every decode enforces; ApprovalRoutingLimits may only tighten them.
const (
	maxRoutesPerBinding     = domain.MaxApprovalRoutesPerBinding
	maxRouteToolPatterns    = domain.MaxApprovalRouteToolPatterns
	maxRouteToolLength      = 120
	maxApproverRefLength    = 128
	minRouteDurationSeconds = domain.MinApprovalRouteDurationSeconds
	maxRouteDurationSeconds = domain.MaxApprovalRouteDurationSeconds
	maxRouteWindowSeconds   = domain.MaxApprovalRouteWindowSeconds
)

var routeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// approvalRiskRank orders the approval risk tiers; an empty minimum is the lowest.
var approvalRiskRank = map[domain.RiskTier]int{
	"": 1, domain.RiskLow: 1, domain.RiskMedium: 2, domain.RiskHigh: 3, domain.RiskCritical: 4,
}

// ApprovalRiskRank orders the approval risk tiers from 1 for low to 4 for critical; an unknown tier is 0.
func ApprovalRiskRank(tier domain.RiskTier) int {
	if tier == "" {
		return 0
	}
	return approvalRiskRank[tier]
}

var approvalSources = map[domain.ApprovalSource]bool{
	domain.ApprovalSourceAutonomy: true, domain.ApprovalSourcePolicy: true, domain.ApprovalSourceGuardrail: true,
}

// DecodeApprovalRouting validates one approval_routing binding value and returns its canonical
// form: tools and sources sorted and de-duplicated, route and step order kept because order is
// precedence, and on_exhausted always written. A route an earlier route covers is refused.
func DecodeApprovalRouting(raw []byte) (domain.ApprovalRouting, error) {
	routing, err := decodeRouting(raw, maxRoutesPerBinding)
	if err != nil {
		return domain.ApprovalRouting{}, err
	}
	if unreachable := UnreachableRoutes(routing); len(unreachable) > 0 {
		return domain.ApprovalRouting{}, fmt.Errorf("%w: approval route %q is unreachable behind an earlier route", domain.ErrValidation, unreachable[0])
	}
	return routing, nil
}

// CheckApprovalRoutingLimits holds one approval_routing binding value to limits tightening the
// bounds DecodeApprovalRouting enforces; it refuses what those bounds would accept but limits do not.
func CheckApprovalRoutingLimits(raw []byte, limits domain.ApprovalRoutingLimits) error {
	limits, err := limits.Resolve()
	if err != nil {
		return err
	}
	routing, err := DecodeApprovalRouting(raw)
	if err != nil {
		return err
	}
	if len(routing.Routes) > limits.MaxRoutesPerBinding {
		return fmt.Errorf("%w: approval routing holds %d routes, the limit is %d", domain.ErrValidation, len(routing.Routes), limits.MaxRoutesPerBinding)
	}
	inRange := func(seconds int64) bool {
		return seconds >= limits.MinDurationSeconds && seconds <= limits.MaxDurationSeconds
	}
	for _, route := range routing.Routes {
		fail := func(format string, args ...any) error {
			return fmt.Errorf("%w: approval route %q: %s", domain.ErrValidation, route.ID, fmt.Sprintf(format, args...))
		}
		if len(route.Match.Tools) > limits.MaxToolPatterns {
			return fail("matches at most %d tool patterns", limits.MaxToolPatterns)
		}
		if !inRange(route.DeadlineSeconds) {
			return fail("deadline_seconds must be from %d to %d", limits.MinDurationSeconds, limits.MaxDurationSeconds)
		}
		window := route.DeadlineSeconds
		for index, step := range route.Escalation {
			if !inRange(step.ExtensionSeconds) {
				return fail("escalation %d extension_seconds must be from %d to %d", index+1, limits.MinDurationSeconds, limits.MaxDurationSeconds)
			}
			window += step.ExtensionSeconds
		}
		if window > limits.MaxWindowSeconds {
			return fail("the deadline and its extensions exceed %d seconds", limits.MaxWindowSeconds)
		}
	}
	return nil
}

// decodeRouting validates and canonicalizes a routing value of at most maxRoutes routes.
func decodeRouting(raw []byte, maxRoutes int) (domain.ApprovalRouting, error) {
	routing := domain.ApprovalRouting{Routes: []domain.ApprovalRoute{}}
	if len(bytes.TrimSpace(raw)) == 0 {
		return routing, nil
	}
	var value domain.ApprovalRouting
	if err := strictDecode(raw, &value); err != nil {
		return routing, fmt.Errorf("%w: approval routing must be a routes object: %v", domain.ErrValidation, err)
	}
	if len(value.Routes) > maxRoutes {
		return routing, fmt.Errorf("%w: approval routing holds %d routes, the limit is %d", domain.ErrValidation, len(value.Routes), maxRoutes)
	}
	seen := make(map[string]bool, len(value.Routes))
	for _, route := range value.Routes {
		canonical, err := canonicalRoute(route)
		if err != nil {
			return routing, err
		}
		if seen[canonical.ID] {
			return routing, fmt.Errorf("%w: duplicate approval route id %q", domain.ErrValidation, canonical.ID)
		}
		seen[canonical.ID] = true
		routing.Routes = append(routing.Routes, canonical)
	}
	return routing, nil
}

func canonicalRoute(route domain.ApprovalRoute) (domain.ApprovalRoute, error) {
	if !routeIDPattern.MatchString(route.ID) {
		return route, fmt.Errorf("%w: approval route id %q must match %s", domain.ErrValidation, route.ID, routeIDPattern)
	}
	fail := func(format string, args ...any) (domain.ApprovalRoute, error) {
		return route, fmt.Errorf("%w: approval route %q: %s", domain.ErrValidation, route.ID, fmt.Sprintf(format, args...))
	}
	if len(route.Match.Tools) > maxRouteToolPatterns {
		return fail("matches at most %d tool patterns", maxRouteToolPatterns)
	}
	tools := make([]string, 0, len(route.Match.Tools))
	for _, tool := range route.Match.Tools {
		if err := validToolPattern(tool); err != nil {
			return fail("%v", err)
		}
		if !slices.Contains(tools, tool) {
			tools = append(tools, tool)
		}
	}
	slices.Sort(tools)
	sources := make([]domain.ApprovalSource, 0, len(route.Match.Sources))
	for _, source := range route.Match.Sources {
		if !approvalSources[source] {
			return fail("unknown approval source %q", source)
		}
		if !slices.Contains(sources, source) {
			sources = append(sources, source)
		}
	}
	slices.Sort(sources)
	if _, known := approvalRiskRank[route.Match.MinRiskTier]; !known {
		return fail("unknown risk tier %q", route.Match.MinRiskTier)
	}
	route.Match.Tools, route.Match.Sources = nilIfEmpty(tools), nilIfEmpty(sources)
	approver, err := canonicalReference(route.Approver)
	if err != nil {
		return fail("%v", err)
	}
	route.Approver = approver
	if !routeDuration(route.DeadlineSeconds) {
		return fail("deadline_seconds must be from %d to %d", minRouteDurationSeconds, maxRouteDurationSeconds)
	}
	if len(route.Escalation) > domain.MaxApprovalEscalationSteps {
		return fail("escalates at most %d steps", domain.MaxApprovalEscalationSteps)
	}
	window := route.DeadlineSeconds
	steps := make([]domain.ApprovalEscalation, 0, len(route.Escalation))
	for index, step := range route.Escalation {
		if !routeDuration(step.ExtensionSeconds) {
			return fail("escalation %d extension_seconds must be from %d to %d", index+1, minRouteDurationSeconds, maxRouteDurationSeconds)
		}
		window += step.ExtensionSeconds
		if step.Approver != nil {
			reference, err := canonicalReference(*step.Approver)
			if err != nil {
				return fail("escalation %d: %v", index+1, err)
			}
			step.Approver = &reference
		}
		steps = append(steps, step)
	}
	if window > maxRouteWindowSeconds {
		return fail("the deadline and its extensions exceed %d seconds", maxRouteWindowSeconds)
	}
	route.Escalation = nilIfEmpty(steps)
	switch route.OnExhausted {
	case "":
		route.OnExhausted = domain.ApprovalExhaustExpire
	case domain.ApprovalExhaustExpire, domain.ApprovalExhaustHold:
	default:
		return fail("on_exhausted must be expire or hold")
	}
	return route, nil
}

func validToolPattern(tool string) error {
	if tool == "" || len(tool) > maxRouteToolLength || strings.TrimSpace(tool) != tool {
		return fmt.Errorf("tool patterns hold 1 to %d characters", maxRouteToolLength)
	}
	if strings.Contains(strings.TrimSuffix(tool, "*"), "*") {
		return fmt.Errorf("tool pattern %q may hold one trailing * only", tool)
	}
	return nil
}

func canonicalReference(reference domain.ApproverReference) (domain.ApproverReference, error) {
	if reference.Kind != domain.ApproverRole && reference.Kind != domain.ApproverPosition {
		return reference, fmt.Errorf("approver kind %q must be role or position", reference.Kind)
	}
	reference.Ref = strings.TrimSpace(reference.Ref)
	if reference.Ref == "" || len(reference.Ref) > maxApproverRefLength || strings.ContainsFunc(reference.Ref, unicode.IsControl) {
		return reference, fmt.Errorf("approver ref must hold 1 to %d characters and no control characters", maxApproverRefLength)
	}
	return reference, nil
}

func routeDuration(seconds int64) bool {
	return seconds >= minRouteDurationSeconds && seconds <= maxRouteDurationSeconds
}

func nilIfEmpty[T any](items []T) []T {
	if len(items) == 0 {
		return nil
	}
	return items
}

func encodeRouting(routing domain.ApprovalRouting) ([]byte, error) {
	if routing.Routes == nil {
		routing.Routes = []domain.ApprovalRoute{}
	}
	return json.Marshal(routing)
}

// UnreachableRoutes returns the ids of routes an earlier route covers, so they never match.
func UnreachableRoutes(routing domain.ApprovalRouting) []string {
	var unreachable []string
	for j, later := range routing.Routes {
		for _, earlier := range routing.Routes[:j] {
			if routeCovers(earlier, later) {
				unreachable = append(unreachable, later.ID)
				break
			}
		}
	}
	return unreachable
}

func routeCovers(earlier, later domain.ApprovalRoute) bool {
	if len(earlier.Match.Tools) > 0 && (len(later.Match.Tools) == 0 || !patternsSubset(earlier.Match.Tools, later.Match.Tools)) {
		return false
	}
	if len(earlier.Match.Sources) > 0 {
		if len(later.Match.Sources) == 0 {
			return false
		}
		for _, source := range later.Match.Sources {
			if !slices.Contains(earlier.Match.Sources, source) {
				return false
			}
		}
	}
	return approvalRiskRank[earlier.Match.MinRiskTier] <= approvalRiskRank[later.Match.MinRiskTier]
}

// MatchApprovalRoute returns the first route serving an ask; an empty risk is the default tier.
func MatchApprovalRoute(routing domain.ApprovalRouting, toolID string, source domain.ApprovalSource, risk domain.RiskTier) (domain.ApprovalRoute, bool) {
	if risk == "" {
		risk = domain.DefaultApprovalRiskTier
	}
	rank, known := approvalRiskRank[risk]
	if !known {
		return domain.ApprovalRoute{}, false
	}
	for _, route := range routing.Routes {
		if len(route.Match.Tools) > 0 && !slices.ContainsFunc(route.Match.Tools, func(pattern string) bool { return patternSubset(toolID, pattern) }) {
			continue
		}
		if len(route.Match.Sources) > 0 && !slices.Contains(route.Match.Sources, source) {
			continue
		}
		if rank >= approvalRiskRank[route.Match.MinRiskTier] {
			return route, true
		}
	}
	return domain.ApprovalRoute{}, false
}

// ApprovalRoutingOf returns the approval routing frozen into resources, or false when they bind
// none. A frozen value that no longer decodes is a conflict, never an empty routing.
func ApprovalRoutingOf(resources []domain.EffectiveResource) (domain.ApprovalRouting, bool, error) {
	for _, resource := range resources {
		if resource.ResourceKind != domain.ResourceApprovalRouting {
			continue
		}
		routing, err := decodeRouting(resource.Value, domain.MaxApprovalRoutes)
		if err != nil {
			return domain.ApprovalRouting{}, true, fmt.Errorf("%w: frozen approval routing: %w", domain.ErrConflict, err)
		}
		return routing, true, nil
	}
	return domain.ApprovalRouting{}, false, nil
}

// ApprovalObligationRisk returns the risk tier a require_approval obligation asks under. Params
// may be absent, {}, or exactly {"risk_tier":"<tier>"}; anything else is refused.
func ApprovalObligationRisk(obligation domain.Obligation) (domain.RiskTier, error) {
	if obligation.Kind != domain.ObligationRequireApproval {
		return "", fmt.Errorf("%w: obligation %q is not require_approval", domain.ErrValidation, obligation.Kind)
	}
	raw := bytes.TrimSpace(obligation.Params)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return domain.DefaultApprovalRiskTier, nil
	}
	var params struct {
		RiskTier *domain.RiskTier `json:"risk_tier"`
	}
	if err := strictDecode(raw, &params); err != nil {
		return "", fmt.Errorf("%w: require_approval params allow only risk_tier: %v", domain.ErrValidation, err)
	}
	if params.RiskTier == nil {
		return domain.DefaultApprovalRiskTier, nil
	}
	if *params.RiskTier == "" || approvalRiskRank[*params.RiskTier] == 0 {
		return "", fmt.Errorf("%w: require_approval risk_tier %q is unknown", domain.ErrValidation, *params.RiskTier)
	}
	return *params.RiskTier, nil
}

// PolicyApprovalRisks returns the distinct risk tiers under which frozen allow statements make
// tool:invoke of toolID require approval. Conditions and denies are ignored, which can only
// count more asks than the runtime makes.
func PolicyApprovalRisks(resources []domain.EffectiveResource, toolID string) ([]domain.RiskTier, error) {
	var tiers []domain.RiskTier
	for _, resource := range resources {
		if resource.ResourceKind != domain.ResourcePolicy {
			continue
		}
		statements, err := decodeStatements(resource.Value)
		if err != nil {
			return nil, err
		}
		for _, statement := range statements {
			if statement.Effect != domain.PolicyAllow ||
				!slices.ContainsFunc(statement.Actions, func(action string) bool { return patternSubset(domain.PolicyActionToolInvoke, action) }) ||
				!slices.ContainsFunc(statement.Resources, func(pattern string) bool { return patternSubset(toolID, pattern) }) {
				continue
			}
			for _, obligation := range statement.Obligations {
				if obligation.Kind != domain.ObligationRequireApproval {
					continue
				}
				tier, err := ApprovalObligationRisk(obligation)
				if err != nil {
					return nil, err
				}
				if !slices.Contains(tiers, tier) {
					tiers = append(tiers, tier)
				}
			}
		}
	}
	slices.SortFunc(tiers, func(a, b domain.RiskTier) int { return approvalRiskRank[a] - approvalRiskRank[b] })
	return tiers, nil
}

// AutonomyOf returns the frozen operating mode and whether it has an operating window; an
// unbound mode is human_only.
func AutonomyOf(resources []domain.EffectiveResource) (domain.AutonomyMode, bool, error) {
	for _, resource := range resources {
		if resource.ResourceKind != domain.ResourceAutonomy {
			continue
		}
		value, err := decodeAutonomy(resource.Value)
		if err != nil {
			return "", false, err
		}
		return value.Mode, value.WindowTo > 0, nil
	}
	return domain.AutonomyHumanOnly, false, nil
}

// RoutingMerger overlays a child's routes on the inherited ones: every parent route keeps its
// place, replaced by the child's route of the same id, and the child's other routes follow. A
// child therefore never removes, reorders, or precedes a parent route.
type RoutingMerger struct{}

func (RoutingMerger) Kind() domain.ResourceKind { return domain.ResourceApprovalRouting }

func (RoutingMerger) Merge(_ context.Context, inherited []byte, override domain.ScopedBinding) ([]byte, error) {
	switch override.MergeMode {
	case domain.MergeReplace, domain.MergeAppend, "":
	default:
		return nil, fmt.Errorf("%w: approval routing overlays its parent; %q is not supported", domain.ErrValidation, override.MergeMode)
	}
	parent, err := decodeRouting(inherited, domain.MaxApprovalRoutes)
	if err != nil {
		return nil, err
	}
	child, err := DecodeApprovalRouting(override.Value)
	if err != nil {
		return nil, err
	}
	own := make(map[string]domain.ApprovalRoute, len(child.Routes))
	for _, route := range child.Routes {
		own[route.ID] = route
	}
	merged := domain.ApprovalRouting{Routes: make([]domain.ApprovalRoute, 0, len(parent.Routes)+len(child.Routes))}
	for _, route := range parent.Routes {
		if replacement, ok := own[route.ID]; ok {
			route = replacement
			delete(own, route.ID)
		}
		merged.Routes = append(merged.Routes, route)
	}
	for _, route := range child.Routes {
		if _, ok := own[route.ID]; ok {
			merged.Routes = append(merged.Routes, route)
		}
	}
	return encodeRouting(merged)
}

// checkRouting holds a candidate to its parent's routes: each in its place with the same match
// and approvers, deadlines no longer, and an exhausted request never held where it expired.
func checkRouting(kind domain.ResourceKind, inherited, candidate []byte) error {
	parent, err := decodeRouting(inherited, domain.MaxApprovalRoutes)
	if err != nil {
		return err
	}
	child, err := decodeRouting(candidate, 4*domain.MaxApprovalRoutes)
	if err != nil {
		return err
	}
	if len(child.Routes) > domain.MaxApprovalRoutes {
		return fmt.Errorf("%w: %s holds %d routes, the limit is %d", domain.ErrAuthorityExceeded, kind, len(child.Routes), domain.MaxApprovalRoutes)
	}
	if len(child.Routes) < len(parent.Routes) {
		return fmt.Errorf("%w: %s drops a route the parent scope sets", domain.ErrAuthorityExceeded, kind)
	}
	for index, want := range parent.Routes {
		got := child.Routes[index]
		if got.ID != want.ID {
			return fmt.Errorf("%w: %s moves or drops parent route %q", domain.ErrAuthorityExceeded, kind, want.ID)
		}
		if !slices.Equal(got.Match.Tools, want.Match.Tools) || !slices.Equal(got.Match.Sources, want.Match.Sources) ||
			got.Match.MinRiskTier != want.Match.MinRiskTier || got.Approver != want.Approver {
			return fmt.Errorf("%w: %s changes what parent route %q matches or who it names", domain.ErrAuthorityExceeded, kind, want.ID)
		}
		if len(got.Escalation) != len(want.Escalation) {
			return fmt.Errorf("%w: %s changes the escalation steps of parent route %q", domain.ErrAuthorityExceeded, kind, want.ID)
		}
		if got.DeadlineSeconds > want.DeadlineSeconds {
			return fmt.Errorf("%w: %s lengthens the deadline of parent route %q", domain.ErrAuthorityExceeded, kind, want.ID)
		}
		for step, parentStep := range want.Escalation {
			childStep := got.Escalation[step]
			if !sameReference(childStep.Approver, parentStep.Approver) {
				return fmt.Errorf("%w: %s changes who escalation %d of parent route %q names", domain.ErrAuthorityExceeded, kind, step+1, want.ID)
			}
			if childStep.ExtensionSeconds > parentStep.ExtensionSeconds {
				return fmt.Errorf("%w: %s lengthens escalation %d of parent route %q", domain.ErrAuthorityExceeded, kind, step+1, want.ID)
			}
		}
		if want.OnExhausted == domain.ApprovalExhaustExpire && got.OnExhausted != domain.ApprovalExhaustExpire {
			return fmt.Errorf("%w: %s holds exhausted requests parent route %q expires", domain.ErrAuthorityExceeded, kind, want.ID)
		}
	}
	return nil
}

func sameReference(left, right *domain.ApproverReference) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

var _ contract.ResourceMerger = RoutingMerger{}
