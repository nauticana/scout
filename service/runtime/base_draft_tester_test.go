package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type quotaFake struct {
	allowed  bool
	logged   int64
	resource string
	logErr   error
}

func (q *quotaFake) CheckQuota(context.Context, int64, string, int64) (bool, error) {
	return q.allowed, nil
}

func (q *quotaFake) LogUsage(_ context.Context, _ int64, resource string, amount int64, _ string) error {
	q.resource, q.logged = resource, amount
	return q.logErr
}

func (q *quotaFake) ConsumeQuota(context.Context, int64, string, int64, string) (bool, time.Time, error) {
	return q.allowed, time.Time{}, nil
}

func (q *quotaFake) CheckAddon(context.Context, int64, string) (bool, error) { return q.allowed, nil }

func (q *quotaFake) GetPartnerQuota(_ context.Context, _ int64, _ string, def int64) (int64, error) {
	return def, nil
}

func (q *quotaFake) ReportAddonUsage(context.Context, int64, string, int64, string) error { return nil }

func testerFor(quota *quotaFake, cost int64) *BaseDraftTester {
	reference := domain.ModelReference{ProviderID: "anthropic", ModelID: "opus"}
	return &BaseDraftTester{
		Quota: quota, QuotaResource: "AI_CREDITS", Operation: "STUDIO_TEST", MinimumCost: 1,
		Build: func(context.Context, domain.ModelReference, string, []domain.CompiledPromptSection) (contract.PricedAgent, error) {
			return &PricedAgent{
				AgentExecutor: &executorFake{output: "sample", in: 1000, out: 2000, ref: reference},
				Pricer:        &pricerFake{cost: cost},
			}, nil
		},
	}
}

func draftDefinition() domain.AgentDefinition {
	reference := domain.ModelReference{ProviderID: "anthropic", ModelID: "opus"}
	return domain.AgentDefinition{
		AgentID: "writer",
		Models:  domain.AgentModelSelection{Text: &reference},
		Languages: []domain.CompiledPrompt{{
			LanguageCode: DefaultAgentLanguage,
			Digest:       "d",
			Sections:     []domain.CompiledPromptSection{{Caption: "task"}},
		}},
	}
}

func TestBaseDraftTesterGatesRunsAndBills(t *testing.T) {
	quota := &quotaFake{allowed: true}
	result, err := testerFor(quota, 220000).Execute(context.Background(),
		domain.StudioActor{TenantID: 3}, domain.AgentTestRequest{}, draftDefinition())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Output != "sample" || result.Usage.InputTokens != 1000 || result.Usage.OutputTokens != 2000 {
		t.Fatalf("result = %+v", result)
	}
	if result.Usage.CostMinorUnits != 220000 || result.Usage.Currency != "CRD" {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if quota.logged != 220000 || quota.resource != "AI_CREDITS" {
		t.Fatalf("ledger = %d %s", quota.logged, quota.resource)
	}
	if len(result.Sections) != 1 || result.Sections[0] != "task" {
		t.Fatalf("sections = %v", result.Sections)
	}
}

func TestBaseDraftTesterRefusesOverQuota(t *testing.T) {
	_, err := testerFor(&quotaFake{allowed: false}, 1).Execute(context.Background(),
		domain.StudioActor{TenantID: 3}, domain.AgentTestRequest{}, draftDefinition())
	if !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("over-quota error = %v", err)
	}
}

// Work that ran is never free, and a ledger failure never discards the result.
func TestBaseDraftTesterFloorsCostAndSurvivesLedgerFailure(t *testing.T) {
	quota := &quotaFake{allowed: true, logErr: errors.New("ledger down")}
	var observed int64
	tester := testerFor(quota, 0)
	tester.OnAccountingError = func(_ context.Context, _, cost int64, _ error) { observed = cost }

	result, err := tester.Execute(context.Background(),
		domain.StudioActor{TenantID: 3}, domain.AgentTestRequest{}, draftDefinition())
	if err != nil {
		t.Fatalf("a ledger failure must not discard the result: %v", err)
	}
	if result.Usage.CostMinorUnits != 1 || observed != 1 {
		t.Fatalf("cost = %d observed = %d, want the 1-unit floor", result.Usage.CostMinorUnits, observed)
	}
}

func TestBaseDraftTesterRequiresTextModelAndBuilder(t *testing.T) {
	if _, err := (&BaseDraftTester{}).Execute(context.Background(),
		domain.StudioActor{TenantID: 3}, domain.AgentTestRequest{}, draftDefinition()); err == nil {
		t.Fatal("a tester with no builder must fail loudly")
	}
	tester := testerFor(&quotaFake{allowed: true}, 1)
	if _, err := tester.Execute(context.Background(), domain.StudioActor{TenantID: 3},
		domain.AgentTestRequest{}, domain.AgentDefinition{AgentID: "writer"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing text model error = %v", err)
	}
}
