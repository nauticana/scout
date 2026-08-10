package domain

// ExecutionGraph is an immutable compiled agent definition.
type ExecutionGraph struct {
	AgentID     string
	Version     string
	Digest      string
	EntryStepID string
	Steps       []ExecutionStep
}

// ExecutionStep describes one node in an agent execution graph.
type ExecutionStep struct {
	StepID        string
	Kind          string
	Configuration []byte
	NextStepIDs   []string
}

// StepCheckpoint contains durable state after a completed step.
type StepCheckpoint struct {
	CheckpointID   string
	ConversationID string
	TurnNo         int64
	StepNo         int
	RequestID      string
	StepID         string
	State          []byte
	Usage          Usage
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
