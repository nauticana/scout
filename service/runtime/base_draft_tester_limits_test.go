package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// stalledAgent answers only when its context ends, reporting the deadline it ran under
// and the partial usage the provider counted before the stop.
type stalledAgent struct {
	contract.PricedAgent
	deadline   time.Time
	in, out    int64
	cost       int64
	costErr    error
	pricedWith domain.ModelUsage
}

func (agent *stalledAgent) GenerateText(ctx context.Context, _ domain.AgentTask) (string, int64, int64, error) {
	agent.deadline, _ = ctx.Deadline()
	<-ctx.Done()
	return "", agent.in, agent.out, errors.New("provider stream reset: secret upstream detail")
}

func (agent *stalledAgent) Cost(_ context.Context, usage domain.ModelUsage) (int64, string, error) {
	agent.pricedWith = usage
	return agent.cost, "CRD", agent.costErr
}

func stalledTester(agent *stalledAgent, quota *quotaFake) *BaseDraftTester {
	tester := &BaseDraftTester{MinimumCost: 7, Build: func(context.Context, domain.ModelReference, string, []domain.CompiledPromptSection) (contract.PricedAgent, error) {
		return agent, nil
	}}
	if quota != nil {
		tester.Quota, tester.QuotaResource, tester.Operation = quota, "AI_CREDITS", "STUDIO_TEST"
	}
	return tester
}

func frozenDuration(duration time.Duration) domain.AgentTestRequest {
	return domain.AgentTestRequest{Limits: domain.ExecutionLimits{MaxDuration: duration}}
}

func TestBaseDraftTesterStopsAtTheDraftsFrozenDuration(t *testing.T) {
	agent := &stalledAgent{}
	_, err := stalledTester(agent, nil).Execute(context.Background(), domain.StudioActor{TenantID: 3}, frozenDuration(20*time.Millisecond), draftDefinition())
	if !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("err = %v, want the draft's duration limit", err)
	}
	if agent.deadline.IsZero() {
		t.Fatal("the model call ran without the draft's deadline")
	}
	if strings.Contains(err.Error(), "secret upstream detail") {
		t.Fatalf("err = %v carries the provider's error text", err)
	}
}

// Provider work cut short by the draft's duration is priced and logged, never free.
func TestBaseDraftTesterSettlesADraftStoppedByItsDuration(t *testing.T) {
	cases := map[string]struct {
		in, out, cost, want int64
	}{
		"partial usage priced":         {in: 400, out: 50, cost: 90, want: 90},
		"no reported usage is floored": {cost: 0, want: 7},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			quota := &quotaFake{allowed: true}
			agent := &stalledAgent{in: tc.in, out: tc.out, cost: tc.cost}
			_, err := stalledTester(agent, quota).Execute(context.Background(), domain.StudioActor{TenantID: 3}, frozenDuration(10*time.Millisecond), draftDefinition())
			var spent *DraftLimitError
			if !errors.As(err, &spent) || !errors.Is(err, domain.ErrExecutionLimit) {
				t.Fatalf("err = %v, want a DraftLimitError", err)
			}
			got := spent.SpentUsage()
			if got.InputTokens != tc.in || got.OutputTokens != tc.out || got.CostMinorUnits != tc.want || got.Currency != "CRD" || agent.pricedWith != (domain.ModelUsage{InputTokens: tc.in, OutputTokens: tc.out}) {
				t.Fatalf("spent = %+v priced %+v, want %d CRD for %d/%d tokens", got, agent.pricedWith, tc.want, tc.in, tc.out)
			}
			if quota.logged != tc.want || quota.resource != "AI_CREDITS" {
				t.Fatalf("ledger = %d %s, want %d", quota.logged, quota.resource, tc.want)
			}
		})
	}
}

func TestBaseDraftTesterFailsAStoppedDraftItCannotPrice(t *testing.T) {
	quota := &quotaFake{allowed: true}
	agent := &stalledAgent{in: 10, costErr: errors.New("no catalog price")}
	_, err := stalledTester(agent, quota).Execute(context.Background(), domain.StudioActor{TenantID: 3}, frozenDuration(10*time.Millisecond), draftDefinition())
	var spent *DraftLimitError
	if err == nil || errors.Is(err, domain.ErrExecutionLimit) || errors.As(err, &spent) || quota.logged != 0 {
		t.Fatalf("err = %v, logged %d; an unpriced stop must not pass as a scored limit", err, quota.logged)
	}
}

func TestBaseDraftTesterReturnsTheCallersOwnDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	quota := &quotaFake{allowed: true}
	_, err := stalledTester(&stalledAgent{}, quota).Execute(ctx, domain.StudioActor{TenantID: 3}, frozenDuration(time.Hour), draftDefinition())
	var spent *DraftLimitError
	if err == nil || ctx.Err() == nil || errors.Is(err, domain.ErrExecutionLimit) || errors.As(err, &spent) {
		t.Fatalf("err = %v, want the caller's own deadline, not the draft's limit", err)
	}
}

func TestBaseDraftTesterAddsNoDeadlineWhenTheDraftBindsNone(t *testing.T) {
	quota := &quotaFake{allowed: true}
	result, err := testerFor(quota, 5).Execute(context.Background(), domain.StudioActor{TenantID: 3}, domain.AgentTestRequest{}, draftDefinition())
	if err != nil || result.Output != "sample" {
		t.Fatalf("result = %+v, %v", result, err)
	}
}
