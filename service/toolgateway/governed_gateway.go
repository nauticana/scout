package toolgateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// GovernedGateway applies policy, credentials, resilience, and validation to tool calls.
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
	Timeout     time.Duration
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
	if err := gateway.Egress.ValidateDestination(ctx, call.TenantContext.TenantID, definition.Endpoint); err != nil {
		return domain.ToolResult{}, err
	}
	if err := gateway.Circuit.Allow(ctx, call.TenantContext.TenantID, call.ToolID); err != nil {
		return domain.ToolResult{}, err
	}
	credential, err := gateway.Credentials.Credential(ctx, call.TenantContext.TenantID, call.ToolID)
	if err != nil {
		return domain.ToolResult{}, err
	}

	for attempt := 1; ; attempt++ {
		result, callErr := gateway.Transport.Invoke(ctx, call, definition, credential, gateway.Timeout)
		if callErr == nil {
			callErr = gateway.Validator.Validate(ctx, definition, result)
		}
		if callErr == nil && !result.Retryable {
			if err := gateway.Circuit.RecordSuccess(ctx, call.TenantContext.TenantID, call.ToolID); err != nil {
				return result, err
			}
			return result, nil
		}
		if callErr == nil {
			callErr = fmt.Errorf("tool %q returned a retryable result", call.ToolID)
		}
		if err := gateway.Circuit.RecordFailure(ctx, call.TenantContext.TenantID, call.ToolID); err != nil {
			return result, errors.Join(callErr, err)
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
	if gateway.Timeout <= 0 {
		return fmt.Errorf("%w: tool timeout must be positive", domain.ErrValidation)
	}
	if call.TenantContext.TenantID <= 0 || strings.TrimSpace(call.RequestID) == "" || strings.TrimSpace(call.ToolID) == "" || strings.TrimSpace(call.ToolVersion) == "" {
		return fmt.Errorf("%w: tenant, request, tool, and version are required", domain.ErrValidation)
	}
	return nil
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
