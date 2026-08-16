package evaluation

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/lru"
)

const (
	sampleReasonBase        = "base"
	sampleReasonRisk        = "risk"
	sampleReasonUncertainty = "uncertainty"
	sampleReasonFeedback    = "feedback"
)

// SamplingPolicyEnforcer applies each tenant's SamplingPolicy to content-free
// turn signals: opt-out, redaction, residency, elevated rates for risky or
// uncertain turns, and a hard per-tenant window cap.
type SamplingPolicyEnforcer struct {
	Policies contract.SamplingPolicyRepository
	// RiskThreshold and UncertaintyThreshold select the elevated rates; zero means 0.7 and 0.5.
	RiskThreshold        float64
	UncertaintyThreshold float64
	// Seed makes draws reproducible in tests; Draw overrides the generator entirely.
	Seed int64
	Draw func() float64
	// MaxTrackedTenants bounds the window counters; zero means 10000.
	MaxTrackedTenants int
	Now               func() time.Time

	once     sync.Once
	mu       sync.Mutex
	random   *rand.Rand
	counters *lru.Cache[int64, *sampleWindow]
}

type sampleWindow struct {
	start time.Time
	count int64
}

var _ contract.ProductionSampler = (*SamplingPolicyEnforcer)(nil)

func (enforcer *SamplingPolicyEnforcer) now() time.Time {
	if enforcer.Now != nil {
		return enforcer.Now()
	}
	return time.Now()
}

func (enforcer *SamplingPolicyEnforcer) init() error {
	if enforcer.Policies == nil {
		return fmt.Errorf("sampling policy enforcer: policy repository is required")
	}
	if enforcer.RiskThreshold < 0 || enforcer.RiskThreshold > 1 || enforcer.UncertaintyThreshold < 0 || enforcer.UncertaintyThreshold > 1 || enforcer.MaxTrackedTenants < 0 {
		return fmt.Errorf("%w: sampling thresholds must be in [0,1] and tracked tenants non-negative", domain.ErrValidation)
	}
	enforcer.once.Do(func() {
		size := enforcer.MaxTrackedTenants
		if size == 0 {
			size = 10000
		}
		enforcer.counters = lru.New[int64, *sampleWindow](size, enforcer.now)
		enforcer.random = rand.New(rand.NewPCG(uint64(enforcer.Seed), uint64(enforcer.Seed)^0x5851f42d4c957f2d))
	})
	return nil
}

func (enforcer *SamplingPolicyEnforcer) draw() float64 {
	if enforcer.Draw != nil {
		return enforcer.Draw()
	}
	enforcer.mu.Lock()
	defer enforcer.mu.Unlock()
	return enforcer.random.Float64()
}

// Sample returns the reason a turn was selected, or "" when policy or chance skipped it.
func (enforcer *SamplingPolicyEnforcer) Sample(ctx context.Context, signal domain.SampleSignal) (string, error) {
	if err := enforcer.init(); err != nil {
		return "", err
	}
	tenantID := signal.TenantContext.TenantID
	if tenantID <= 0 || strings.TrimSpace(signal.RequestID) == "" || strings.TrimSpace(signal.AgentID) == "" {
		return "", fmt.Errorf("%w: sample signal needs tenant, request, and agent", domain.ErrValidation)
	}
	policy, err := enforcer.Policies.SamplingPolicyFor(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("sampling policy for tenant %d: %w", tenantID, err)
	}
	if err := validateSamplingPolicy(policy); err != nil {
		return "", err
	}
	if policy.OptOut || (policy.RedactionRequired && !signal.Redacted) || (policy.ResidencyRegion != "" && signal.TenantContext.Region != policy.ResidencyRegion) {
		return "", nil
	}
	rate, reason := policy.BaseRate, sampleReasonBase
	switch {
	case signal.RiskScore >= enforcer.riskThreshold():
		rate, reason = policy.RiskRate, sampleReasonRisk
	case signal.Feedback["escalation"] > 0 || signal.Feedback["retry"] > 0 || signal.Feedback["correction"] > 0:
		rate, reason = policy.RiskRate, sampleReasonFeedback
	case signal.Uncertainty >= enforcer.uncertaintyThreshold():
		rate, reason = policy.UncertaintyRate, sampleReasonUncertainty
	}
	if rate <= 0 || enforcer.draw() >= rate {
		return "", nil
	}
	if !enforcer.admit(tenantID, policy) {
		return "", nil
	}
	return reason, nil
}

// admit consumes one slot of the tenant's window cap.
func (enforcer *SamplingPolicyEnforcer) admit(tenantID int64, policy domain.SamplingPolicy) bool {
	now := enforcer.now()
	enforcer.mu.Lock()
	defer enforcer.mu.Unlock()
	window, ok := enforcer.counters.Get(tenantID)
	if !ok || now.Sub(window.start) >= policy.Window {
		window = &sampleWindow{start: now}
		enforcer.counters.Set(tenantID, window, policy.Window)
	}
	if window.count >= policy.MaxPerWindow {
		return false
	}
	window.count++
	return true
}

func (enforcer *SamplingPolicyEnforcer) riskThreshold() float64 {
	if enforcer.RiskThreshold > 0 {
		return enforcer.RiskThreshold
	}
	return 0.7
}

func (enforcer *SamplingPolicyEnforcer) uncertaintyThreshold() float64 {
	if enforcer.UncertaintyThreshold > 0 {
		return enforcer.UncertaintyThreshold
	}
	return 0.5
}

func validateSamplingPolicy(policy domain.SamplingPolicy) error {
	for _, rate := range []float64{policy.BaseRate, policy.RiskRate, policy.UncertaintyRate} {
		if rate < 0 || rate > 1 {
			return fmt.Errorf("%w: sampling rates must be in [0,1]", domain.ErrValidation)
		}
	}
	if policy.MaxPerWindow <= 0 || policy.Window <= 0 {
		return fmt.Errorf("%w: sampling policy needs a positive per-window cap and window", domain.ErrValidation)
	}
	return nil
}
