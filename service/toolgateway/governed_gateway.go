package toolgateway

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nauticana/keel/clock"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
	"github.com/nauticana/scout/service/scope"
)

// GovernedGateway applies policy, guardrails, credentials, resilience, and validation to tool calls.
type GovernedGateway struct {
	Registry    contract.ToolRegistry
	RateLimiter contract.TenantRateLimiter
	Authorizer  contract.ToolAuthorizer
	Credentials contract.ToolCredentialProvider
	Egress      contract.ToolEgressPolicy
	Circuit     contract.ToolCircuitBreaker
	Transport   contract.ToolTransport
	Retry       contract.ToolRetryPolicy
	Validator   contract.ToolResultValidator
	// Effects observes the postcondition of tools that declare VerifyEffect; such a tool is refused without it.
	Effects contract.ToolEffectVerifier
	// Guardrails is optional; when set, GuardrailConfigs is required and both tool stages are enforced.
	Guardrails       contract.GuardrailEnforcer
	GuardrailConfigs contract.ToolGuardrailConfigResolver
	// Clock paces retry waits; nil uses the system clock.
	Clock clock.Clock
	// Classifier decides which failures reach the breaker; nil counts every failure except
	// cancellation and typed tenant, authorization, and rate-limit errors.
	Classifier contract.ToolFailureClassifier
	// Policy is optional; when set it runs before guardrails and its obligations
	// are applied before egress. An obligation with no enforcer fails the call.
	Policy      contract.PolicyDecisionPoint
	Obligations []contract.ObligationEnforcer
	// Audit is optional; when set every call records its outcome and every credential
	// resolution which authority it exercised, never the secret.
	Audit contract.AuditSink
	// Limits reads the operating mode frozen into the calling agent's release, and Approvals
	// decides the calls that mode routes to a person; see autonomy.
	Limits    contract.PrincipalLimits
	Approvals contract.ToolApprovalGate
	// CredentialPurpose labels why the credential is requested; it selects the binding.
	CredentialPurpose string
	Timeout           time.Duration
	// Outcomes is the product's trusted declaration, by tool id, of which tools deduplicate by
	// idempotency key and which the Lookup answers; nothing a tenant registers can declare either.
	Outcomes map[string]domain.ToolOutcomeTraits
	// Lookup answers the calls of tools whose Outcomes declare lookup support; required by any.
	Lookup contract.ToolOutcomeLookup
}

// Invoke executes one tool call through the complete governance chain and records its outcome.
// A failure says what became of the call: domain.ErrNotSent when it was refused before it left;
// for a non-read-only tool, domain.ErrEffectUnknown when it was sent and nothing establishes its
// effect, domain.ErrNoEffect or domain.ErrEffectViolated when its effect is proven absent, and
// domain.ErrOutputWithheld when it took effect but its output cannot be returned. A person's call
// in a turn they took over is bounded by decision 5's intersection: the turn principal's release
// bindings and authority chain through the Authorizer, policy as that principal and as the person,
// the release's guardrails, and the person's own credential; the agent's operating mode does not apply.
func (gateway *GovernedGateway) Invoke(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
	if err := gateway.validate(call); err != nil {
		return domain.ToolResult{}, notSent(err)
	}
	// The idempotency key is stable across redelivery, so the call's decisions are recorded once.
	if call.IdempotencyKey != "" {
		ctx = domain.WithDecisionScope(ctx, "tool/", call.IdempotencyKey)
	}
	result, err := gateway.invoke(ctx, call)
	if errors.Is(err, domain.ErrApprovalPending) {
		return result, err
	}
	recordErr := gateway.recordInvocation(ctx, call, result, err)
	if err == nil && recordErr != nil {
		// The call took effect; only its evidence failed.
		return result, fmt.Errorf("%w: %w", domain.ErrOutputWithheld, recordErr)
	}
	return result, errors.Join(err, recordErr)
}

// operated reports a call a person makes in a turn they took over.
func operated(call domain.ToolCall) bool { return call.Operator.Kind != "" }

// actor is who the call's records name: the person for an operated call, else its principal.
func actor(call domain.ToolCall) domain.PrincipalRef {
	if operated(call) {
		return domain.PrincipalRef{Kind: call.Operator.Kind, ID: call.Operator.ID}
	}
	return domain.PrincipalRef{Kind: call.Principal.Kind, ID: call.Principal.ID}
}

// notSent marks a failure that kept the call from leaving the platform.
func notSent(err error) error {
	return fmt.Errorf("%w: %w", domain.ErrNotSent, err)
}

// preparedCall is a call that passed governance and may now be sent.
type preparedCall struct {
	call       domain.ToolCall
	definition domain.ToolDefinition
	guardrails domain.GuardrailConfig
	generation int64
	credential []byte
}

func (gateway *GovernedGateway) invoke(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
	prepared, err := gateway.prepare(ctx, call)
	if err != nil {
		return domain.ToolResult{}, notSent(err)
	}
	result, err := gateway.dispatch(ctx, prepared)
	if err != nil && !prepared.definition.ReadOnly {
		err = classifySent(err)
	}
	return result, err
}

// prepare runs every check before the call can leave; none of its failures sent anything.
func (gateway *GovernedGateway) prepare(ctx context.Context, call domain.ToolCall) (preparedCall, error) {
	if err := gateway.RateLimiter.AllowToolCall(ctx, call); err != nil {
		return preparedCall{}, err
	}
	definition, err := gateway.Registry.Get(ctx, call.TenantContext.TenantID, call.ToolID, call.ToolVersion)
	if err != nil {
		return preparedCall{}, err
	}
	if definition.ToolID != call.ToolID || definition.Version != call.ToolVersion || strings.TrimSpace(definition.Endpoint) == "" {
		return preparedCall{}, fmt.Errorf("%w: registered tool definition is inconsistent", domain.ErrConflict)
	}
	if definition.VerifyEffect && gateway.Effects == nil {
		return preparedCall{}, fmt.Errorf("%w: tool %q requires effect verification but no verifier is composed", domain.ErrNotReady, call.ToolID)
	}
	if err := validateArguments(definition, call.Arguments); err != nil {
		return preparedCall{}, err
	}
	// A turn that compiled a working set mutates only on verified or bounded context.
	if call.Assurance != "" && !definition.ReadOnly && !call.Assurance.Acts() {
		return preparedCall{}, fmt.Errorf("%w: %w: tool %q mutates and the turn's working set assurance is %s",
			domain.ErrForbidden, domain.ErrWorkingSetUnassured, call.ToolID, call.Assurance)
	}
	if err := gateway.Authorizer.Authorize(ctx, call, definition); err != nil {
		return preparedCall{}, err
	}
	policyAsk, err := gateway.decide(ctx, call, call.Principal, definition)
	if err != nil {
		return preparedCall{}, err
	}
	autonomyAsks := false
	if operated(call) {
		// A person's call meets policy as the person too; the agent's operating mode does not govern a person.
		operatorAsk, err := gateway.decide(ctx, call, call.Operator, definition)
		if err != nil {
			return preparedCall{}, err
		}
		if operatorAsk != nil && (policyAsk == nil || scope.ApprovalRiskRank(operatorAsk.RiskTier) > scope.ApprovalRiskRank(policyAsk.RiskTier)) {
			policyAsk = operatorAsk
		}
	} else if autonomyAsks, err = gateway.autonomy(ctx, call, definition); err != nil {
		return preparedCall{}, err
	}
	if err := gateway.approve(ctx, call, policyAsk, autonomyAsks); err != nil {
		return preparedCall{}, err
	}
	// Guardrails run before egress and credentials so blocked arguments never leave the platform.
	guardrails, err := gateway.guardrailConfig(ctx, call)
	if err != nil {
		return preparedCall{}, err
	}
	if gateway.Guardrails != nil {
		if call, err = gateway.Guardrails.BeforeTool(ctx, guardrails, call); err != nil {
			return preparedCall{}, stage.At(domain.StageGuardrail, err)
		}
	}
	if err := gateway.Egress.ValidateDestination(ctx, call.TenantContext.TenantID, definition.Endpoint); err != nil {
		return preparedCall{}, err
	}
	generation, err := gateway.admit(ctx, call, definition)
	if err != nil {
		return preparedCall{}, err
	}
	// A person's call runs on the person's own credential, resolved for this call.
	holder := call.Principal
	if operated(call) {
		holder = call.Operator
	}
	credential, authority, err := gateway.Credentials.Credential(ctx, holder, call.ToolID, "invoke", gateway.CredentialPurpose)
	if err != nil {
		return preparedCall{}, err
	}
	if err := gateway.recordCredential(ctx, call, definition, authority); err != nil {
		return preparedCall{}, err
	}
	return preparedCall{call: call, definition: definition, guardrails: guardrails, generation: generation, credential: credential}, nil
}

// dispatch sends the prepared call within its attempt ceiling. Attempt one's pre-observation
// precedes any send of this invocation, so its failure is marked not sent.
func (gateway *GovernedGateway) dispatch(ctx context.Context, prepared preparedCall) (domain.ToolResult, error) {
	call, definition := prepared.call, prepared.definition
	// The registered contract's own timeout and attempt ceiling win over the gateway defaults.
	timeout := gateway.Timeout
	if definition.Timeout > 0 {
		timeout = definition.Timeout
	}
	// uncertain is set once an attempt of a mutating call ended without proving it took no effect.
	uncertain := false
	for attempt := 1; ; attempt++ {
		// Observing first keeps a redelivered or retried call from repeating an effect that already landed.
		result, reconciled, err := gateway.reconcile(ctx, call, definition)
		if err != nil {
			if attempt == 1 {
				err = notSent(err)
			}
			return result, err
		}
		var callErr error
		if !reconciled {
			if uncertain && !definition.ReadOnly && !gateway.Traits(call.ToolID).DedupesByKey &&
				(result.Effect == nil || result.Effect.Status != domain.EffectViolated) {
				return result, fmt.Errorf("%w: no source proves the previous attempt had no effect", domain.ErrEffectUnknown)
			}
			result, callErr = gateway.Transport.Invoke(ctx, call, definition, prepared.credential, timeout)
		}
		if callErr == nil && result.Acceptance != nil {
			return gateway.acceptance(ctx, prepared, result)
		}
		if callErr == nil {
			if err := gateway.Validator.Validate(ctx, definition, result); err != nil {
				callErr = fmt.Errorf("%w: %w", ErrInvalidToolOutput, err)
			}
		}
		if callErr == nil && !result.Retryable && !reconciled {
			if result, err = gateway.verifyEffect(ctx, call, definition, result); err != nil {
				if !definition.RetryWhenEffectAbsent || !errors.Is(err, domain.ErrEffectViolated) {
					return result, errors.Join(err, gateway.settle(ctx, call, definition, prepared.generation, true))
				}
				// A proven-absent effect the contract declares safe to resend takes the bounded retry path.
				callErr, result.Retryable = err, true
			}
		}
		if callErr == nil && !result.Retryable {
			if err := gateway.settle(ctx, call, definition, prepared.generation, true); err != nil {
				return result, withheld(err)
			}
			if gateway.Guardrails != nil {
				subject := domain.GuardrailSubject{
					TenantID: call.TenantContext.TenantID, Principal: actor(call),
					RequestID: call.RequestID, ConversationID: call.ConversationID, ReleaseVersion: call.Principal.Release,
				}
				guarded, err := gateway.Guardrails.AfterTool(ctx, prepared.guardrails, subject, result)
				if err != nil {
					return domain.ToolResult{Usage: result.Usage}, withheld(stage.At(domain.StageGuardrail, err))
				}
				result = guarded
			}
			return result, nil
		}
		if callErr == nil {
			callErr = fmt.Errorf("%w: tool %q returned a retryable result", domain.ErrNoEffect, call.ToolID)
		} else if result.Retryable && !errors.Is(callErr, domain.ErrEffectViolated) {
			// A retryable answer declares the attempt took no effect, which is what makes resending it safe.
			callErr = fmt.Errorf("%w: %w", domain.ErrNoEffect, callErr)
		}
		if uncertain = uncertain || !definition.ReadOnly && !errors.Is(callErr, domain.ErrNoEffect) && !errors.Is(callErr, domain.ErrEffectViolated); uncertain {
			callErr = fmt.Errorf("%w: %w", domain.ErrEffectUnknown, callErr)
		}
		if gateway.classifier().CountsAsDependencyFailure(ctx, call, result, callErr) {
			if err := gateway.settle(ctx, call, definition, prepared.generation, false); err != nil {
				return result, errors.Join(callErr, err)
			}
		}
		if err := ctx.Err(); err != nil {
			return result, errors.Join(callErr, err)
		}
		delay, retry := gateway.Retry.NextDelay(ctx, call, result, callErr, attempt)
		if !retry || definition.MaxAttempts > 0 && attempt >= definition.MaxAttempts {
			return result, callErr
		}
		if err := gateway.waitForRetry(ctx, delay); err != nil {
			return result, errors.Join(callErr, err)
		}
	}
}

// acceptance admits an asynchronous acceptance only from a tool whose composition declares lookup
// support and only when it is valid; otherwise it is invalid output of a sent call.
func (gateway *GovernedGateway) acceptance(ctx context.Context, prepared preparedCall, result domain.ToolResult) (domain.ToolResult, error) {
	call := prepared.call
	if !gateway.Traits(call.ToolID).Lookup {
		return domain.ToolResult{Usage: result.Usage}, fmt.Errorf("%w: tool %q accepted a call but declares no lookup", ErrInvalidToolOutput, call.ToolID)
	}
	if err := result.Acceptance.Validate(); err != nil {
		return domain.ToolResult{Usage: result.Usage}, fmt.Errorf("%w: %w", ErrInvalidToolOutput, err)
	}
	if err := gateway.settle(ctx, call, prepared.definition, prepared.generation, true); err != nil {
		return result, withheld(err)
	}
	// An acceptance carries no output for the model; only its handle and usage go on.
	return domain.ToolResult{Usage: result.Usage, Acceptance: result.Acceptance}, nil
}

// withheld marks a failure after the call took effect.
func withheld(err error) error {
	return fmt.Errorf("%w: %w", domain.ErrOutputWithheld, err)
}

// classifySent marks a sent non-read-only call's failure that proves no effect and leaves no
// effect established either way as unknown.
func classifySent(err error) error {
	switch {
	case errors.Is(err, domain.ErrNotSent), errors.Is(err, domain.ErrEffectUnknown), errors.Is(err, domain.ErrOutputWithheld),
		errors.Is(err, domain.ErrEffectViolated), errors.Is(err, domain.ErrNoEffect):
		return err
	}
	return fmt.Errorf("%w: %w", domain.ErrEffectUnknown, err)
}

func (gateway *GovernedGateway) validate(call domain.ToolCall) error {
	if gateway.Registry == nil || gateway.RateLimiter == nil || gateway.Authorizer == nil || gateway.Credentials == nil || gateway.Egress == nil || gateway.Circuit == nil || gateway.Transport == nil || gateway.Retry == nil || gateway.Validator == nil || gateway.Limits == nil {
		return fmt.Errorf("tool gateway: every governance dependency is required")
	}
	if gateway.Guardrails != nil && gateway.GuardrailConfigs == nil {
		return fmt.Errorf("tool gateway: guardrails require a guardrail config resolver")
	}
	if gateway.Timeout <= 0 {
		return fmt.Errorf("%w: tool timeout must be positive", domain.ErrValidation)
	}
	if gateway.Approvals != nil && gateway.enforcerFor(domain.ObligationRequireApproval) != nil {
		return fmt.Errorf("tool gateway: require_approval is enforced by the approval gate; an enforcer of it as well is ambiguous")
	}
	if gateway.Lookup == nil {
		for toolID, traits := range gateway.Outcomes {
			if traits.Lookup {
				return fmt.Errorf("tool gateway: tool %q declares lookup support but no outcome lookup is composed", toolID)
			}
		}
	}
	if call.TenantContext.TenantID <= 0 || strings.TrimSpace(call.RequestID) == "" || strings.TrimSpace(call.ToolID) == "" || strings.TrimSpace(call.ToolVersion) == "" {
		return fmt.Errorf("%w: tenant, request, tool, and version are required", domain.ErrValidation)
	}
	if call.Principal.Kind == "" || strings.TrimSpace(call.Principal.ID) == "" {
		return fmt.Errorf("%w: tool calls require a resolved principal", domain.ErrPrincipalUnknown)
	}
	if call.Assurance != "" && !call.Assurance.Valid() {
		return fmt.Errorf("%w: working set assurance %q is not a working_set_assurance code", domain.ErrValidation, call.Assurance)
	}
	if call.Principal.TenantID != call.TenantContext.TenantID {
		return fmt.Errorf("%w: principal tenant %d does not own the call", domain.ErrForbidden, call.Principal.TenantID)
	}
	if operated(call) && (call.Operator.Kind != domain.PrincipalHuman || strings.TrimSpace(call.Operator.ID) == "" || call.Operator.TenantID != call.TenantContext.TenantID) {
		return fmt.Errorf("%w: an operator call is made by a person of the call's tenant", domain.ErrForbidden)
	}
	return nil
}

// decide asks the policy decision point about principal invoking the call and applies every
// obligation it attached. An unrecognized obligation is a hard failure: silently ignoring one would
// turn a conditional allow into an unconditional one. With an approval gate composed,
// require_approval becomes one ask at the riskiest tier its obligations name, which approve then
// puts to a person.
func (gateway *GovernedGateway) decide(ctx context.Context, call domain.ToolCall, principal domain.Principal, definition domain.ToolDefinition) (*domain.ApprovalAsk, error) {
	if gateway.Policy == nil {
		return nil, nil
	}
	subject := domain.DecisionSubject{
		Principal: principal, Action: domain.PolicyActionToolInvoke, Resource: definition.ToolID,
		ResourceKind: domain.ResourceTool, RequestID: call.RequestID, ConversationID: call.ConversationID,
	}
	decision, err := gateway.Policy.Decide(ctx, subject)
	if err != nil {
		return nil, err
	}
	if decision.Outcome != domain.DecisionAllow {
		return nil, fmt.Errorf("%w: %s", domain.ErrForbidden, decision.Reason)
	}
	var ask *domain.ApprovalAsk
	for _, obligation := range decision.Obligations {
		if obligation.Kind == domain.ObligationRequireApproval && gateway.Approvals != nil {
			risk, err := scope.ApprovalObligationRisk(obligation)
			if err != nil {
				return nil, err
			}
			if ask == nil || scope.ApprovalRiskRank(risk) > scope.ApprovalRiskRank(ask.RiskTier) {
				ask = &domain.ApprovalAsk{Source: domain.ApprovalSourcePolicy, RuleID: string(domain.ApprovalSourcePolicy), RiskTier: risk}
			}
			continue
		}
		enforcer := gateway.enforcerFor(obligation.Kind)
		if enforcer == nil {
			return nil, fmt.Errorf("%w: no enforcer for obligation %q", domain.ErrDegraded, obligation.Kind)
		}
		if err := enforcer.Enforce(ctx, subject, obligation); err != nil {
			return nil, err
		}
	}
	return ask, nil
}

// autonomy applies the agent's operating mode and reports whether it routes the call to a
// person: human_only acts on nothing; advise and draft call only read-only tools;
// execute_with_approval calls read-only tools and asks a person for every other call;
// bounded_autonomous acts within policy, and outside its window the mode has already degraded
// to execute_with_approval. An unbound mode is human_only.
func (gateway *GovernedGateway) autonomy(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition) (bool, error) {
	now := time.Now()
	if gateway.Clock != nil {
		now = gateway.Clock.Now()
	}
	mode, err := gateway.Limits.AutonomyMode(ctx, call.Principal, now)
	if err != nil {
		return false, fmt.Errorf("resolve operating mode of %q: %w", call.Principal.ID, err)
	}
	switch {
	case mode == domain.AutonomyBounded:
		return false, nil
	case definition.ReadOnly && (mode == domain.AutonomyAdvise || mode == domain.AutonomyDraft || mode == domain.AutonomyExecuteWithApproval):
		return false, nil
	case mode != domain.AutonomyExecuteWithApproval:
		return false, fmt.Errorf("%w: operating mode %q does not let %q call %q", domain.ErrForbidden, mode, call.Principal.ID, call.ToolID)
	}
	return true, nil
}

// approve puts each ask policy and the operating mode make of a call to the approval gate, the
// riskiest first (the mode asks at the default tier), and stops at the first not yet approved.
// The gate lets an approval decide a later ask only when it covers that ask's risk and route.
func (gateway *GovernedGateway) approve(ctx context.Context, call domain.ToolCall, policyAsk *domain.ApprovalAsk, autonomyAsks bool) error {
	var asks []domain.ApprovalAsk
	if policyAsk != nil {
		asks = append(asks, *policyAsk)
	}
	if autonomyAsks {
		asks = append(asks, domain.ApprovalAsk{Source: domain.ApprovalSourceAutonomy, RuleID: string(domain.ApprovalSourceAutonomy), RiskTier: domain.DefaultApprovalRiskTier})
	}
	if len(asks) == 0 {
		return nil
	}
	if gateway.Approvals == nil {
		return fmt.Errorf("%w: tool %q needs a person's approval but no tool approval gate is composed", domain.ErrDegraded, call.ToolID)
	}
	slices.SortStableFunc(asks, func(a, b domain.ApprovalAsk) int {
		return cmp.Compare(scope.ApprovalRiskRank(b.RiskTier), scope.ApprovalRiskRank(a.RiskTier))
	})
	for _, ask := range asks {
		decision, err := gateway.Approvals.Decide(ctx, call, ask)
		if err != nil {
			return err
		}
		switch decision {
		case domain.ApprovalApproved:
		case domain.ApprovalPending:
			return fmt.Errorf("%w: tool %q awaits a person's approval", domain.ErrApprovalPending, call.ToolID)
		default:
			return fmt.Errorf("%w: a person refused tool call %q", domain.ErrForbidden, call.ToolID)
		}
	}
	return nil
}

func (gateway *GovernedGateway) enforcerFor(kind domain.ObligationKind) contract.ObligationEnforcer {
	for _, enforcer := range gateway.Obligations {
		if enforcer.Kind() == kind {
			return enforcer
		}
	}
	return nil
}

// recordCredential writes which authority the call exercised. The secret itself
// is never passed here and never recorded.
func (gateway *GovernedGateway) recordCredential(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition, authority domain.AuthorityRef) error {
	if gateway.Audit == nil {
		return nil
	}
	return gateway.Audit.Record(ctx, domain.DecisionRecord{
		TenantID: call.TenantContext.TenantID, Principal: actor(call),
		Authority: authority, ScopeID: call.Principal.ScopeID, Category: domain.DecisionCategoryCredential,
		Action: "resolve", Resource: definition.ToolID, ReleaseVersion: call.Principal.Release,
		Outcome: domain.DecisionAllow, RequestID: call.RequestID, ConversationID: call.ConversationID,
	})
}

// recordInvocation writes the tool_invoke decision: what was called and how it ended, never the arguments or output.
func (gateway *GovernedGateway) recordInvocation(ctx context.Context, call domain.ToolCall, result domain.ToolResult, cause error) error {
	if gateway.Audit == nil {
		return nil
	}
	outcome := domain.DecisionAllow
	if cause != nil {
		outcome = domain.DecisionDeny
	}
	reason := stage.ErrorClass(cause)
	if result.Effect != nil {
		reason = strings.TrimPrefix(reason+" effect "+string(result.Effect.Status), " ")
	}
	return gateway.Audit.Record(ctx, domain.DecisionRecord{
		TenantID: call.TenantContext.TenantID, Principal: actor(call),
		ScopeID: call.Principal.ScopeID, Category: domain.DecisionCategoryToolInvoke, Action: "invoke",
		Resource: call.ToolID, ReleaseVersion: call.Principal.Release, Outcome: outcome, Reason: reason,
		RequestID: call.RequestID, ConversationID: call.ConversationID,
	})
}

func (gateway *GovernedGateway) guardrailConfig(ctx context.Context, call domain.ToolCall) (domain.GuardrailConfig, error) {
	if gateway.Guardrails == nil {
		return domain.GuardrailConfig{}, nil
	}
	config, err := gateway.GuardrailConfigs.GuardrailConfig(ctx, call)
	if err != nil {
		return domain.GuardrailConfig{}, stage.At(domain.StageGuardrail, err)
	}
	return config, nil
}

// admit uses fenced admission when the breaker supports it, so a stale outcome cannot move newer state.
func (gateway *GovernedGateway) admit(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition) (int64, error) {
	if fenced, ok := gateway.Circuit.(contract.FencedToolCircuitBreaker); ok {
		return fenced.Admit(ctx, call, definition)
	}
	return 0, gateway.Circuit.Allow(ctx, call.TenantContext.TenantID, call.ToolID)
}

func (gateway *GovernedGateway) settle(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition, generation int64, success bool) error {
	if fenced, ok := gateway.Circuit.(contract.FencedToolCircuitBreaker); ok {
		return fenced.Settle(ctx, call, definition, generation, success)
	}
	if success {
		return gateway.Circuit.RecordSuccess(ctx, call.TenantContext.TenantID, call.ToolID)
	}
	return gateway.Circuit.RecordFailure(ctx, call.TenantContext.TenantID, call.ToolID)
}

func (gateway *GovernedGateway) classifier() contract.ToolFailureClassifier {
	if gateway.Classifier != nil {
		return gateway.Classifier
	}
	return DefaultFailureClassifier{CountValidationFailures: true}
}

// validationProbe satisfies the per-call checks so a composition can be validated at construction time.
var validationProbe = domain.ToolCall{
	TenantContext: domain.TenantContext{TenantID: 1},
	Principal:     domain.Principal{Kind: domain.PrincipalAgent, ID: "probe", TenantID: 1},
	RequestID:     "probe", ToolID: "probe", ToolVersion: "probe",
}

func (gateway *GovernedGateway) waitForRetry(ctx context.Context, delay time.Duration) error {
	if gateway.Clock == nil {
		return clock.System{}.Sleep(ctx, delay)
	}
	return gateway.Clock.Sleep(ctx, delay)
}

var _ contract.GovernedToolGateway = (*GovernedGateway)(nil)
