package dataplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/jsonschema"
	"github.com/nauticana/scout/internal/stage"
)

// ToolLoopExecutor runs model → governed tool call → observation → model until
// the model answers without calling a tool. Every model decision and every tool
// observation is journaled before the loop moves on, so redelivery and approval
// resume replay the journal instead of repeating a decision or an effect. Tools
// are reachable only through the governed gateway. Every non-read-only call is
// recorded in flight before it leaves and settled with its observation; a call
// whose outcome is unknown is never re-sent or failed, it holds the turn.
type ToolLoopExecutor struct {
	Requests contract.ToolLoopRequestBuilder
	Router   contract.ModelRouter
	Models   contract.ModelGateway
	Registry contract.ToolRegistry
	Tools    contract.GovernedToolGateway
	Journal  contract.LoopJournal
	// Outcomes records the outcome of every non-read-only call; required.
	Outcomes contract.CallOutcomeStore
	// Reconciler settles a dispatched call met unobserved, and declares which tools deduplicate by
	// key or accept asynchronously; nil leaves such a call unknown and declares nothing.
	Reconciler contract.ToolOutcomeReconciler
	// Dispatch bounds how long a call may stay in flight, which sets its outcome deadline.
	Dispatch domain.ToolDispatchWindow
	// Pricer is optional; without it MaxCostMinorUnits sees only the cost providers and tools report.
	Pricer contract.ModelPricer
	// Evidence is optional; it is required by a step whose configuration sets require_evidence.
	Evidence contract.EvidenceValidator
	// Revisions is optional; it reads a decider's edit of a parked call so the loop runs the
	// revision instead. Without it an edited call fails the turn rather than run as proposed.
	Revisions contract.ProposalRevisions
	Limits    domain.ToolLoopLimits
	Now       func() time.Time
}

// NewToolLoopExecutor rejects a composition that could run unbounded.
func NewToolLoopExecutor(executor ToolLoopExecutor) (*ToolLoopExecutor, error) {
	if executor.Requests == nil || executor.Router == nil || executor.Models == nil || executor.Registry == nil ||
		executor.Tools == nil || executor.Journal == nil || executor.Outcomes == nil {
		return nil, fmt.Errorf("%w: tool loop needs a request builder, router, model gateway, tool registry, tool gateway, journal, and call outcome store", domain.ErrValidation)
	}
	if err := executor.Dispatch.Validate(); err != nil {
		return nil, fmt.Errorf("tool loop: %w", err)
	}
	limits := executor.Limits
	if limits.MaxIterations <= 0 || limits.MaxToolCalls <= 0 || limits.MaxTokens <= 0 || limits.MaxRepeatedCalls <= 0 ||
		limits.Deadline <= 0 || limits.MaxCostMinorUnits < 0 || limits.MaxCallRevisions < 0 {
		return nil, fmt.Errorf("%w: tool loop iterations, tool calls, tokens, repeated calls, and deadline must be positive, and cost and call revisions not negative", domain.ErrValidation)
	}
	return &executor, nil
}

// LoopError carries the usage a failed loop already spent, so the turn settles
// it instead of refunding work the providers performed.
type LoopError struct {
	Err   error
	Usage domain.Usage
}

func (e *LoopError) Error() string            { return e.Err.Error() }
func (e *LoopError) Unwrap() error            { return e.Err }
func (e *LoopError) SpentUsage() domain.Usage { return e.Usage }

// loopRun is the mutable state of one Execute call.
type loopRun struct {
	input     domain.StepInput
	config    domain.ToolLoopConfig
	limits    domain.ToolLoopLimits
	key       domain.LoopKey
	request   domain.ModelRequest
	tools     map[string]domain.ModelTool
	readOnly  map[string]bool
	windows   map[string]time.Duration
	journal   []domain.LoopEntry
	cursor    int
	usage     domain.Usage
	toolCalls int
	repeats   map[string]int
	events    []domain.TurnEvent
	started   time.Time
	// drained counts the operator entries already given to the model.
	drained int
}

func (executor *ToolLoopExecutor) now() time.Time {
	if executor.Now != nil {
		return executor.Now()
	}
	return time.Now()
}

// Execute runs or resumes the loop for one step.
func (executor *ToolLoopExecutor) Execute(ctx context.Context, input domain.StepInput) (domain.StepResult, error) {
	run, err := executor.begin(ctx, input)
	if err != nil {
		return domain.StepResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, run.limits.Deadline)
	defer cancel()
	result, err := executor.iterate(ctx, run)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w: tool loop exceeded its %s deadline: %w", domain.ErrExecutionLimit, run.limits.Deadline, err)
		}
		return domain.StepResult{}, &LoopError{Err: err, Usage: run.usage}
	}
	return result, nil
}

func (executor *ToolLoopExecutor) begin(ctx context.Context, input domain.StepInput) (*loopRun, error) {
	principal := input.Principal
	if principal.Kind == "" || strings.TrimSpace(principal.ID) == "" || principal.TenantID <= 0 {
		return nil, fmt.Errorf("%w: a tool loop requires a resolved principal", domain.ErrPrincipalUnknown)
	}
	if strings.TrimSpace(principal.Release) == "" {
		return nil, fmt.Errorf("%w: principal %q is not pinned to a release", domain.ErrForbidden, principal.ID)
	}
	if strings.TrimSpace(input.RequestID) == "" || input.Step.ExecutionStepID <= 0 {
		return nil, fmt.Errorf("%w: request id and compiled execution step id are required", domain.ErrValidation)
	}
	run := &loopRun{
		input: input, limits: executor.Limits, repeats: map[string]int{}, started: executor.now(),
		key: domain.LoopKey{TenantID: principal.TenantID, RequestID: input.RequestID, ExecutionStepID: input.Step.ExecutionStepID},
	}
	if len(input.Step.Configuration) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(input.Step.Configuration))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&run.config); err != nil {
			return nil, fmt.Errorf("%w: tool loop configuration: %w", domain.ErrValidation, err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return nil, fmt.Errorf("%w: tool loop configuration has trailing JSON", domain.ErrValidation)
		}
	}
	if run.config.MaxIterations < 0 || run.config.MaxToolCalls < 0 || run.config.MaxTokens < 0 || run.config.MaxCostMinorUnits < 0 ||
		run.config.MaxRepeatedCalls < 0 || run.config.DeadlineSeconds < 0 || run.config.MaxOutputTokens < 0 {
		return nil, fmt.Errorf("%w: tool loop configuration limits cannot be negative", domain.ErrValidation)
	}
	if err := run.config.Search.Validate(); err != nil {
		return nil, fmt.Errorf("tool loop configuration: %w", err)
	}
	run.limits.MaxIterations = narrowed(run.limits.MaxIterations, run.config.MaxIterations)
	run.limits.MaxToolCalls = narrowed(run.limits.MaxToolCalls, run.config.MaxToolCalls)
	run.limits.MaxTokens = narrowed64(run.limits.MaxTokens, run.config.MaxTokens)
	run.limits.MaxCostMinorUnits = narrowedOptional64(run.limits.MaxCostMinorUnits, run.config.MaxCostMinorUnits)
	run.limits.MaxRepeatedCalls = narrowed(run.limits.MaxRepeatedCalls, run.config.MaxRepeatedCalls)
	if requested := time.Duration(run.config.DeadlineSeconds) * time.Second; requested > 0 && requested < run.limits.Deadline {
		run.limits.Deadline = requested
	}
	run.limits = narrowLoopLimits(run.limits, input.Limits)
	run.limits.MaxTokens = narrowed64(run.limits.MaxTokens, input.Spend.Tokens)
	run.limits.MaxCostMinorUnits = narrowedOptional64(run.limits.MaxCostMinorUnits, input.Spend.CostMinorUnits)
	if input.Bounds.BudgetMinorUnits > 0 && strings.TrimSpace(input.Bounds.Currency) == "" {
		return nil, fmt.Errorf("%w: a delegated cost budget requires a currency", domain.ErrValidation)
	}
	if (run.limits.MaxCostMinorUnits > 0 || input.Bounds.BudgetMinorUnits > 0 || input.Spend.PricingRequired) && executor.Pricer == nil {
		return nil, fmt.Errorf("%w: a cost-bounded tool loop requires a model pricer", domain.ErrDegraded)
	}
	if run.config.RequireEvidence && executor.Evidence == nil {
		return nil, fmt.Errorf("%w: step %q requires evidence but no validator is composed", domain.ErrDegraded, input.Step.StepID)
	}
	request, err := executor.Requests.Build(ctx, input, run.config)
	if err != nil {
		return nil, stage.At(domain.StagePrompt, err)
	}
	if request.TenantContext.TenantID != principal.TenantID {
		return nil, fmt.Errorf("%w: loop request tenant %d is not the principal's", domain.ErrForbidden, request.TenantContext.TenantID)
	}
	request.Principal = domain.PrincipalRef{Kind: principal.Kind, ID: principal.ID}
	request.RequiredCapabilities = append(request.RequiredCapabilities, run.config.Capabilities...)
	if run.config.MaxOutputTokens > 0 && run.config.MaxOutputTokens < request.MaxOutputTokens {
		request.MaxOutputTokens = run.config.MaxOutputTokens
	}
	if len(run.config.OutputSchema) > 0 {
		request.Output = domain.OutputConstraint{Mode: domain.OutputModeJSONSchema, SchemaName: run.config.OutputSchemaName, Schema: run.config.OutputSchema}
	}
	bound, err := executor.Registry.List(ctx, principal.TenantID, principal.ID, principal.Release)
	if err != nil {
		return nil, fmt.Errorf("list tools bound to %s@%s: %w", principal.ID, principal.Release, err)
	}
	run.tools, run.readOnly = make(map[string]domain.ModelTool, len(bound)), make(map[string]bool, len(bound))
	run.windows = make(map[string]time.Duration, len(bound))
	request.Tools = request.Tools[:0]
	for _, definition := range bound {
		tool := domain.ModelTool{
			Name: modelToolName(definition.ToolID), Description: definition.DisplayName,
			ToolID: definition.ToolID, ToolVersion: definition.Version, InputSchema: definition.InputSchema,
		}
		if _, collides := run.tools[tool.Name]; collides {
			return nil, fmt.Errorf("%w: tools of %s@%s collide on model name %q", domain.ErrConflict, principal.ID, principal.Release, tool.Name)
		}
		run.tools[tool.Name], run.readOnly[tool.Name] = tool, definition.ReadOnly
		run.windows[tool.Name] = executor.Dispatch.For(definition)
		request.Tools = append(request.Tools, tool)
	}
	run.request = request
	if run.journal, err = executor.Journal.Load(ctx, run.key); err != nil {
		return nil, stage.At(domain.StageCheckpoint, fmt.Errorf("load loop journal: %w", err))
	}
	return run, nil
}

// modelToolName maps a tool id onto the [a-zA-Z0-9_-]{1,64} alphabet every provider accepts.
func modelToolName(toolID string) string {
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, toolID)
	return common.TruncateRunes(name, 64)
}

func narrowed(limit, requested int) int {
	if requested > 0 && requested < limit {
		return requested
	}
	return limit
}

func narrowed64(limit, requested int64) int64 {
	if requested > 0 && requested < limit {
		return requested
	}
	return limit
}

// narrowedOptional64 treats zero as unbounded at the executor level. A
// positive step value therefore narrows zero, but cannot widen a positive cap.
func narrowedOptional64(limit, requested int64) int64 {
	if requested > 0 && (limit == 0 || requested < limit) {
		return requested
	}
	return limit
}

func (executor *ToolLoopExecutor) iterate(ctx context.Context, run *loopRun) (domain.StepResult, error) {
	for iteration := 1; ; iteration++ {
		if iteration > run.limits.MaxIterations {
			return domain.StepResult{}, fmt.Errorf("%w: tool loop exceeded %d iterations", domain.ErrExecutionLimit, run.limits.MaxIterations)
		}
		if err := ctx.Err(); err != nil {
			return domain.StepResult{}, err
		}
		// Every observation of the last iteration is journaled: a durable boundary to hold at.
		if iteration > 1 && run.holdRequested() {
			return domain.StepResult{}, fmt.Errorf("%w: before loop iteration %d", domain.ErrTurnHeld, iteration)
		}
		run.drainOperator()
		decision, err := executor.decide(ctx, run, iteration)
		if err != nil {
			return domain.StepResult{}, err
		}
		if len(decision.ToolCalls) == 0 {
			return executor.finish(ctx, run, decision)
		}
		// The model is shown the arguments each call ran with, which a decider may have revised;
		// the journaled decision itself is never changed.
		calls := append([]domain.ModelToolCall(nil), decision.ToolCalls...)
		run.request.Messages = append(run.request.Messages, domain.ModelMessage{
			Role: domain.ModelRoleAssistant, Text: decision.Output, ToolCalls: calls,
		})
		observations := make([]domain.ModelToolObservation, 0, len(decision.ToolCalls))
		for index, call := range decision.ToolCalls {
			observation, ran, err := executor.observe(ctx, run, iteration, call)
			if err != nil {
				return domain.StepResult{}, err
			}
			calls[index].Arguments = ran.Arguments
			observations = append(observations, observation)
		}
		run.request.Messages = append(run.request.Messages, domain.ModelMessage{Role: domain.ModelRoleTool, Observations: observations})
	}
}

// decide returns the journaled model decision of this iteration, or makes and
// journals a new one. A journaled decision is never re-asked: its tool calls
// are what approvals and idempotency keys were bound to.
func (executor *ToolLoopExecutor) decide(ctx context.Context, run *loopRun, iteration int) (domain.ModelResult, error) {
	if entry, replayed := run.next(domain.LoopEntryModel); replayed {
		if entry.Model == nil {
			return domain.ModelResult{}, fmt.Errorf("%w: loop journal entry %d has no model decision", domain.ErrConflict, entry.EntryNo)
		}
		return *entry.Model, run.account(entry.Usage)
	}
	ctx = domain.WithDecisionScope(ctx, "iteration/", iteration)
	selection, err := executor.Router.Select(ctx, run.request)
	if err != nil {
		return domain.ModelResult{}, stage.At(domain.StageModel, err)
	}
	result, err := executor.Models.Generate(ctx, selection, run.request)
	usage, priceErr := executor.priced(ctx, selection, result.Usage)
	if err != nil {
		return domain.ModelResult{}, errors.Join(stage.At(domain.StageModel, err), stage.At(domain.StageModel, priceErr), run.account(usage))
	}
	if priceErr != nil {
		return domain.ModelResult{}, errors.Join(stage.At(domain.StageModel, priceErr), run.account(usage))
	}
	for _, call := range result.ToolCalls {
		if _, offered := run.tools[call.Name]; !offered {
			return domain.ModelResult{}, errors.Join(run.account(usage),
				fmt.Errorf("%w: model called tool %q, which is not bound to the release", domain.ErrInvalidModelOutput, call.Name))
		}
	}
	if err := run.distinctCalls(result.ToolCalls); err != nil {
		return domain.ModelResult{}, errors.Join(run.account(usage), err)
	}
	entry, err := executor.append(ctx, run, domain.LoopEntry{Kind: domain.LoopEntryModel, Iteration: iteration, Model: &result, Usage: usage})
	if err != nil {
		return domain.ModelResult{}, errors.Join(err, run.account(usage))
	}
	return *entry.Model, run.account(entry.Usage)
}

// priced attributes the call's usage to the selected model unless the gateway named
// the models that performed it, then prices each model's share when no cost was reported.
func (executor *ToolLoopExecutor) priced(ctx context.Context, selection domain.ModelSelection, usage domain.Usage) (domain.Usage, error) {
	if len(usage.Models) == 0 {
		usage = usage.AttributedTo(domain.ModelReference{ProviderID: selection.Provider, ModelID: selection.Model})
	}
	if executor.Pricer == nil || usage.CostMinorUnits > 0 {
		return usage, nil
	}
	spends := slices.Clone(usage.Models)
	if len(spends) == 0 {
		spends = []domain.ModelSpend{{Model: domain.ModelReference{ProviderID: selection.Provider, ModelID: selection.Model}}}
	}
	var total int64
	currency := ""
	for i, spend := range spends {
		cost, spendCurrency, err := executor.Pricer.Cost(ctx, spend.Model,
			domain.ModelUsage{InputTokens: spend.InputTokens, OutputTokens: spend.OutputTokens, SearchQueries: spend.SearchQueries})
		if err != nil {
			return usage, fmt.Errorf("price model %s/%s: %w", spend.Model.ProviderID, spend.Model.ModelID, err)
		}
		if cost < 0 || cost > 0 && strings.TrimSpace(spendCurrency) == "" || cost > math.MaxInt64-total ||
			spendCurrency != "" && currency != "" && spendCurrency != currency {
			return usage, fmt.Errorf("%w: model pricer returned invalid cost", domain.ErrValidation)
		}
		if currency == "" {
			currency = spendCurrency
		}
		total += cost
		spends[i].CostMinorUnits = cost
	}
	usage.CostMinorUnits = total
	if currency != "" {
		usage.Currency = currency
	}
	if len(usage.Models) > 0 {
		usage.Models = spends
	}
	return usage, nil
}

// observe returns the journaled observation of one proposed call, or invokes
// the tool through the governed gateway and journals what came back. It also
// returns the call as it ran: a decider may have revised a parked call's arguments.
func (executor *ToolLoopExecutor) observe(ctx context.Context, run *loopRun, iteration int, call domain.ModelToolCall) (domain.ModelToolObservation, domain.ModelToolCall, error) {
	tool := run.tools[call.Name]
	reference := domain.ToolReference{ToolID: tool.ToolID, Version: tool.ToolVersion}
	proposal := domain.ToolCall{
		TenantContext: run.request.TenantContext, Principal: run.input.Principal, OnBehalfOf: run.input.OnBehalfOf,
		RequestID: run.input.RequestID, ConversationID: run.request.ConversationID,
		ToolID: tool.ToolID, ToolVersion: tool.ToolVersion, Arguments: call.Arguments,
		IdempotencyKey: callIdempotencyKey(run.key, call),
	}
	wasPending, maxRevisions := false, positiveOr(run.limits.MaxCallRevisions, defaultCallRevisions)
	for revisions := 0; ; {
		entry, replayed := run.next(domain.LoopEntryApprovalPending)
		if !replayed {
			break
		}
		if entry.Observation == nil || entry.Observation.CallID != call.CallID {
			return domain.ModelToolObservation{}, call, fmt.Errorf("%w: loop journal entry %d does not match call %q", domain.ErrConflict, entry.EntryNo, call.CallID)
		}
		wasPending = true
		revision, revised, err := executor.revision(ctx, run, iteration, reference, call, proposal)
		if err != nil {
			return domain.ModelToolObservation{}, call, err
		}
		if !revised {
			break
		}
		if revisions++; revisions > maxRevisions {
			return domain.ModelToolObservation{}, call, fmt.Errorf("%w: call %q was revised more than %d times", domain.ErrExecutionLimit, call.CallID, maxRevisions)
		}
		run.recordRevision(call, reference)
		call.Arguments, proposal = revision.Arguments, revisedCall(proposal, revision)
		wasPending = false
	}
	if entry, replayed := run.next(domain.LoopEntryObservation); replayed {
		if entry.Observation == nil || entry.Observation.CallID != call.CallID {
			return domain.ModelToolObservation{}, call, fmt.Errorf("%w: loop journal entry %d does not match call %q", domain.ErrConflict, entry.EntryNo, call.CallID)
		}
		run.recordCall(call, reference, entry, wasPending)
		return *entry.Observation, call, run.account(entry.Usage)
	}
	if withdrawal, withdrawn := run.withdrawal(call.CallID); withdrawn {
		observation, err := executor.withdraw(ctx, run, iteration, call, reference, withdrawal, wasPending)
		return observation, call, err
	}
	mutating := !run.readOnly[call.Name]
	sent := outgoingCall{key: run.callKey(call), reference: reference, call: call, proposal: proposal, iteration: iteration, wasPending: wasPending}
	resend := false
	if mutating {
		// An unobserved call at the cursor may already have left: its record decides, never a resend.
		recorded, found, err := executor.Outcomes.Find(ctx, sent.key)
		if err != nil {
			return domain.ModelToolObservation{}, call, stage.At(domain.StageCheckpoint, err)
		}
		if found {
			observation, handled, err := executor.resume(ctx, run, sent, recorded)
			if handled || err != nil {
				return observation, call, err
			}
			resend, sent.resend = true, true
		}
	}
	if err := run.admitCall(call); err != nil {
		return domain.ModelToolObservation{}, call, err
	}
	// The decision is journaled and the call has not left: a mutation is held here, before its effect.
	if mutating && run.holdRequested() {
		return domain.ModelToolObservation{}, call, fmt.Errorf("%w: before call %q", domain.ErrTurnHeld, call.CallID)
	}
	if mutating && !resend {
		// Intent before effect: a call without a record never left.
		if _, _, err := executor.Outcomes.Intend(ctx, run.input.Owner, domain.CallIntent{
			Key: sent.key, IdempotencyKey: proposal.IdempotencyKey, Tool: reference, Window: run.windows[call.Name],
		}); err != nil {
			return domain.ModelToolObservation{}, call, stage.At(domain.StageCheckpoint, fmt.Errorf("record intent of call %q: %w", call.CallID, err))
		}
	}
	result, err := executor.Tools.Invoke(domain.WithDecisionScope(ctx, "iteration/", iteration, "/call/", call.CallID), proposal)
	if mutating {
		observation, err := executor.settleSent(ctx, run, sent, result, err)
		return observation, call, err
	}
	observation := domain.ModelToolObservation{CallID: call.CallID, Name: call.Name, Output: result.Output}
	switch {
	case err == nil:
	case errors.Is(err, domain.ErrApprovalPending):
		if !wasPending {
			pending := domain.LoopEntry{Kind: domain.LoopEntryApprovalPending, Iteration: iteration, Tool: reference,
				Observation: &domain.ModelToolObservation{CallID: call.CallID, Name: call.Name}}
			if _, appendErr := executor.append(ctx, run, pending); appendErr != nil {
				return domain.ModelToolObservation{}, call, errors.Join(err, appendErr)
			}
		}
		return domain.ModelToolObservation{}, call, err
	case endsLoop(ctx, err):
		return domain.ModelToolObservation{}, call, stage.At(domain.StageTool, err)
	default:
		// The model sees the failure class, never the internal error text.
		observation.IsError = true
		observation.Output, _ = json.Marshal(map[string]string{"error": stage.ErrorClass(err)})
	}
	usage := result.Usage
	usage.ToolCalls = max(usage.ToolCalls, 1)
	entry, err := executor.append(ctx, run, domain.LoopEntry{
		Kind: domain.LoopEntryObservation, Iteration: iteration, Tool: reference, Observation: &observation, Usage: usage,
		ResourceURIs: resourceURIs(result.Evidence), Effect: result.Effect,
	})
	if err != nil {
		return domain.ModelToolObservation{}, call, errors.Join(err, run.account(usage))
	}
	run.recordCall(call, reference, entry, wasPending)
	return *entry.Observation, call, run.account(entry.Usage)
}

// endsLoop reports failures the model must not be asked to work around: the
// limits, identity, and authority of the turn itself.
func endsLoop(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	for _, fatal := range []error{
		context.Canceled, context.DeadlineExceeded, domain.ErrTurnCanceled, domain.ErrBudgetExceeded, domain.ErrExecutionLimit,
		domain.ErrLoopDetected, domain.ErrPrincipalUnknown, domain.ErrAuthorityExceeded, domain.ErrDelegationDepth, domain.ErrGrantExpired,
		domain.ErrProposalRevised,
	} {
		if errors.Is(err, fatal) {
			return true
		}
	}
	return false
}

func (executor *ToolLoopExecutor) finish(ctx context.Context, run *loopRun, decision domain.ModelResult) (domain.StepResult, error) {
	if run.config.RequireEvidence {
		if err := executor.requireEvidence(ctx, run, decision.Output); err != nil {
			return domain.StepResult{}, err
		}
	}
	run.events = append(run.events, domain.TurnEvent{Version: domain.TurnEventVersion, Kind: domain.TurnEventResult, Text: string(decision.Output)})
	citations := run.citations()
	if len(citations) > 0 {
		run.events = append(run.events, domain.TurnEvent{Version: domain.TurnEventVersion, Kind: domain.TurnEventCitations, Citations: citations})
	}
	return domain.StepResult{
		State: decision.Output, NextStepID: run.config.NextStepID, Fingerprint: DigestBytes(decision.Output),
		Usage: run.usage, Citations: citations, Events: run.events,
	}, nil
}

// citations merges the sources of every grounded model call of the loop, in
// first-seen order, renumbered across calls.
func (run *loopRun) citations() []domain.Citation {
	var merged []domain.Citation
	seen := map[string]bool{}
	for _, entry := range run.journal {
		if entry.Kind != domain.LoopEntryModel || entry.Model == nil {
			continue
		}
		for _, citation := range entry.Model.Citations {
			if seen[citation.URL] {
				continue
			}
			seen[citation.URL] = true
			citation.Position = len(merged) + 1
			merged = append(merged, citation)
		}
	}
	return merged
}

func (executor *ToolLoopExecutor) requireEvidence(ctx context.Context, run *loopRun, output []byte) error {
	var answer domain.EvidencedAnswer
	if err := json.Unmarshal(output, &answer); err != nil {
		return fmt.Errorf("%w: answer is not an evidenced answer: %w", domain.ErrInvalidModelOutput, err)
	}
	report, err := executor.Evidence.Validate(ctx, run.evidenceScope(), answer)
	if err != nil {
		return stage.At(domain.StageGuardrail, err)
	}
	if len(report.Unsupported) > 0 {
		return stage.At(domain.StageGuardrail, unsupportedClaim(report.Unsupported[0]))
	}
	run.events = append(run.events, domain.TurnEvent{Version: domain.TurnEventVersion, Kind: domain.TurnEventEvidence, Evidence: answer.Evidence})
	return nil
}

// unsupportedClaim names the first failing claim by its index, or by its text when the
// answer states it without declaring it.
func unsupportedClaim(finding domain.ClaimFinding) error {
	if finding.ClaimIndex < 0 {
		return fmt.Errorf("%w: undeclared claim %q (%s)", domain.ErrUnsupportedClaim, finding.Claim, finding.Reason)
	}
	return fmt.Errorf("%w: claim %d (%s)", domain.ErrUnsupportedClaim, finding.ClaimIndex, finding.Reason)
}

func (run *loopRun) holdRequested() bool {
	return run.input.HoldRequested != nil && run.input.HoldRequested()
}

// distinctCalls refuses a decision that reuses a call id of this step: the id is what the call's
// idempotency key, approval, and outcome record are bound to.
func (run *loopRun) distinctCalls(calls []domain.ModelToolCall) error {
	seen := map[string]bool{}
	for _, entry := range run.journal {
		if entry.Kind == domain.LoopEntryModel && entry.Model != nil {
			for _, call := range entry.Model.ToolCalls {
				seen[call.CallID] = true
			}
		}
	}
	for _, call := range calls {
		if strings.TrimSpace(call.CallID) == "" || seen[call.CallID] {
			return fmt.Errorf("%w: model call id %q is empty or already used in this step", domain.ErrInvalidModelOutput, call.CallID)
		}
		seen[call.CallID] = true
	}
	return nil
}

// next consumes the journal entry at the cursor when it is of the wanted kind.
func (run *loopRun) next(kind domain.LoopEntryKind) (domain.LoopEntry, bool) {
	if run.cursor >= len(run.journal) || run.journal[run.cursor].Kind != kind {
		return domain.LoopEntry{}, false
	}
	entry := run.journal[run.cursor]
	run.cursor++
	return entry, true
}

func (executor *ToolLoopExecutor) append(ctx context.Context, run *loopRun, entry domain.LoopEntry) (domain.LoopEntry, error) {
	if run.cursor != len(run.journal) {
		return domain.LoopEntry{}, fmt.Errorf("%w: loop journal diverged at entry %d", domain.ErrConflict, run.cursor+1)
	}
	entry.EntryNo = len(run.journal) + 1
	entry.Offset = executor.now().Sub(run.started)
	stored, err := executor.Journal.Append(context.WithoutCancel(ctx), run.input.Owner, run.key, entry)
	if err != nil {
		return domain.LoopEntry{}, stage.At(domain.StageCheckpoint, fmt.Errorf("append loop journal entry %d: %w", entry.EntryNo, err))
	}
	if !sameLoopEntry(stored, entry) {
		return domain.LoopEntry{}, fmt.Errorf("%w: another worker journaled different content at entry %d", domain.ErrConflict, entry.EntryNo)
	}
	run.journal = append(run.journal, stored)
	run.cursor++
	return stored, nil
}

func sameLoopEntry(stored, offered domain.LoopEntry) bool {
	// Offset is diagnostic timing from each worker's local start. The first
	// writer's value wins and does not make otherwise identical content diverge.
	stored.Offset, offered.Offset = 0, 0
	return reflect.DeepEqual(stored, offered)
}

// admitCall enforces the tool-call and repeated-fingerprint limits before a new call leaves.
func (run *loopRun) admitCall(call domain.ModelToolCall) error {
	if run.toolCalls >= run.limits.MaxToolCalls {
		return fmt.Errorf("%w: tool loop exceeded %d tool calls", domain.ErrExecutionLimit, run.limits.MaxToolCalls)
	}
	if run.repeats[callFingerprint(call)] >= run.limits.MaxRepeatedCalls {
		return fmt.Errorf("%w: tool %q was called %d times with identical arguments", domain.ErrLoopDetected, call.Name, run.limits.MaxRepeatedCalls)
	}
	return nil
}

// account adds settled usage and fails closed on the token, cost, and delegated budget limits.
func (run *loopRun) account(usage domain.Usage) error {
	if bounds := run.input.Bounds; bounds.BudgetMinorUnits > 0 && usage.Currency != "" && usage.Currency != bounds.Currency {
		return fmt.Errorf("%w: loop usage currency %s differs from delegated budget currency %s", domain.ErrValidation, usage.Currency, bounds.Currency)
	}
	if usage.Currency != "" && run.usage.Currency != "" && usage.Currency != run.usage.Currency {
		return fmt.Errorf("%w: tool loop usage currencies differ", domain.ErrValidation)
	}
	if spend := run.input.Spend; spend.PricingRequired && usage.Currency != "" && usage.Currency != spend.Currency {
		return fmt.Errorf("%w: loop usage currency %s differs from the release budget currency %s", domain.ErrValidation, usage.Currency, spend.Currency)
	}
	if err := addChunkUsage(&run.usage, usage); err != nil {
		return err
	}
	run.toolCalls = run.usage.ToolCalls
	switch bounds := run.input.Bounds; {
	case run.usage.InputTokens > run.limits.MaxTokens || run.usage.OutputTokens > run.limits.MaxTokens-run.usage.InputTokens:
		return fmt.Errorf("%w: tool loop exceeded %d tokens", domain.ErrBudgetExceeded, run.limits.MaxTokens)
	case run.limits.MaxCostMinorUnits > 0 && run.usage.CostMinorUnits > run.limits.MaxCostMinorUnits:
		return fmt.Errorf("%w: tool loop exceeded its cost limit", domain.ErrBudgetExceeded)
	case bounds.BudgetMinorUnits > 0 && run.usage.CostMinorUnits > bounds.BudgetMinorUnits:
		return fmt.Errorf("%w: tool loop exceeded its delegated budget", domain.ErrBudgetExceeded)
	}
	return nil
}

// recordCall counts the call toward loop detection and emits its typed events.
func (run *loopRun) recordCall(call domain.ModelToolCall, tool domain.ToolReference, entry domain.LoopEntry, wasPending bool) {
	observation := *entry.Observation
	run.repeats[callFingerprint(call)]++
	event := func(kind domain.TurnEventKind) domain.TurnEvent {
		return domain.TurnEvent{Version: domain.TurnEventVersion, Kind: kind}
	}
	if wasPending {
		resolved := event(domain.TurnEventApprovalResolved)
		resolved.Approval = &domain.TurnApprovalEvent{CallID: call.CallID, ToolID: tool.ToolID, Decision: approvalOutcome(observation)}
		run.events = append(run.events, resolved)
	}
	proposal, result := event(domain.TurnEventToolProposal), event(domain.TurnEventToolResult)
	proposal.Tool = &domain.TurnToolEvent{CallID: call.CallID, ToolID: tool.ToolID, ToolVersion: tool.Version, Arguments: rawJSON(call.Arguments)}
	result.Tool = &domain.TurnToolEvent{CallID: call.CallID, ToolID: tool.ToolID, ToolVersion: tool.Version, Output: rawJSON(observation.Output), IsError: observation.IsError}
	run.events = append(run.events, proposal, result)
	if entry.Effect != nil {
		effect := event(domain.TurnEventEffect)
		effect.Effect = &domain.TurnEffectEvent{CallID: call.CallID, ToolID: tool.ToolID, ToolVersion: tool.Version, Observation: *entry.Effect}
		run.events = append(run.events, effect)
	}
}

func approvalOutcome(observation domain.ModelToolObservation) string {
	if observation.IsError {
		return string(domain.ApprovalDenied)
	}
	return string(domain.ApprovalApproved)
}

func resourceURIs(links []domain.MCPResourceLink) []string {
	uris := make([]string, 0, len(links))
	for _, link := range links {
		uris = append(uris, link.URI)
	}
	return uris
}

// rawJSON keeps an event encodable when a tool returned something other than JSON.
func rawJSON(payload []byte) json.RawMessage {
	if json.Valid(payload) {
		return payload
	}
	quoted, _ := json.Marshal(string(payload))
	return quoted
}

func callFingerprint(call domain.ModelToolCall) string {
	arguments := call.Arguments
	if canonical, err := jsonschema.Canonical(arguments); err == nil {
		arguments = canonical
	}
	sum := sha256.New()
	sum.Write([]byte(call.Name + "\x1f"))
	sum.Write(arguments)
	return hex.EncodeToString(sum.Sum(nil))
}

func callIdempotencyKey(key domain.LoopKey, call domain.ModelToolCall) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x1f%s\x1f%d\x1f%s", key.TenantID, key.RequestID, key.ExecutionStepID, call.CallID)))
	return "loop:" + hex.EncodeToString(sum[:])
}

// evidenceScope lists the calls of this turn that returned validated output.
func (run *loopRun) evidenceScope() domain.EvidenceScope {
	scope := domain.EvidenceScope{TenantID: run.key.TenantID}
	for _, entry := range run.journal {
		if entry.Kind == domain.LoopEntryObservation && entry.Observation != nil && !entry.Observation.IsError {
			scope.ToolResults = append(scope.ToolResults, domain.ToolEvidence{CallID: entry.Observation.CallID, ResourceURIs: entry.ResourceURIs})
		}
	}
	return scope
}

// LoopTrajectory projects a journal onto the trajectory Scout evaluation scores.
func LoopTrajectory(journal []domain.LoopEntry) []domain.TrajectoryEvent {
	events := make([]domain.TrajectoryEvent, 0, len(journal))
	add := func(entry domain.LoopEntry, kind domain.TrajectoryEventKind, name string, payload []byte, failed bool) {
		events = append(events, domain.TrajectoryEvent{
			Sequence: int64(len(events) + 1), Kind: kind, Name: name, Payload: payload,
			Offset: entry.Offset, Usage: entry.Usage, Failed: failed,
		})
	}
	for _, entry := range journal {
		switch {
		case entry.Kind == domain.LoopEntryModel && entry.Model != nil:
			if len(entry.Model.ToolCalls) == 0 {
				add(entry, domain.TrajectoryState, "answer", entry.Model.Output, false)
			}
			for index, call := range entry.Model.ToolCalls {
				proposal := entry
				if index > 0 {
					proposal.Usage = domain.Usage{}
				}
				add(proposal, domain.TrajectoryToolCall, call.Name, call.Arguments, false)
			}
		case entry.Kind == domain.LoopEntryObservation && entry.Observation != nil:
			add(entry, domain.TrajectoryObservation, entry.Tool.ToolID, entry.Observation.Output, entry.Observation.IsError)
		case entry.Kind == domain.LoopEntryApprovalPending:
			add(entry, domain.TrajectoryPolicy, entry.Tool.ToolID, nil, false)
		case entry.Kind == domain.LoopEntryRevision && entry.Revision != nil:
			// A decider's edit, not the model's call: the observation that follows ran these arguments.
			add(entry, domain.TrajectoryPolicy, entry.Tool.ToolID, entry.Revision.Arguments, false)
		}
	}
	return events
}

var _ contract.StepExecutor = (*ToolLoopExecutor)(nil)
