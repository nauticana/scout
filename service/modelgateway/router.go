package modelgateway

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// AuditCategoryModelRoute labels the audit event written for every routing decision.
const AuditCategoryModelRoute = domain.DecisionCategoryModelRoute

// Bounded rejection labels reported in the routing audit payload.
const (
	rejectExcluded     = "excluded"
	rejectCapability   = "capability"
	rejectContextLimit = "context_limit"
	rejectRegion       = "region"
	rejectCapacity     = "capacity_unknown"
	rejectUnhealthy    = "unhealthy"
	rejectDraining     = "draining"
	rejectUnpriced     = "unpriced"
	rejectDeadline     = "deadline"
)

// PolicyRouter is the deterministic reference ModelRouter: it filters the tenant's
// catalog by capability, limits, residency, and fresh capacity, then ranks the
// survivors by quality class, affinity, warmth, locality, cost, and predicted latency.
// Degradation is only ever the tenant's explicit RoutingPolicy.Fallbacks.
type PolicyRouter struct {
	Catalog  contract.ModelCandidateCatalog
	Capacity contract.CapacitySnapshotSource
	Policies contract.TenantRoutingPolicyRepository
	// Pricer supplies the minor-unit cost tiebreak; nil skips cost ranking.
	Pricer contract.ModelPricer
	// Affinity is the optional sticky-route hint store.
	Affinity contract.RouteAffinity
	// Audit receives one event per decision when set.
	Audit contract.AuditSink
	// PromptTokens estimates prompt work; nil uses EstimatePromptTokens.
	PromptTokens func([]byte) int64
	// MaxSnapshotAge is the default freshness bound; a policy may tighten or loosen it.
	MaxSnapshotAge time.Duration
	Now            func() time.Time
}

var _ contract.ModelRouter = (*PolicyRouter)(nil)

// NewPolicyRouter builds a router over the required immutable inputs.
func NewPolicyRouter(catalog contract.ModelCandidateCatalog, capacity contract.CapacitySnapshotSource, policies contract.TenantRoutingPolicyRepository, maxSnapshotAge time.Duration) (*PolicyRouter, error) {
	router := &PolicyRouter{Catalog: catalog, Capacity: capacity, Policies: policies, MaxSnapshotAge: maxSnapshotAge}
	if err := router.validate(); err != nil {
		return nil, err
	}
	return router, nil
}

func (router *PolicyRouter) validate() error {
	if router.Catalog == nil || router.Capacity == nil || router.Policies == nil {
		return fmt.Errorf("policy router: catalog, capacity source, and policy repository are required")
	}
	if router.MaxSnapshotAge <= 0 {
		return fmt.Errorf("policy router: max snapshot age must be positive")
	}
	return nil
}

func (router *PolicyRouter) now() time.Time {
	if router.Now == nil {
		return time.Now()
	}
	return router.Now()
}

type scoredCandidate struct {
	candidate domain.ModelCandidate
	affinity  bool
	known     bool
	warm      bool
	local     bool
	cost      int64
	currency  string
	predicted time.Duration
}

type routingInputs struct {
	request    domain.ModelRequest
	policy     domain.RoutingPolicy
	snapshots  map[routeKey]domain.CapacitySnapshot
	maxAge     time.Duration
	now        time.Time
	deadline   time.Time
	sticky     domain.ModelSelection
	hasSticky  bool
	excluded   map[string]struct{}
	prompt     int64
	catalogGen int64
	snapGen    int64
}

// Select returns the best eligible route or the first explicit fallback that is
// eligible; it never substitutes a provider the tenant did not configure.
func (router *PolicyRouter) Select(ctx context.Context, request domain.ModelRequest) (domain.ModelSelection, error) {
	return router.SelectExcluding(ctx, request)
}

// SelectExcluding routes like Select while treating extra route ids as ineligible,
// which is how a hedge obtains attempt diversity.
func (router *PolicyRouter) SelectExcluding(ctx context.Context, request domain.ModelRequest, excludedRouteIDs ...string) (domain.ModelSelection, error) {
	if err := router.validate(); err != nil {
		return domain.ModelSelection{}, err
	}
	if request.TenantContext.TenantID <= 0 || request.MaxOutputTokens <= 0 {
		return domain.ModelSelection{}, fmt.Errorf("%w: tenant and positive max output tokens are required", domain.ErrValidation)
	}
	inputs, err := router.load(ctx, request, excludedRouteIDs)
	if err != nil {
		return domain.ModelSelection{}, err
	}
	candidates, err := router.Catalog.CandidatesFor(ctx, request.TenantContext)
	if err != nil {
		return domain.ModelSelection{}, fmt.Errorf("route candidates for tenant %d: %w", request.TenantContext.TenantID, err)
	}
	inputs.catalogGen = candidates.Generation

	preferred := make([]scoredCandidate, 0, len(candidates.Candidates))
	degraded := make([]scoredCandidate, 0)
	rejected := make(map[string]int)
	for _, candidate := range candidates.Candidates {
		scored, reason, err := router.score(ctx, candidate, inputs)
		if err != nil {
			return domain.ModelSelection{}, err
		}
		if reason != "" {
			rejected[reason]++
			continue
		}
		if candidate.QualityClass >= inputs.policy.MinQualityClass {
			preferred = append(preferred, scored)
		} else {
			degraded = append(degraded, scored)
		}
	}
	generation := combineGenerations(inputs.catalogGen, inputs.snapGen)
	selection, ok := router.choose(preferred, degraded, inputs)
	if !ok {
		err = router.noRoute(rejected)
		return domain.ModelSelection{}, errors.Join(err, router.audit(ctx, request, domain.ModelSelection{RoutingGeneration: generation}, inputs, rejected, err))
	}
	selection.RoutingGeneration = generation
	selection.Reason = fmt.Sprintf("%s catalog=%d snapshot=%d", selection.Reason, inputs.catalogGen, inputs.snapGen)
	if err := router.audit(ctx, request, selection, inputs, rejected, nil); err != nil {
		return domain.ModelSelection{}, err
	}
	if router.Affinity != nil && strings.TrimSpace(request.AffinityKey) != "" {
		router.Affinity.Remember(ctx, request.TenantContext.TenantID, request.AffinityKey, selection)
	}
	return selection, nil
}

func (router *PolicyRouter) load(ctx context.Context, request domain.ModelRequest, excludedRouteIDs []string) (routingInputs, error) {
	tenantID := request.TenantContext.TenantID
	policy, err := router.Policies.RoutingPolicyFor(ctx, tenantID)
	if err != nil {
		return routingInputs{}, fmt.Errorf("routing policy for tenant %d: %w", tenantID, err)
	}
	if policy.MaxSnapshotAge < 0 {
		return routingInputs{}, fmt.Errorf("%w: routing policy max snapshot age cannot be negative", domain.ErrValidation)
	}
	snapshots, err := router.Capacity.Snapshots(ctx)
	if err != nil {
		return routingInputs{}, fmt.Errorf("capacity snapshots: %w", err)
	}
	inputs := routingInputs{
		request:   request,
		policy:    policy,
		snapshots: make(map[routeKey]domain.CapacitySnapshot, len(snapshots)),
		maxAge:    router.MaxSnapshotAge,
		now:       router.now(),
		excluded:  make(map[string]struct{}, len(excludedRouteIDs)+len(request.ExcludedRouteIDs)),
		prompt:    promptTokens(router.PromptTokens, request.Prompt),
	}
	if policy.MaxSnapshotAge > 0 {
		inputs.maxAge = policy.MaxSnapshotAge
	}
	for _, snapshot := range snapshots {
		inputs.snapshots[snapshotRouteKey(snapshot)] = snapshot
		inputs.snapGen = max(inputs.snapGen, snapshot.Generation)
	}
	if deadline, ok := ctx.Deadline(); ok {
		inputs.deadline = deadline
	}
	for _, routeID := range slices.Concat(excludedRouteIDs, request.ExcludedRouteIDs) {
		if routeID = strings.TrimSpace(routeID); routeID != "" {
			inputs.excluded[routeID] = struct{}{}
		}
	}
	if router.Affinity != nil && policy.PreferAffinity && strings.TrimSpace(request.AffinityKey) != "" {
		inputs.sticky, inputs.hasSticky = router.Affinity.Lookup(ctx, tenantID, request.AffinityKey)
	}
	return inputs, nil
}

// score applies the hard filters in a fixed order and returns the first failing
// label; the deadline check runs last so a "deadline" rejection means the candidate
// was otherwise eligible.
func (router *PolicyRouter) score(ctx context.Context, candidate domain.ModelCandidate, inputs routingInputs) (scoredCandidate, string, error) {
	scored := scoredCandidate{candidate: candidate}
	if _, excluded := inputs.excluded[candidate.RouteID]; excluded && candidate.RouteID != "" {
		return scored, rejectExcluded, nil
	}
	for _, required := range inputs.request.RequiredCapabilities {
		if !slices.Contains(candidate.Capabilities, required) {
			return scored, rejectCapability, nil
		}
	}
	if candidate.MaxOutputTokens > 0 && inputs.request.MaxOutputTokens > candidate.MaxOutputTokens ||
		candidate.MaxContextTokens > 0 && inputs.prompt+inputs.request.MaxOutputTokens > candidate.MaxContextTokens {
		return scored, rejectContextLimit, nil
	}
	if len(inputs.policy.AllowedRegions) > 0 && !slices.Contains(inputs.policy.AllowedRegions, candidate.Region) {
		return scored, rejectRegion, nil
	}
	snapshot, ok := inputs.snapshots[candidateRouteKey(candidate)]
	scored.known = ok && !snapshot.ObservedAt.IsZero() && inputs.now.Sub(snapshot.ObservedAt) <= inputs.maxAge
	if scored.known {
		if !snapshot.Healthy {
			return scored, rejectUnhealthy, nil
		}
		if snapshot.Draining {
			return scored, rejectDraining, nil
		}
		scored.warm = snapshot.Warm
		scored.predicted = snapshot.PredictedQueueDelay + snapshot.TimeToFirstToken +
			time.Duration(inputs.request.MaxOutputTokens)*snapshot.TimePerOutputToken
	} else if !inputs.policy.AllowUnknownCapacity {
		return scored, rejectCapacity, nil
	}
	if router.Pricer != nil {
		cost, currency, err := router.Pricer.Cost(ctx, domain.ModelReference{ProviderID: candidate.Provider, ModelID: candidate.Model},
			domain.ModelUsage{InputTokens: inputs.prompt, OutputTokens: inputs.request.MaxOutputTokens})
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return scored, rejectUnpriced, nil
			}
			return scored, "", fmt.Errorf("price route %s/%s: %w", candidate.Provider, candidate.Model, err)
		}
		scored.cost, scored.currency = cost, currency
	}
	scored.local = inputs.request.TenantContext.Region != "" && candidate.Region == inputs.request.TenantContext.Region
	scored.affinity = inputs.hasSticky && selectionRouteKey(inputs.sticky) == candidateRouteKey(candidate)
	if !inputs.deadline.IsZero() && inputs.now.Add(scored.predicted).After(inputs.deadline) {
		return scored, rejectDeadline, nil
	}
	return scored, "", nil
}

func (router *PolicyRouter) choose(preferred, degraded []scoredCandidate, inputs routingInputs) (domain.ModelSelection, bool) {
	if best, ok := bestOf(preferred); ok {
		return router.selectionOf(best, "preferred"), true
	}
	eligible := slices.Concat(preferred, degraded)
	for index, fallback := range inputs.policy.Fallbacks {
		matching := eligible[:0:0]
		for _, scored := range eligible {
			if scored.candidate.Provider == fallback.ProviderID && scored.candidate.Model == fallback.ModelID {
				matching = append(matching, scored)
			}
		}
		if best, ok := bestOf(matching); ok {
			return router.selectionOf(best, fmt.Sprintf("fallback[%d]", index)), true
		}
	}
	return domain.ModelSelection{}, false
}

func (router *PolicyRouter) selectionOf(scored scoredCandidate, tier string) domain.ModelSelection {
	candidate := scored.candidate
	return domain.ModelSelection{
		Provider:     candidate.Provider,
		Model:        candidate.Model,
		ModelVersion: candidate.ModelVersion,
		Region:       candidate.Region,
		RouteID:      candidate.RouteID,
		Reason: fmt.Sprintf("%s quality=%d affinity=%t known=%t warm=%t local=%t cost=%d predicted=%s",
			tier, candidate.QualityClass, scored.affinity, scored.known, scored.warm, scored.local, scored.cost, scored.predicted),
	}
}

func (router *PolicyRouter) noRoute(rejected map[string]int) error {
	total := 0
	for _, count := range rejected {
		total += count
	}
	if total > 0 && rejected[rejectDeadline] == total {
		return fmt.Errorf("%w: every eligible route misses the request deadline", domain.ErrDeadlineInfeasible)
	}
	return fmt.Errorf("%w: %d candidates rejected", domain.ErrNoRoute, total)
}

// bestOf ranks by quality class, affinity, known capacity, warmth, locality, cost,
// predicted latency, then route identity, so equal inputs always yield the same route.
func bestOf(candidates []scoredCandidate) (scoredCandidate, bool) {
	if len(candidates) == 0 {
		return scoredCandidate{}, false
	}
	best := candidates[0]
	for _, other := range candidates[1:] {
		if rankLess(other, best) {
			best = other
		}
	}
	return best, true
}

func rankLess(a, b scoredCandidate) bool {
	if a.candidate.QualityClass != b.candidate.QualityClass {
		return a.candidate.QualityClass > b.candidate.QualityClass
	}
	for _, flag := range [][2]bool{{a.affinity, b.affinity}, {a.known, b.known}, {a.warm, b.warm}, {a.local, b.local}} {
		if flag[0] != flag[1] {
			return flag[0]
		}
	}
	// Costs are comparable only within one currency; otherwise latency decides.
	if a.currency == b.currency && a.cost != b.cost {
		return a.cost < b.cost
	}
	if a.predicted != b.predicted {
		return a.predicted < b.predicted
	}
	return candidateRouteKey(a.candidate).String() < candidateRouteKey(b.candidate).String()
}

// combineGenerations folds the catalog and snapshot generations into one
// non-negative FNV-1a value, so the same inputs yield the same routing generation
// on every replica.
func combineGenerations(catalog, snapshot int64) int64 {
	hash := fnv.New64a()
	var buffer [16]byte
	binary.BigEndian.PutUint64(buffer[:8], uint64(catalog))
	binary.BigEndian.PutUint64(buffer[8:], uint64(snapshot))
	_, _ = hash.Write(buffer[:])
	return int64(hash.Sum64() & math.MaxInt64)
}

type routeAuditPayload struct {
	RequestID          string         `json:"request_id"`
	Provider           string         `json:"provider,omitempty"`
	Model              string         `json:"model,omitempty"`
	ModelVersion       string         `json:"model_version,omitempty"`
	Region             string         `json:"region,omitempty"`
	RouteID            string         `json:"route_id,omitempty"`
	Reason             string         `json:"reason,omitempty"`
	CatalogGeneration  int64          `json:"catalog_generation"`
	SnapshotGeneration int64          `json:"snapshot_generation"`
	RoutingGeneration  int64          `json:"routing_generation"`
	Rejected           map[string]int `json:"rejected,omitempty"`
	Error              string         `json:"error,omitempty"`
}

func (router *PolicyRouter) audit(ctx context.Context, request domain.ModelRequest, selection domain.ModelSelection, inputs routingInputs, rejected map[string]int, cause error) error {
	if router.Audit == nil {
		return nil
	}
	payload := routeAuditPayload{
		RequestID: request.RequestID, Provider: selection.Provider, Model: selection.Model, ModelVersion: selection.ModelVersion,
		Region: selection.Region, RouteID: selection.RouteID, Reason: selection.Reason,
		CatalogGeneration: inputs.catalogGen, SnapshotGeneration: inputs.snapGen, RoutingGeneration: selection.RoutingGeneration,
		Rejected: rejected,
	}
	if cause != nil {
		payload.Error = errorClass(cause)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode routing audit: %w", err)
	}
	outcome := domain.DecisionAllow
	if cause != nil {
		outcome = domain.DecisionDeny
	}
	record := domain.DecisionRecord{
		TenantID: request.TenantContext.TenantID, Principal: request.Principal,
		Category: AuditCategoryModelRoute, Action: "route", Resource: selection.Model,
		ScopeID: request.TenantContext.ScopeID, Outcome: outcome, Reason: selection.Reason,
		RequestID: request.RequestID, Payload: encoded, OccurredAt: inputs.now,
	}
	if err := router.Audit.Record(ctx, record); err != nil {
		return fmt.Errorf("record routing audit: %w", err)
	}
	return nil
}
