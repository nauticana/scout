package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// GuardrailEnforcer applies pinned tenant policy at every runtime boundary.
type GuardrailEnforcer interface {
	// BeforeModel validates and transforms model input.
	BeforeModel(ctx context.Context, config domain.GuardrailConfig, request domain.ModelRequest) (domain.ModelRequest, error)
	// AfterModelChunk validates and transforms streamed model output.
	AfterModelChunk(ctx context.Context, config domain.GuardrailConfig, chunk domain.ModelChunk) (domain.ModelChunk, error)
	// BeforeTool validates and transforms tool arguments.
	BeforeTool(ctx context.Context, config domain.GuardrailConfig, call domain.ToolCall) (domain.ToolCall, error)
	// AfterTool validates and transforms tool output.
	AfterTool(ctx context.Context, config domain.GuardrailConfig, result domain.ToolResult) (domain.ToolResult, error)
}

// StreamingGuardrail is the optional stateful output capability of an enforcer:
// one session per streamed reply owns cross-chunk state and bounded buffering.
type StreamingGuardrail interface {
	OpenOutputSession(ctx context.Context, config domain.GuardrailConfig, subject domain.GuardrailSubject) (GuardrailOutputSession, error)
}

// GuardrailOutputSession inspects one ordered model stream; it is not safe for concurrent use.
type GuardrailOutputSession interface {
	// Inspect returns the approved part of the chunk; held reports that bytes were retained for lookback.
	// After a violation no further bytes are ever released.
	Inspect(ctx context.Context, chunk domain.ModelChunk) (approved domain.ModelChunk, held bool, err error)
	// Flush inspects and releases the retained tail at end of stream.
	Flush(ctx context.Context) ([]domain.ModelChunk, error)
	// Close discards retained state; it is idempotent.
	Close() error
}

// ClassifierProvider scores content for one or more classifier rule kinds.
type ClassifierProvider interface {
	Classify(ctx context.Context, kind domain.GuardrailRuleKind, content []byte) (domain.Classification, error)
}

// GuardrailRuleCompiler validates a rule envelope at publication time.
type GuardrailRuleCompiler interface {
	// Validate parses the envelope, verifies RulesDigest, and rejects unsupported or unsafe rules.
	Validate(ctx context.Context, config domain.GuardrailConfig) (domain.GuardrailRuleSet, error)
}

// SafetyEventSink durably records redacted rule hits for rollout and audit consumers.
type SafetyEventSink interface {
	Record(ctx context.Context, event domain.SafetyEvent) error
}

// ToolApprovalGate decides whether an irreversible tool call may proceed.
type ToolApprovalGate interface {
	Decide(ctx context.Context, call domain.ToolCall, ruleID string) (domain.ApprovalDecision, error)
}
