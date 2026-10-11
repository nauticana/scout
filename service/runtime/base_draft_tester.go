package runtime

import (
	"cmp"
	"context"
	"errors"
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
	// BuildTenant binds a tenant-scoped executor when model governance needs identity.
	BuildTenant func(ctx context.Context, tenantID int64, reference domain.ModelReference, agentID string, sections []domain.CompiledPromptSection) (contract.PricedAgent, error)
	// FallbackLanguage resolves a draft with no prompt in the requested language.
	FallbackLanguage string
	// DefaultTask is the task of a test that sends none; empty takes DefaultTestTask.
	DefaultTask string
	// MinimumCost floors the charge so work that ran is never free.
	MinimumCost int64
	// OnAccountingError observes a ledger failure. The result still stands: an
	// accounting failure must not discard work the owner already saw.
	OnAccountingError func(ctx context.Context, tenantID, cost int64, err error)
	Now               func() time.Time
}

var _ contract.AgentDraftTestExecutor = (*BaseDraftTester)(nil)

func (tester *BaseDraftTester) Execute(ctx context.Context, actor domain.StudioActor, request domain.AgentTestRequest, definition domain.AgentDefinition) (domain.AgentTestResult, error) {
	if tester == nil || tester.Build == nil && tester.BuildTenant == nil {
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
	var agent contract.PricedAgent
	if tester.BuildTenant != nil {
		agent, err = tester.BuildTenant(ctx, actor.TenantID, *definition.Models.Text, definition.AgentID, compiled.Sections)
	} else {
		agent, err = tester.Build(ctx, *definition.Models.Text, definition.AgentID, compiled.Sections)
	}
	if err != nil {
		return domain.AgentTestResult{}, fmt.Errorf("%w: %v", domain.ErrNotReady, err)
	}

	task := strings.TrimSpace(request.Task)
	if task == "" {
		task = cmp.Or(strings.TrimSpace(tester.DefaultTask), DefaultTestTask)
	}
	now := tester.Now
	if now == nil {
		now = time.Now
	}
	started := now()
	output, inputTokens, outputTokens, err := generateWithin(ctx, agent, domain.AgentTask{Task: task, InputData: request.InputData}, request.Limits.MaxDuration)
	if errors.Is(err, domain.ErrExecutionLimit) {
		return domain.AgentTestResult{}, tester.settleStopped(ctx, actor.TenantID, agent, inputTokens, outputTokens, err)
	}
	if err != nil {
		return domain.AgentTestResult{}, err
	}
	latency := now().Sub(started).Milliseconds()

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

// errDraftDuration is the cause of a draft run stopped by its frozen max duration.
var errDraftDuration = errors.New("draft duration elapsed")

// generateWithin bounds the call by the draft's frozen duration; zero adds no deadline, and
// the caller's own cancellation or deadline is returned as is. A stop by the duration keeps
// the partial token counts the provider reported and drops its error text.
func generateWithin(ctx context.Context, agent contract.PricedAgent, task domain.AgentTask, duration time.Duration) (string, int64, int64, error) {
	if duration <= 0 {
		return agent.GenerateText(ctx, task)
	}
	bounded, cancel := context.WithTimeoutCause(ctx, duration, errDraftDuration)
	defer cancel()
	output, inputTokens, outputTokens, err := agent.GenerateText(bounded, task)
	if err != nil && ctx.Err() == nil && errors.Is(context.Cause(bounded), errDraftDuration) {
		return "", inputTokens, outputTokens, fmt.Errorf("%w: draft exceeded its %s duration", domain.ErrExecutionLimit, duration)
	}
	return output, inputTokens, outputTokens, err
}

// settleStopped charges the provider work a duration stop cut short, at least MinimumCost,
// and returns the stop with that usage; a pricing failure replaces the stop, since an
// unpriced stop must not be scored as free.
func (tester *BaseDraftTester) settleStopped(ctx context.Context, tenantID int64, agent contract.PricedAgent, inputTokens, outputTokens int64, stop error) error {
	cost, currency, err := tester.settle(context.WithoutCancel(ctx), tenantID, agent, max(inputTokens, 0), max(outputTokens, 0))
	if err != nil {
		return fmt.Errorf("price draft stopped by its duration: %w", err)
	}
	return &DraftLimitError{Err: stop, Usage: domain.Usage{
		InputTokens: max(inputTokens, 0), OutputTokens: max(outputTokens, 0), CostMinorUnits: cost, Currency: currency,
	}}
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
