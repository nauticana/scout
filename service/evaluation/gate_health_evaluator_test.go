package evaluation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func healthEvaluator(latest func(context.Context, string) (domain.GateDecision, error), online domain.OnlineMetrics) *GateHealthEvaluator {
	return &GateHealthEvaluator{
		Decisions:        &fake.GateDecisionStore{LatestFunc: latest},
		Signer:           testSigner(),
		Online:           &fake.OnlineMetricsSource{OnlineFunc: func(context.Context, domain.RolloutTarget) (domain.OnlineMetrics, error) { return online, nil }},
		MinOnlineSamples: 100,
		MaxOnlineAge:     30 * time.Minute,
		Now:              fixedClock(testClock),
	}
}

func issuedDecision(t *testing.T, verdict domain.RolloutVerdict, issuedAt time.Time, ttl time.Duration) domain.GateDecision {
	t.Helper()
	manifest := testManifest(t, testExample("a"))
	summary := promotableSummary(manifest.ManifestID)
	if verdict != domain.RolloutHealthy {
		summary.Promotable, summary.Reasons, summary.CriticalFailures = false, []string{"regression"}, 1
	}
	issuer := newIssuer(&fake.GateDecisionStore{}, &fake.AuditSink{})
	issuer.Now, issuer.TTL = fixedClock(issuedAt), ttl
	decision, err := issuer.Issue(context.Background(), manifest, summary, "platform-9", issuedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func freshOnline() domain.OnlineMetrics {
	return domain.OnlineMetrics{Samples: 500, Window: time.Hour, ObservedAt: testClock.Add(-time.Minute)}
}

func TestGateHealthEvaluatorVerdicts(t *testing.T) {
	target := domain.RolloutTarget{PlatformVersion: "platform-9", TenantRing: "ring-1", Percentage: 10}
	healthy := issuedDecision(t, domain.RolloutHealthy, testClock, time.Hour)
	tampered := healthy
	tampered.Deltas = append([]domain.SliceDelta(nil), domain.SliceDelta{Slice: domain.SliceAll, Metric: domain.MetricCorrectness, Delta: 0.9, Samples: 40})
	expired := issuedDecision(t, domain.RolloutHealthy, testClock.Add(-2*time.Hour), time.Hour)
	unhealthy := issuedDecision(t, domain.RolloutUnhealthy, testClock, time.Hour)

	cases := []struct {
		name     string
		decision func(context.Context, string) (domain.GateDecision, error)
		online   domain.OnlineMetrics
		want     domain.RolloutVerdict
		breach   string
	}{
		{"healthy", func(context.Context, string) (domain.GateDecision, error) { return healthy, nil }, freshOnline(), domain.RolloutHealthy, ""},
		{"missing", func(context.Context, string) (domain.GateDecision, error) {
			return domain.GateDecision{}, fmt.Errorf("%w: none", domain.ErrNotFound)
		}, freshOnline(), domain.RolloutInconclusive, "gate_decision_missing"},
		{"expired", func(context.Context, string) (domain.GateDecision, error) { return expired, nil }, freshOnline(), domain.RolloutInconclusive, "gate_decision_expired"},
		{"tampered", func(context.Context, string) (domain.GateDecision, error) { return tampered, nil }, freshOnline(), domain.RolloutInconclusive, "gate_decision_tampered"},
		{"stale telemetry", func(context.Context, string) (domain.GateDecision, error) { return healthy, nil },
			domain.OnlineMetrics{Samples: 500, ObservedAt: testClock.Add(-2 * time.Hour)}, domain.RolloutInconclusive, "online_metrics_stale"},
		{"insufficient samples", func(context.Context, string) (domain.GateDecision, error) { return healthy, nil },
			domain.OnlineMetrics{Samples: 5, ObservedAt: testClock}, domain.RolloutInconclusive, "online_samples_insufficient"},
		{"online breach", func(context.Context, string) (domain.GateDecision, error) { return healthy, nil },
			domain.OnlineMetrics{Samples: 500, ObservedAt: testClock, Breached: []string{"error_rate"}}, domain.RolloutUnhealthy, "error_rate"},
		{"unhealthy decision", func(context.Context, string) (domain.GateDecision, error) { return unhealthy, nil }, freshOnline(), domain.RolloutUnhealthy, "gate_decision_unhealthy"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			health, err := healthEvaluator(testCase.decision, testCase.online).Evaluate(context.Background(), target)
			if err != nil {
				t.Fatal(err)
			}
			if health.Verdict != testCase.want || health.BreachedMetric != testCase.breach {
				t.Fatalf("health = %+v, want %s/%s", health, testCase.want, testCase.breach)
			}
		})
	}
}

func TestGateHealthEvaluatorRequiresConfiguration(t *testing.T) {
	evaluator := healthEvaluator(func(context.Context, string) (domain.GateDecision, error) { return domain.GateDecision{}, nil }, freshOnline())
	evaluator.MinOnlineSamples = 0
	if _, err := evaluator.Evaluate(context.Background(), domain.RolloutTarget{PlatformVersion: "p"}); err == nil {
		t.Fatal("accepted a zero sample floor")
	}
	if _, err := (&GateHealthEvaluator{}).Evaluate(context.Background(), domain.RolloutTarget{PlatformVersion: "p"}); err == nil {
		t.Fatal("accepted missing dependencies")
	}
}
