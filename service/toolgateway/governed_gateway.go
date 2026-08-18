package toolgateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
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
	// Guardrails is optional; when set, GuardrailConfigs is required and both tool stages are enforced.
	Guardrails       contract.GuardrailEnforcer
	GuardrailConfigs contract.ToolGuardrailConfigResolver
	// Classifier decides which failures reach the breaker; nil counts every failure except
	// cancellation and typed tenant, authorization, and rate-limit errors.
	Classifier contract.ToolFailureClassifier
	// Policy is optional; when set it runs before guardrails and its obligations
	// are applied before egress. An obligation with no enforcer fails the call.
	Policy      contract.PolicyDecisionPoint
	Obligations []contract.ObligationEnforcer
	// Audit is optional; when set every credential resolution records which
	// authority it exercised, never the secret.
	Audit contract.AuditSink
	// CredentialPurpose labels why the credential is requested; it selects the binding.
	CredentialPurpose string
	Timeout           time.Duration
}

// Invoke executes one tool call through the complete governance chain.
func (gateway *GovernedGateway) Invoke(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
	if err := gateway.validate(call); err != nil {
		return domain.ToolResult{}, err
	}
	if err := gateway.RateLimiter.AllowToolCall(ctx, call); err != nil {
		return domain.ToolResult{}, err
	}
	definition, err := gateway.Registry.Get(ctx, call.TenantContext.TenantID, call.ToolID, call.ToolVersion)
	if err != nil {
		return domain.ToolResult{}, err
	}
	if definition.ToolID != call.ToolID || definition.Version != call.ToolVersion || strings.TrimSpace(definition.Endpoint) == "" {
		return domain.ToolResult{}, fmt.Errorf("%w: registered tool definition is inconsistent", domain.ErrConflict)
	}
	if err := gateway.Authorizer.Authorize(ctx, call, definition); err != nil {
		return domain.ToolResult{}, err
	}
	if err := gateway.decide(ctx, call, definition); err != nil {
		return domain.ToolResult{}, err
	}
	// Guardrails run before egress and credentials so blocked arguments never leave the platform.
	guardrails, err := gateway.guardrailConfig(ctx, call)
	if err != nil {
		return domain.ToolResult{}, err
	}
	if gateway.Guardrails != nil {
		if call, err = gateway.Guardrails.BeforeTool(ctx, guardrails, call); err != nil {
			return domain.ToolResult{}, stage.At(domain.StageGuardrail, err)
		}
	}
	if err := gateway.Egress.ValidateDestination(ctx, call.TenantContext.TenantID, definition.Endpoint); err != nil {
		return domain.ToolResult{}, err
	}
	generation, err := gateway.admit(ctx, call, definition)
	if err != nil {
		return domain.ToolResult{}, err
	}
	credential, authority, err := gateway.Credentials.Credential(ctx, call.Principal, call.ToolID, "invoke", gateway.CredentialPurpose)
	if err != nil {
		return domain.ToolResult{}, err
	}
	if err := gateway.recordCredential(ctx, call, definition, authority); err != nil {
		return domain.ToolResult{}, err
	}

	for attempt := 1; ; attempt++ {
		result, callErr := gateway.Transport.Invoke(ctx, call, definition, credential, gateway.Timeout)
		if callErr == nil {
			if err := gateway.Validator.Validate(ctx, definition, result); err != nil {
				callErr = fmt.Errorf("%w: %w", ErrInvalidToolOutput, err)
			}
		}
		if callErr == nil && !result.Retryable {
			if err := gateway.settle(ctx, call, definition, generation, true); err != nil {
				return result, err
			}
			if gateway.Guardrails != nil {
				subject := domain.GuardrailSubject{
					TenantID: call.TenantContext.TenantID, Principal: domain.PrincipalRef{Kind: call.Principal.Kind, ID: call.Principal.ID},
					RequestID: call.RequestID, ConversationID: call.ConversationID, ReleaseVersion: call.Principal.Release,
				}
				guarded, err := gateway.Guardrails.AfterTool(ctx, guardrails, subject, result)
				if err != nil {
					return domain.ToolResult{}, stage.At(domain.StageGuardrail, err)
				}
				result = guarded
			}
			return result, nil
		}
		if callErr == nil {
			callErr = fmt.Errorf("tool %q returned a retryable result", call.ToolID)
		}
		if gateway.classifier().CountsAsDependencyFailure(ctx, call, result, callErr) {
			if err := gateway.settle(ctx, call, definition, generation, false); err != nil {
				return result, errors.Join(callErr, err)
			}
		}
		if err := ctx.Err(); err != nil {
			return result, errors.Join(callErr, err)
		}
		delay, retry := gateway.Retry.NextDelay(ctx, call, result, callErr, attempt)
		if !retry {
			return result, callErr
		}
		if err := waitForRetry(ctx, delay); err != nil {
			return result, errors.Join(callErr, err)
		}
	}
}

func (gateway *GovernedGateway) validate(call domain.ToolCall) error {
	if gateway.Registry == nil || gateway.RateLimiter == nil || gateway.Authorizer == nil || gateway.Credentials == nil || gateway.Egress == nil || gateway.Circuit == nil || gateway.Transport == nil || gateway.Retry == nil || gateway.Validator == nil {
		return fmt.Errorf("tool gateway: every governance dependency is required")
	}
	if gateway.Guardrails != nil && gateway.GuardrailConfigs == nil {
		return fmt.Errorf("tool gateway: guardrails require a guardrail config resolver")
	}
	if gateway.Timeout <= 0 {
		return fmt.Errorf("%w: tool timeout must be positive", domain.ErrValidation)
	}
	if call.TenantContext.TenantID <= 0 || strings.TrimSpace(call.RequestID) == "" || strings.TrimSpace(call.ToolID) == "" || strings.TrimSpace(call.ToolVersion) == "" {
		return fmt.Errorf("%w: tenant, request, tool, and version are required", domain.ErrValidation)
	}
	if call.Principal.Kind == "" || strings.TrimSpace(call.Principal.ID) == "" {
		return fmt.Errorf("%w: tool calls require a resolved principal", domain.ErrPrincipalUnknown)
	}
	if call.Principal.TenantID != call.TenantContext.TenantID {
		return fmt.Errorf("%w: principal tenant %d does not own the call", domain.ErrForbidden, call.Principal.TenantID)
	}
	return nil
}

// decide asks the policy decision point and applies every obligation it
// attached. An unrecognized obligation is a hard failure: silently ignoring one
// would turn a conditional allow into an unconditional one.
func (gateway *GovernedGateway) decide(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition) error {
	if gateway.Policy == nil {
		return nil
	}
	subject := domain.DecisionSubject{
		Principal: call.Principal, Action: "tool:invoke", Resource: definition.ToolID,
		ResourceKind: domain.ResourceTool, RequestID: call.RequestID, ConversationID: call.ConversationID,
	}
	decision, err := gateway.Policy.Decide(ctx, subject)
	if err != nil {
		return err
	}
	if decision.Outcome != domain.DecisionAllow {
		return fmt.Errorf("%w: %s", domain.ErrForbidden, decision.Reason)
	}
	for _, obligation := range decision.Obligations {
		enforcer := gateway.enforcerFor(obligation.Kind)
		if enforcer == nil {
			return fmt.Errorf("%w: no enforcer for obligation %q", domain.ErrDegraded, obligation.Kind)
		}
		if err := enforcer.Enforce(ctx, subject, obligation); err != nil {
			return err
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
		TenantID: call.TenantContext.TenantID, Principal: domain.PrincipalRef{Kind: call.Principal.Kind, ID: call.Principal.ID},
		Authority: authority, ScopeID: call.Principal.ScopeID, Category: domain.DecisionCategoryCredential,
		Action: "resolve", Resource: definition.ToolID, ReleaseVersion: call.Principal.Release,
		Outcome: domain.DecisionAllow, RequestID: call.RequestID, ConversationID: call.ConversationID,
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

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ contract.GovernedToolGateway = (*GovernedGateway)(nil)
