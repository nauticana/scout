package evaluation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// GateHealthEvaluator answers rollout health from the latest verified, unexpired
// gate decision cross-checked with fresh online metrics; anything missing,
// expired, tampered, or stale is inconclusive, never healthy.
type GateHealthEvaluator struct {
	Decisions contract.GateDecisionStore
	Signer    contract.GateSigner
	Online    contract.OnlineMetricsSource
	// MinOnlineSamples below which online telemetry cannot confirm a verdict.
	MinOnlineSamples int64
	// MaxOnlineAge treats older telemetry as unavailable.
	MaxOnlineAge time.Duration
	Now          func() time.Time
}

var _ contract.DetailedRolloutHealthEvaluator = (*GateHealthEvaluator)(nil)

func (evaluator *GateHealthEvaluator) now() time.Time {
	if evaluator.Now != nil {
		return evaluator.Now()
	}
	return time.Now()
}

// Evaluate returns the three-state verdict with the evidence behind it.
func (evaluator *GateHealthEvaluator) Evaluate(ctx context.Context, target domain.RolloutTarget) (domain.RolloutHealth, error) {
	if evaluator.Decisions == nil || evaluator.Signer == nil || evaluator.Online == nil {
		return domain.RolloutHealth{}, fmt.Errorf("gate health evaluator: decision store, signer, and online metrics are required")
	}
	if evaluator.MinOnlineSamples <= 0 || evaluator.MaxOnlineAge <= 0 {
		return domain.RolloutHealth{}, fmt.Errorf("%w: min online samples and max online age must be positive", domain.ErrValidation)
	}
	if strings.TrimSpace(target.PlatformVersion) == "" {
		return domain.RolloutHealth{}, fmt.Errorf("%w: platform version is required", domain.ErrValidation)
	}
	now := evaluator.now()
	decision, err := evaluator.Decisions.Latest(ctx, target.PlatformVersion)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.RolloutHealth{Verdict: domain.RolloutInconclusive, BreachedMetric: "gate_decision_missing"}, nil
		}
		return domain.RolloutHealth{}, fmt.Errorf("latest gate decision: %w", err)
	}
	if !now.Before(decision.ExpiresAt) {
		return domain.RolloutHealth{Verdict: domain.RolloutInconclusive, BreachedMetric: "gate_decision_expired"}, nil
	}
	if err := VerifyDecision(ctx, evaluator.Signer, decision); err != nil {
		if errors.Is(err, domain.ErrUnauthorized) {
			return domain.RolloutHealth{Verdict: domain.RolloutInconclusive, BreachedMetric: "gate_decision_tampered"}, nil
		}
		return domain.RolloutHealth{}, err
	}
	online, err := evaluator.Online.Online(ctx, target)
	if err != nil {
		return domain.RolloutHealth{}, fmt.Errorf("online metrics: %w", err)
	}
	if online.ObservedAt.IsZero() || now.Sub(online.ObservedAt) > evaluator.MaxOnlineAge {
		return domain.RolloutHealth{Verdict: domain.RolloutInconclusive, BreachedMetric: "online_metrics_stale", Samples: online.Samples, Window: online.Window}, nil
	}
	if online.Samples < evaluator.MinOnlineSamples {
		return domain.RolloutHealth{Verdict: domain.RolloutInconclusive, BreachedMetric: "online_samples_insufficient", Samples: online.Samples, Window: online.Window}, nil
	}
	if len(online.Breached) > 0 {
		return domain.RolloutHealth{Verdict: domain.RolloutUnhealthy, BreachedMetric: online.Breached[0], Samples: online.Samples, Window: online.Window}, nil
	}
	health := domain.RolloutHealth{Verdict: decision.Verdict, Samples: online.Samples, Window: online.Window}
	if decision.Verdict != domain.RolloutHealthy {
		health.BreachedMetric = "gate_decision_" + string(decision.Verdict)
	}
	return health, nil
}
