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
	"reflect"
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
// are reachable only through the governed gateway.
type ToolLoopExecutor struct {
	Requests contract.ToolLoopRequestBuilder
	Router   contract.ModelRouter
	Models   contract.ModelGateway
	Registry contract.ToolRegistry
	Tools    contract.GovernedToolGateway
	Journal  contract.LoopJournal
	// Pricer is optional; without it MaxCostMinorUnits sees only the cost providers and tools report.
	Pricer contract.ModelPricer
	// Evidence is optional; it is required by a step whose configuration sets require_evidence.
	Evidence contract.EvidenceValidator
	Limits   domain.ToolLoopLimits
	Now      func() time.Time
}

// NewToolLoopExecutor rejects a composition that could run unbounded.
func NewToolLoopExecutor(executor ToolLoopExecutor) (*ToolLoopExecutor, error) {
	if executor.Requests == nil || executor.Router == nil || executor.Models == nil || executor.Registry == nil ||
		executor.Tools == nil || executor.Journal == nil {
		return nil, fmt.Errorf("%w: tool loop needs a request builder, router, model gateway, tool registry, tool gateway, and journal", domain.ErrValidation)
	}
	limits := executor.Limits
	if limits.MaxIterations <= 0 || limits.MaxToolCalls <= 0 || limits.MaxTokens <= 0 || limits.MaxRepeatedCalls <= 0 ||
		limits.Deadline <= 0 || limits.MaxCostMinorUnits < 0 {
		return nil, fmt.Errorf("%w: tool loop iterations, tool calls, tokens, repeated calls, and deadline must be positive", domain.ErrValidation)
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
	journal   []domain.LoopEntry
	cursor    int
	usage     domain.Usage
	toolCalls int
	repeats   map[string]int
	events    []domain.TurnEvent
	started   time.Time
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
	run.limits.MaxIterations = narrowed(run.limits.MaxIterations, run.config.MaxIterations)
	run.limits.MaxToolCalls = narrowed(run.limits.MaxToolCalls, run.config.MaxToolCalls)
	run.limits.MaxTokens = narrowed64(run.limits.MaxTokens, run.config.MaxTokens)
	run.limits.MaxCostMinorUnits = narrowedOptional64(run.limits.MaxCostMinorUnits, run.config.MaxCostMinorUnits)
	run.limits.MaxRepeatedCalls = narrowed(run.limits.MaxRepeatedCalls, run.config.MaxRepeatedCalls)
	if requested := time.Duration(run.config.DeadlineSeconds) * time.Second; requested > 0 && requested < run.limits.Deadline {
		run.limits.Deadline = requested
	}
	if input.Bounds.BudgetMinorUnits > 0 && strings.TrimSpace(input.Bounds.Currency) == "" {
		return nil, fmt.Errorf("%w: a delegated cost budget requires a currency", domain.ErrValidation)
	}
	if (run.limits.MaxCostMinorUnits > 0 || input.Bounds.BudgetMinorUnits > 0) && executor.Pricer == nil {
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
	run.tools = make(map[string]domain.ModelTool, len(bound))
	request.Tools = request.Tools[:0]
	for _, definition := range bound {
		tool := domain.ModelTool{
			Name: modelToolName(definition.ToolID), Description: definition.DisplayName,
			ToolID: definition.ToolID, ToolVersion: definition.Version, InputSchema: definition.InputSchema,
		}
		if _, collides := run.tools[tool.Name]; collides {
			return nil, fmt.Errorf("%w: tools of %s@%s collide on model name %q", domain.ErrConflict, principal.ID, principal.Release, tool.Name)
		}
		run.tools[tool.Name] = tool
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
		decision, err := executor.decide(ctx, run, iteration)
		if err != nil {
			return domain.StepResult{}, err
		}
		if len(decision.ToolCalls) == 0 {
			return executor.finish(ctx, run, decision)
		}
		run.request.Messages = append(run.request.Messages, domain.ModelMessage{
			Role: domain.ModelRoleAssistant, Text: decision.Output, ToolCalls: decision.ToolCalls,
		})
		observations := make([]domain.ModelToolObservation, 0, len(decision.ToolCalls))
		for _, call := range decision.ToolCalls {
			observation, err := executor.observe(ctx, run, iteration, call)
			if err != nil {
				return domain.StepResult{}, err
			}
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
	entry, err := executor.append(ctx, run, domain.LoopEntry{Kind: domain.LoopEntryModel, Iteration: iteration, Model: &result, Usage: usage})
	if err != nil {
		return domain.ModelResult{}, errors.Join(err, run.account(usage))
	}
	return *entry.Model, run.account(entry.Usage)
}

func (executor *ToolLoopExecutor) priced(ctx context.Context, selection domain.ModelSelection, usage domain.Usage) (domain.Usage, error) {
	if executor.Pricer == nil || usage.CostMinorUnits > 0 {
		return usage, nil
	}
	reference := domain.ModelReference{ProviderID: selection.Provider, ModelID: selection.Model}
	cost, currency, err := executor.Pricer.Cost(ctx, reference, domain.ModelUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens})
	if err != nil {
		return usage, fmt.Errorf("price model %s/%s: %w", selection.Provider, selection.Model, err)
	}
	if cost < 0 || cost > 0 && strings.TrimSpace(currency) == "" {
		return usage, fmt.Errorf("%w: model pricer returned invalid cost", domain.ErrValidation)
	}
	usage.CostMinorUnits, usage.Currency = cost, currency
	return usage, nil
}

// observe returns the journaled observation of one proposed call, or invokes
// the tool through the governed gateway and journals what came back.
func (executor *ToolLoopExecutor) observe(ctx context.Context, run *loopRun, iteration int, call domain.ModelToolCall) (domain.ModelToolObservation, error) {
	tool := run.tools[call.Name]
	reference := domain.ToolReference{ToolID: tool.ToolID, Version: tool.ToolVersion}
	wasPending := false
	if entry, replayed := run.next(domain.LoopEntryApprovalPending); replayed {
		if entry.Observation == nil || entry.Observation.CallID != call.CallID {
			return domain.ModelToolObservation{}, fmt.Errorf("%w: loop journal entry %d does not match call %q", domain.ErrConflict, entry.EntryNo, call.CallID)
		}
		wasPending = true
	}
	if entry, replayed := run.next(domain.LoopEntryObservation); replayed {
		if entry.Observation == nil || entry.Observation.CallID != call.CallID {
			return domain.ModelToolObservation{}, fmt.Errorf("%w: loop journal entry %d does not match call %q", domain.ErrConflict, entry.EntryNo, call.CallID)
		}
		run.recordCall(call, reference, entry, wasPending)
		return *entry.Observation, run.account(entry.Usage)
	}
	if err := run.admitCall(call); err != nil {
		return domain.ModelToolObservation{}, err
	}
	result, err := executor.Tools.Invoke(domain.WithDecisionScope(ctx, "iteration/", iteration, "/call/", call.CallID), domain.ToolCall{
		TenantContext: run.request.TenantContext, Principal: run.input.Principal, OnBehalfOf: run.input.OnBehalfOf,
		RequestID: run.input.RequestID, ConversationID: run.request.ConversationID,
		ToolID: tool.ToolID, ToolVersion: tool.ToolVersion, Arguments: call.Arguments,
		IdempotencyKey: callIdempotencyKey(run.key, call),
	})
	observation := domain.ModelToolObservation{CallID: call.CallID, Name: call.Name, Output: result.Output}
	switch {
	case err == nil:
	case errors.Is(err, domain.ErrApprovalPending):
		if !wasPending {
			pending := domain.LoopEntry{Kind: domain.LoopEntryApprovalPending, Iteration: iteration, Tool: reference,
				Observation: &domain.ModelToolObservation{CallID: call.CallID, Name: call.Name}}
			if _, appendErr := executor.append(ctx, run, pending); appendErr != nil {
				return domain.ModelToolObservation{}, errors.Join(err, appendErr)
			}
		}
		return domain.ModelToolObservation{}, err
	case endsLoop(ctx, err):
		return domain.ModelToolObservation{}, stage.At(domain.StageTool, err)
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
		return domain.ModelToolObservation{}, errors.Join(err, run.account(usage))
	}
	run.recordCall(call, reference, entry, wasPending)
	return *entry.Observation, run.account(entry.Usage)
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
	return domain.StepResult{
		State: decision.Output, NextStepID: run.config.NextStepID, Fingerprint: DigestBytes(decision.Output),
		Usage: run.usage, Events: run.events,
	}, nil
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
		finding := report.Unsupported[0]
		return stage.At(domain.StageGuardrail, fmt.Errorf("%w: claim %d (%s)", domain.ErrUnsupportedClaim, finding.ClaimIndex, finding.Reason))
	}
	run.events = append(run.events, domain.TurnEvent{Version: domain.TurnEventVersion, Kind: domain.TurnEventEvidence, Evidence: answer.Evidence})
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
	stored, err := executor.Journal.Append(context.WithoutCancel(ctx), run.key, entry)
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
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ToolCalls < 0 || usage.CostMinorUnits < 0 ||
		usage.CostMinorUnits > 0 && strings.TrimSpace(usage.Currency) == "" {
		return fmt.Errorf("%w: loop usage is invalid", domain.ErrValidation)
	}
	if bounds := run.input.Bounds; bounds.BudgetMinorUnits > 0 && usage.Currency != "" && usage.Currency != bounds.Currency {
		return fmt.Errorf("%w: loop usage currency %s differs from delegated budget currency %s", domain.ErrValidation, usage.Currency, bounds.Currency)
	}
	if usage.Currency != "" && run.usage.Currency != "" && usage.Currency != run.usage.Currency {
		return fmt.Errorf("%w: tool loop usage currencies differ", domain.ErrValidation)
	}
	run.usage.InputTokens += usage.InputTokens
	run.usage.OutputTokens += usage.OutputTokens
	run.usage.ToolCalls += usage.ToolCalls
	run.usage.CostMinorUnits += usage.CostMinorUnits
	if run.usage.Currency == "" {
		run.usage.Currency = usage.Currency
	}
	run.toolCalls += usage.ToolCalls
	switch bounds := run.input.Bounds; {
	case run.usage.InputTokens+run.usage.OutputTokens > run.limits.MaxTokens:
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
		}
	}
	return events
}

var _ contract.StepExecutor = (*ToolLoopExecutor)(nil)
