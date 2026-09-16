package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// GuardrailEnforcer contains configurable guardrail callbacks; nil funcs pass values through.
type GuardrailEnforcer struct {
	BeforeModelFunc     func(context.Context, domain.GuardrailConfig, domain.ModelRequest) (domain.ModelRequest, error)
	AfterModelChunkFunc func(context.Context, domain.GuardrailConfig, domain.GuardrailSubject, domain.ModelChunk) (domain.ModelChunk, error)
	BeforeToolFunc      func(context.Context, domain.GuardrailConfig, domain.ToolCall) (domain.ToolCall, error)
	AfterToolFunc       func(context.Context, domain.GuardrailConfig, domain.GuardrailSubject, domain.ToolResult) (domain.ToolResult, error)
}

func (enforcer *GuardrailEnforcer) BeforeModel(ctx context.Context, config domain.GuardrailConfig, request domain.ModelRequest) (domain.ModelRequest, error) {
	if enforcer.BeforeModelFunc == nil {
		return request, nil
	}
	return enforcer.BeforeModelFunc(ctx, config, request)
}

func (enforcer *GuardrailEnforcer) AfterModelChunk(ctx context.Context, config domain.GuardrailConfig, subject domain.GuardrailSubject, chunk domain.ModelChunk) (domain.ModelChunk, error) {
	if enforcer.AfterModelChunkFunc == nil {
		return chunk, nil
	}
	return enforcer.AfterModelChunkFunc(ctx, config, subject, chunk)
}

func (enforcer *GuardrailEnforcer) BeforeTool(ctx context.Context, config domain.GuardrailConfig, call domain.ToolCall) (domain.ToolCall, error) {
	if enforcer.BeforeToolFunc == nil {
		return call, nil
	}
	return enforcer.BeforeToolFunc(ctx, config, call)
}

func (enforcer *GuardrailEnforcer) AfterTool(ctx context.Context, config domain.GuardrailConfig, subject domain.GuardrailSubject, result domain.ToolResult) (domain.ToolResult, error) {
	if enforcer.AfterToolFunc == nil {
		return result, nil
	}
	return enforcer.AfterToolFunc(ctx, config, subject, result)
}

var _ contract.GuardrailEnforcer = (*GuardrailEnforcer)(nil)

// ClassifierProviderFunc adapts a function to contract.ClassifierProvider.
type ClassifierProviderFunc func(context.Context, domain.GuardrailRuleKind, []byte) (domain.Classification, error)

// Classify invokes the configured function.
func (function ClassifierProviderFunc) Classify(ctx context.Context, kind domain.GuardrailRuleKind, content []byte) (domain.Classification, error) {
	return function(ctx, kind, content)
}

// SafetyEventSink records every event in memory; RecordFunc, when set, runs first and may fail.
type SafetyEventSink struct {
	RecordFunc func(context.Context, domain.SafetyEvent) error
	Events     []domain.SafetyEvent
}

// Record appends the event after RecordFunc succeeds.
func (sink *SafetyEventSink) Record(ctx context.Context, event domain.SafetyEvent) error {
	if sink.RecordFunc != nil {
		if err := sink.RecordFunc(ctx, event); err != nil {
			return err
		}
	}
	sink.Events = append(sink.Events, event)
	return nil
}

// ToolApprovalGateFunc adapts a function to contract.ToolApprovalGate.
type ToolApprovalGateFunc func(context.Context, domain.ToolCall, string) (domain.ApprovalDecision, error)

// Decide invokes the configured function.
func (function ToolApprovalGateFunc) Decide(ctx context.Context, call domain.ToolCall, ruleID string) (domain.ApprovalDecision, error) {
	return function(ctx, call, ruleID)
}

// GuardrailOutputSession contains configurable session callbacks; nil funcs pass chunks through.
type GuardrailOutputSession struct {
	InspectFunc func(context.Context, domain.ModelChunk) (domain.ModelChunk, bool, error)
	FlushFunc   func(context.Context) ([]domain.ModelChunk, error)
	Closed      int
}

// Inspect invokes InspectFunc.
func (session *GuardrailOutputSession) Inspect(ctx context.Context, chunk domain.ModelChunk) (domain.ModelChunk, bool, error) {
	if session.InspectFunc == nil {
		return chunk, false, nil
	}
	return session.InspectFunc(ctx, chunk)
}

// Flush invokes FlushFunc.
func (session *GuardrailOutputSession) Flush(ctx context.Context) ([]domain.ModelChunk, error) {
	if session.FlushFunc == nil {
		return nil, nil
	}
	return session.FlushFunc(ctx)
}

// Close counts closes.
func (session *GuardrailOutputSession) Close() error {
	session.Closed++
	return nil
}

// StreamingGuardrailFunc adapts a function to contract.StreamingGuardrail.
type StreamingGuardrailFunc func(context.Context, domain.GuardrailConfig, domain.GuardrailSubject) (contract.GuardrailOutputSession, error)

// OpenOutputSession invokes the configured function.
func (function StreamingGuardrailFunc) OpenOutputSession(ctx context.Context, config domain.GuardrailConfig, subject domain.GuardrailSubject) (contract.GuardrailOutputSession, error) {
	return function(ctx, config, subject)
}

var (
	_ contract.ClassifierProvider     = ClassifierProviderFunc(nil)
	_ contract.SafetyEventSink        = (*SafetyEventSink)(nil)
	_ contract.ToolApprovalGate       = ToolApprovalGateFunc(nil)
	_ contract.GuardrailOutputSession = (*GuardrailOutputSession)(nil)
	_ contract.StreamingGuardrail     = StreamingGuardrailFunc(nil)
)
