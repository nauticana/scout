package domain

import "time"

// RunTraceEventKind classifies one event of a run trace; it is never stored.
type RunTraceEventKind string

const (
	RunTraceEventModelStep RunTraceEventKind = "model_step"
	RunTraceEventToolCall  RunTraceEventKind = "tool_call"
	RunTraceEventGuardrail RunTraceEventKind = "guardrail"
	RunTraceEventApproval  RunTraceEventKind = "approval"
	RunTraceEventDecision  RunTraceEventKind = "decision"
)

// RunTraceFinish is how a model step ended: with the answer, or by proposing tool calls.
type RunTraceFinish string

const (
	RunTraceFinishAnswer    RunTraceFinish = "answer"
	RunTraceFinishToolCalls RunTraceFinish = "tool_calls"
)

// RunTraceToolOutcome is whether a journaled tool call returned output or a classified error.
type RunTraceToolOutcome string

const (
	RunTraceToolOK    RunTraceToolOutcome = "ok"
	RunTraceToolError RunTraceToolOutcome = "error"
)

// RunTraceDecisionSource names the record a governed decision event was read from.
type RunTraceDecisionSource string

const (
	RunTraceDecisionAudit   RunTraceDecisionSource = "audit"
	RunTraceDecisionCharter RunTraceDecisionSource = "charter"
)

// RunTrace is one settled run's ordered, payload-free trace.
type RunTrace struct {
	Run AgentRun
	// Turn is nil for a run outside a turn or one whose turn record is gone.
	Turn      *RunTraceTurn
	Events    []RunTraceEvent
	Truncated bool
}

// RunTraceTurn is the timing and status of the run's durable turn; a zero time was never reached.
type RunTraceTurn struct {
	ConversationID  string
	TurnNo          int64
	Status          TurnStatus
	QueuedAt        time.Time
	StartedAt       time.Time
	FirstResponseAt time.Time
	CompletedAt     time.Time
}

// RunTraceEvent has exactly the detail matching Kind and a one-based Sequence.
type RunTraceEvent struct {
	Sequence  int
	Kind      RunTraceEventKind
	At        time.Time
	Model     *RunTraceModelStep
	Tool      *RunTraceToolCall
	Guardrail *RunTraceGuardrailHit
	Approval  *RunTraceApproval
	Decision  *RunTraceDecisionEvent
}

// RunTraceJournalRef locates a journaled event; Digest is the SHA-256 of the entry that holds its content.
type RunTraceJournalRef struct {
	ExecutionStepID int64
	EntryNo         int
	Iteration       int
	Digest          string
}

// RunTraceModelStep is one model decision of a tool loop.
type RunTraceModelStep struct {
	Journal RunTraceJournalRef
	// Models are the models the step's usage is attributed to.
	Models    []ModelReference
	Usage     Usage
	Finish    RunTraceFinish
	ToolCalls int
}

// RunTraceToolCall is one journaled tool observation.
type RunTraceToolCall struct {
	Journal    RunTraceJournalRef
	Tool       ToolReference
	Outcome    RunTraceToolOutcome
	ErrorClass string
	Effect     EffectStatus
	Reconciled bool
	Usage      Usage
}

// RunTraceGuardrailHit is one recorded guardrail intervention.
type RunTraceGuardrailHit struct {
	Stage          GuardrailStage
	Layer          GuardrailLayer
	Action         GuardrailAction
	Severity       GuardrailSeverity
	RuleIDs        []string
	ReleaseVersion string
	PolicyVersion  string
	Duration       time.Duration
}

// RunTraceApproval is one approval request and its verdict, without summary or evidence.
type RunTraceApproval struct {
	ID              int64
	ExecutionStepID int64
	RequestedAction string
	Resource        string
	RuleSource      string
	RuleID          string
	RiskTier        RiskTier
	Status          ApprovalStatus
	ProposedDigest  string
	RevisesStepID   int64
	ResolvedAt      time.Time
	DeciderKind     PrincipalKind
	DecidedAt       time.Time
}

// RunTraceDecisionEvent is an audit decision or a Charter disposition.
type RunTraceDecisionEvent struct {
	Source         RunTraceDecisionSource
	Category       string
	Action         string
	Resource       string
	Outcome        DecisionOutcome
	PolicyID       string
	PolicyVersion  string
	ReleaseVersion string
	Authority      AuthorityRef
	Obligations    []ObligationKind
	EvidenceDigest string
	Charter        *CharterAction
}

// CharterAction is Charter's payload-free disposition for one governed action.
type CharterAction struct {
	ActionID             CharterRef
	Capability           CharterRef
	OperationClass       string
	Disposition          string
	Outcome              string
	Assignment           *CharterRef
	Authority            []CharterEvaluation
	Approvals            []CharterEvaluation
	Reconciles           *CharterRef
	SodException         *CharterRef
	MaterialInputsDigest string
	At                   time.Time
}

// CharterEvaluation is one evaluation result and its source record.
type CharterEvaluation struct {
	Ref    *CharterRef
	Result string
}

// RunTraceLimits bound returned events and journal classification; zero takes the default.
type RunTraceLimits struct {
	MaxEvents         int
	MaxJournalEntries int
	MaxJournalBytes   int64
}
