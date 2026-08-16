package domain

import (
	"encoding/json"
	"time"
)

// GuardrailConfig is an immutable set of tenant policy rules; RulesDigest is the SHA-256 hex of Rules.
type GuardrailConfig struct {
	Version     string
	RulesDigest string
	Rules       []byte
}

// GuardrailLayer identifies which policy layer a rule belongs to.
type GuardrailLayer string

const (
	GuardrailLayerBaseline GuardrailLayer = "baseline"
	GuardrailLayerRelease  GuardrailLayer = "release"
)

// GuardrailStage is the runtime boundary a rule applies to.
type GuardrailStage string

const (
	GuardrailStageInput      GuardrailStage = "input"
	GuardrailStageOutput     GuardrailStage = "output"
	GuardrailStageToolInput  GuardrailStage = "tool_input"
	GuardrailStageToolOutput GuardrailStage = "tool_output"
	GuardrailStageRetrieval  GuardrailStage = "retrieval"
)

// GuardrailAction is what a matching rule does to the inspected content.
type GuardrailAction string

const (
	GuardrailActionBlock  GuardrailAction = "block"
	GuardrailActionRedact GuardrailAction = "redact"
	GuardrailActionFlag   GuardrailAction = "flag"
)

// GuardrailSeverity classifies a rule hit for rollout and audit consumers.
type GuardrailSeverity string

const (
	GuardrailSeverityHard GuardrailSeverity = "hard"
	GuardrailSeveritySoft GuardrailSeverity = "soft"
)

// GuardrailRuleKind names a deterministic structural control or a classifier provider.
type GuardrailRuleKind string

const (
	GuardrailKindMaxInputBytes            GuardrailRuleKind = "max_input_bytes"
	GuardrailKindMaxOutputBytes           GuardrailRuleKind = "max_output_bytes"
	GuardrailKindJSONSchema               GuardrailRuleKind = "json_schema"
	GuardrailKindToolAllowlist            GuardrailRuleKind = "tool_allowlist"
	GuardrailKindDestinationAllowlist     GuardrailRuleKind = "destination_allowlist"
	GuardrailKindExactPhrase              GuardrailRuleKind = "exact_phrase"
	GuardrailKindRegex                    GuardrailRuleKind = "regex"
	GuardrailKindUntrustedContentMarker   GuardrailRuleKind = "untrusted_content_marker"
	GuardrailKindIrreversibleToolApproval GuardrailRuleKind = "irreversible_tool_approval"
	GuardrailKindPII                      GuardrailRuleKind = "pii"
	GuardrailKindToxicity                 GuardrailRuleKind = "toxicity"
	GuardrailKindMalware                  GuardrailRuleKind = "malware"
	GuardrailKindPromptInjection          GuardrailRuleKind = "prompt_injection"
	GuardrailKindJailbreak                GuardrailRuleKind = "jailbreak"
)

// GuardrailRule is one versioned policy rule; Params is kind-specific JSON.
type GuardrailRule struct {
	ID       string            `json:"id"`
	Kind     GuardrailRuleKind `json:"kind"`
	Stages   []GuardrailStage  `json:"stages,omitempty"`
	Action   GuardrailAction   `json:"action"`
	Severity GuardrailSeverity `json:"severity"`
	Params   json.RawMessage   `json:"params,omitempty"`
}

// GuardrailRuleSet is the typed, versioned envelope stored in GuardrailConfig.Rules.
type GuardrailRuleSet struct {
	SchemaVersion int             `json:"schema_version"`
	Rules         []GuardrailRule `json:"rules"`
}

// GuardrailVerdict summarizes one stage evaluation without carrying content.
type GuardrailVerdict struct {
	Allowed       bool
	RuleIDs       []string
	Severity      GuardrailSeverity
	RedactedBytes int
	Version       string
}

// GuardrailSubject identifies the request an inspection belongs to; ReleaseVersion is the agent version.
type GuardrailSubject struct {
	TenantID       int64
	RequestID      string
	ConversationID string
	ReleaseVersion string
}

// SafetyEvent is the redacted record of a rule hit; it never carries inspected content.
type SafetyEvent struct {
	TenantID       int64
	Stage          GuardrailStage
	Layer          GuardrailLayer
	Action         GuardrailAction
	RuleIDs        []string
	Severity       GuardrailSeverity
	ReleaseVersion string
	PolicyVersion  string
	Duration       time.Duration
	OccurredAt     time.Time
}

// ContentSpan is a half-open byte range [Start, End) inside inspected content.
type ContentSpan struct {
	Start int
	End   int
}

// Classification is a provider's bounded verdict on one piece of content.
type Classification struct {
	// Score is in [0,1]; rules compare it against their threshold.
	Score float64
	// Spans locate redactable matches; empty with a matching score redacts everything.
	Spans []ContentSpan
}

// ApprovalDecision is the state of an irreversible tool call in the approval gate.
type ApprovalDecision string

const (
	ApprovalApproved ApprovalDecision = "approved"
	ApprovalPending  ApprovalDecision = "pending"
	ApprovalDenied   ApprovalDecision = "denied"
)
