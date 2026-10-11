package api

import "time"

// StudioRunTracePath serves GET runs/trace?agent_name=&run_id=.
const StudioRunTracePath = StudioBasePath + "runs/trace"

// RunTrace is one run's ordered, payload-free trace; Truncated means later events are missing.
type RunTrace struct {
	Run       AgentRun        `json:"run"`
	Turn      *RunTraceTurn   `json:"turn,omitempty"`
	Events    []RunTraceEvent `json:"events"`
	Truncated bool            `json:"truncated"`
}

// RunTraceTurn is the run's turn; status is a turn_status code.
type RunTraceTurn struct {
	ConversationID  string     `json:"conversation_id"`
	TurnNo          int64      `json:"turn_no"`
	Status          string     `json:"status"`
	QueuedAt        time.Time  `json:"queued_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FirstResponseAt *time.Time `json:"first_response_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

// RunTraceEvent holds the detail member named by Kind.
type RunTraceEvent struct {
	Sequence  int                   `json:"sequence"`
	Kind      string                `json:"kind"`
	At        time.Time             `json:"at"`
	ModelStep *RunTraceModelStep    `json:"model_step,omitempty"`
	ToolCall  *RunTraceToolCall     `json:"tool_call,omitempty"`
	Guardrail *RunTraceGuardrailHit `json:"guardrail,omitempty"`
	Approval  *RunTraceApproval     `json:"approval,omitempty"`
	Decision  *RunTraceDecision     `json:"decision,omitempty"`
}

// RunTraceJournal locates a journaled event; digest is the SHA-256 of the entry holding its content.
type RunTraceJournal struct {
	ExecutionStepID int64  `json:"execution_step_id"`
	EntryNo         int    `json:"entry_no"`
	Iteration       int    `json:"iteration"`
	Digest          string `json:"digest"`
}

// RunTraceModel names a model by provider and id.
type RunTraceModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// RunTraceModelStep is one model decision; finish is answer or tool_calls.
type RunTraceModelStep struct {
	Journal   RunTraceJournal  `json:"journal"`
	Models    []RunTraceModel  `json:"models,omitempty"`
	Usage     *TrajectoryUsage `json:"usage,omitempty"`
	Finish    string           `json:"finish"`
	ToolCalls int              `json:"tool_calls,omitempty"`
}

// RunTraceToolCall is one tool observation with a bounded error class and observed effect.
type RunTraceToolCall struct {
	Journal     RunTraceJournal  `json:"journal"`
	ToolID      string           `json:"tool_id"`
	ToolVersion string           `json:"tool_version,omitempty"`
	Outcome     string           `json:"outcome"`
	ErrorClass  string           `json:"error_class,omitempty"`
	Effect      string           `json:"effect,omitempty"`
	Reconciled  bool             `json:"reconciled,omitempty"`
	Usage       *TrajectoryUsage `json:"usage,omitempty"`
}

// RunTraceGuardrailHit is one guardrail intervention.
type RunTraceGuardrailHit struct {
	Stage          string   `json:"stage"`
	Layer          string   `json:"layer"`
	Action         string   `json:"action"`
	Severity       string   `json:"severity"`
	RuleIDs        []string `json:"rule_ids"`
	ReleaseVersion string   `json:"release_version,omitempty"`
	PolicyVersion  string   `json:"policy_version,omitempty"`
	DurationMs     int64    `json:"duration_ms"`
}

// RunTraceApproval is one approval request and its verdict.
type RunTraceApproval struct {
	ID              int64      `json:"id"`
	ExecutionStepID int64      `json:"execution_step_id"`
	RequestedAction string     `json:"requested_action"`
	Resource        string     `json:"resource,omitempty"`
	RuleSource      string     `json:"rule_source"`
	RuleID          string     `json:"rule_id"`
	RiskTier        string     `json:"risk_tier"`
	Status          string     `json:"status"`
	ProposedDigest  string     `json:"proposed_digest"`
	RevisesStepID   int64      `json:"revises_step_id,omitempty"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	DeciderKind     string     `json:"decider_kind,omitempty"`
	DecidedAt       *time.Time `json:"decided_at,omitempty"`
}

// RunTraceDecision is an audit decision or a Charter disposition.
type RunTraceDecision struct {
	Source         string          `json:"source"`
	Category       string          `json:"category,omitempty"`
	Action         string          `json:"action"`
	Resource       string          `json:"resource,omitempty"`
	Outcome        string          `json:"outcome,omitempty"`
	PolicyID       string          `json:"policy_id,omitempty"`
	PolicyVersion  string          `json:"policy_version,omitempty"`
	ReleaseVersion string          `json:"release_version,omitempty"`
	PrincipalKind  string          `json:"principal_kind,omitempty"`
	PrincipalID    string          `json:"principal_id,omitempty"`
	GrantID        string          `json:"grant_id,omitempty"`
	GrantorKind    string          `json:"grantor_kind,omitempty"`
	GrantorID      string          `json:"grantor_id,omitempty"`
	Obligations    []string        `json:"obligations,omitempty"`
	EvidenceDigest string          `json:"evidence_digest,omitempty"`
	Charter        *RunTraceAction `json:"charter,omitempty"`
}

// RunTraceAction is the disposition Charter recorded for one governed action.
type RunTraceAction struct {
	ActionID             RunTraceRef          `json:"action_id"`
	Capability           RunTraceRef          `json:"capability"`
	OperationClass       string               `json:"operation_class"`
	Disposition          string               `json:"disposition"`
	Outcome              string               `json:"outcome,omitempty"`
	Assignment           *RunTraceRef         `json:"assignment,omitempty"`
	Authority            []RunTraceEvaluation `json:"authority,omitempty"`
	Approvals            []RunTraceEvaluation `json:"approvals,omitempty"`
	Reconciles           *RunTraceRef         `json:"reconciles,omitempty"`
	SodException         *RunTraceRef         `json:"sod_exception,omitempty"`
	MaterialInputsDigest string               `json:"material_inputs_digest,omitempty"`
}

// RunTraceRef is a Charter document reference.
type RunTraceRef struct {
	Namespace string `json:"namespace,omitempty"`
	ID        string `json:"id"`
}

// RunTraceEvaluation is one Charter evaluation result and its source record.
type RunTraceEvaluation struct {
	Ref    *RunTraceRef `json:"ref,omitempty"`
	Result string       `json:"result"`
}
