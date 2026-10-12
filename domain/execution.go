package domain

// ExecutionGraph is an immutable compiled agent definition.
type ExecutionGraph struct {
	AgentID     string
	Version     string
	Digest      string
	EntryStepID string
	Steps       []ExecutionStep
}

// Execution step kinds, the seeded execution_step_kind codes a compiled graph may contain.
const (
	StepKindModel     = "M"
	StepKindTool      = "T"
	StepKindKnowledge = "K"
	StepKindAgent     = "A"
	StepKindWait      = "W"
	// StepKindToolLoop runs a bounded model↔tool loop.
	StepKindToolLoop = "L"
)

// ValidStepKind reports whether kind is a seeded execution_step_kind code.
func ValidStepKind(kind string) bool {
	switch kind {
	case StepKindModel, StepKindTool, StepKindKnowledge, StepKindAgent, StepKindWait, StepKindToolLoop:
		return true
	default:
		return false
	}
}

// ExecutionStep describes one node in an agent execution graph. ExecutionStepID
// is the compiled surrogate identity persistence keys on; StepID is the logical
// name authors and transitions use.
type ExecutionStep struct {
	ExecutionStepID int64
	StepID          string
	Kind            string
	Configuration   []byte
	NextStepIDs     []string
}

// StepCheckpoint contains durable state after a completed step. State is the
// hydrated payload; StateRef is filled by the durable store once dehydrated.
type StepCheckpoint struct {
	ConversationID  string
	TurnNo          int64
	StepNo          int
	ExecutionStepID int64
	StepID          string
	IdempotencyKey  string
	Fingerprint     string
	State           []byte
	StateRef        ObjectRef
	Usage           Usage
}

// StepInput contains the step and state required for execution. Principal and
// Bounds travel with it so a step that delegates knows who is acting and how
// much authority is left to pass on.
type StepInput struct {
	Step      ExecutionStep
	Snapshot  SessionSnapshot
	Principal Principal
	// OnBehalfOf is the human the turn acts for; zero when it acts for no one.
	OnBehalfOf    PrincipalRef
	Bounds        DelegationBounds
	WorkItemID    int64
	WorkItemDepth int
	RequestID     string
	// Input is the turn's input, so a step can build its prompt from what was asked.
	Input []byte
	// Limits are the release's execution limits the turn runs under; a step only narrows its own ceilings by them.
	Limits ExecutionLimits
	// Spend is what the step may still spend of the release's per-turn budget.
	Spend StepSpend
	// Owner is the turn's acquired fence; every durable write the step makes presents it.
	Owner TurnOwner
	// HoldRequested reports a pending hold request; a step that can stop at a durable boundary
	// returns ErrTurnHeld there. Nil means no hold is ever requested.
	HoldRequested func() bool `json:"-"`
}

// StepResult contains state, routing, and usage produced by a step.
type StepResult struct {
	State       []byte
	NextStepID  string
	Fingerprint string
	Usage       Usage
	// Citations are the sources the step's grounded model calls used.
	Citations []Citation
	// Events are published on this step's reply frame, after its guardrails pass.
	Events []TurnEvent
}
