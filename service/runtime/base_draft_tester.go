package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// DefaultTestTask is used when the caller sends no task text.
const DefaultTestTask = "Produce a short representative sample of your configured output so the owner can judge tone and format."

// BaseDraftTester runs a compiled draft through a governed provider call:
// quota gate, execution, pricing, and usage accounting. It is complete and
// usable as is — a product supplies the metered resource name and the agent
// builder rather than reimplementing the sequence. Scout records the Studio
// TEST lifecycle event around Execute. It satisfies a contract/studio.go port
// but lives here because Registry already imports controlplane; the reverse
// direction would cycle.
type BaseDraftTester struct {
	// Quota gates and records the spend; nil skips accounting entirely.
	Quota keelport.QuotaService
	// QuotaResource names the metered resource, e.g. "AI_CREDITS".
	QuotaResource string
	// Operation labels the usage ledger entry, e.g. "STUDIO_TEST".
	Operation string
	// Build binds one model and prompt into an executor that prices itself.
	Build func(ctx context.Context, reference domain.ModelReference, agentID string, sections []domain.CompiledPromptSection) (contract.PricedAgent, error)
	// FallbackLanguage resolves a draft with no prompt in the requested language.
	FallbackLanguage string
	// MinimumCost floors the charge so work that ran is never free.
	MinimumCost int64
	// OnAccountingError observes a ledger failure. The result still stands: an
	// accounting failure must not discard work the owner already saw.
	OnAccountingError func(ctx context.Context, tenantID, cost int64, err error)
}

var _ contract.AgentDraftTestExecutor = (*BaseDraftTester)(nil)

func (tester *BaseDraftTester) Execute(ctx context.Context, actor domain.StudioActor, request domain.AgentTestRequest, definition domain.AgentDefinition) (domain.AgentTestResult, error) {
	if tester == nil || tester.Build == nil {
		return domain.AgentTestResult{}, fmt.Errorf("draft tester: an agent builder is required")
	}
	if definition.Models.Text == nil {
		return domain.AgentTestResult{}, fmt.Errorf("%w: definition has no text model", domain.ErrValidation)
	}
	if err := tester.checkQuota(ctx, actor.TenantID); err != nil {
		return domain.AgentTestResult{}, err
	}

	fallback := tester.FallbackLanguage
	if fallback == "" {
		fallback = DefaultAgentLanguage
	}
	compiled, err := SelectLanguage(definition, request.LanguageCode, fallback)
	if err != nil {
		return domain.AgentTestResult{}, err
	}
	agent, err := tester.Build(ctx, *definition.Models.Text, definition.AgentID, compiled.Sections)
	if err != nil {
		return domain.AgentTestResult{}, fmt.Errorf("%w: %v", domain.ErrNotReady, err)
	}

	task := strings.TrimSpace(request.Task)
	if task == "" {
		task = DefaultTestTask
	}
	started := time.Now()
	output, inputTokens, outputTokens, err := agent.GenerateText(ctx, domain.AgentTask{Task: task, InputData: request.InputData})
	if err != nil {
		return domain.AgentTestResult{}, err
	}
	latency := time.Since(started).Milliseconds()

	cost, currency, err := tester.settle(ctx, actor.TenantID, agent, inputTokens, outputTokens)
	if err != nil {
		return domain.AgentTestResult{}, err
	}
	captions := make([]string, 0, len(compiled.Sections))
	for _, section := range compiled.Sections {
		captions = append(captions, section.Caption)
	}
	return domain.AgentTestResult{
		AgentID:      definition.AgentID,
		LanguageCode: compiled.LanguageCode,
		Model:        *definition.Models.Text,
		Digest:       compiled.Digest,
		Output:       output,
		LatencyMs:    latency,
		Usage: domain.Usage{
			InputTokens:    inputTokens,
			OutputTokens:   outputTokens,
			CostMinorUnits: cost,
			Currency:       currency,
		},
		Sections: captions,
	}, nil
}

func (tester *BaseDraftTester) checkQuota(ctx context.Context, tenantID int64) error {
	if tester.Quota == nil || tester.QuotaResource == "" {
		return nil
	}
	allowed, err := tester.Quota.CheckQuota(ctx, tenantID, tester.QuotaResource, 1)
	if err != nil {
		return fmt.Errorf("quota check for tenant %d: %w", tenantID, err)
	}
	if !allowed {
		return fmt.Errorf("%w: %s quota exceeded", domain.ErrRateLimited, tester.QuotaResource)
	}
	return nil
}

// settle prices the call and records it. A pricing failure is fatal — billing
// at zero is never the safe default — but a ledger write failure is reported
// and survived.
func (tester *BaseDraftTester) settle(ctx context.Context, tenantID int64, agent contract.PricedAgent, inputTokens, outputTokens int64) (int64, string, error) {
	cost, currency, err := agent.Cost(ctx, domain.ModelUsage{InputTokens: inputTokens, OutputTokens: outputTokens})
	if err != nil {
		return 0, "", err
	}
	if cost < tester.MinimumCost {
		cost = tester.MinimumCost
	}
	if tester.Quota == nil || tester.QuotaResource == "" {
		return cost, currency, nil
	}
	if err := tester.Quota.LogUsage(context.WithoutCancel(ctx), tenantID, tester.QuotaResource, cost, tester.Operation); err != nil && tester.OnAccountingError != nil {
		tester.OnAccountingError(ctx, tenantID, cost, err)
	}
	return cost, currency, nil
}
