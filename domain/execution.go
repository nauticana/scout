package domain

// ExecutionGraph is an immutable compiled agent definition.
type ExecutionGraph struct {
	AgentID     string
	Version     string
	Digest      string
	EntryStepID string
	Steps       []ExecutionStep
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

// StepInput contains the step and state required for execution.
type StepInput struct {
	Step     ExecutionStep
	Snapshot SessionSnapshot
}

// StepResult contains state, routing, and usage produced by a step.
type StepResult struct {
	State       []byte
	NextStepID  string
	Fingerprint string
	Usage       Usage
}
