package domain

import "time"

// DataPlaneSettings are the deployment knobs of the table-backed data plane.
type DataPlaneSettings struct {
	// StateBucket holds turn input and conversation state; it must not be a publicly served bucket.
	StateBucket   string
	StateMaxBytes int64
	// QueuePartitions is the fixed partition pool; QueueShards the subset one tenant spreads over.
	QueuePartitions  int
	QueueShards      int
	QueueMaxAttempts int
	// QueueLease is how long one claimed turn stays leased; it must outlast the loop deadline.
	QueueLease       time.Duration
	QueueBatch       int
	SessionCacheSize int
	SessionCacheTTL  time.Duration
	GraphCacheSize   int
	GraphCacheTTL    time.Duration
	// StepClaimLease is how long a step claim blocks other workers; it should outlast the loop deadline.
	StepClaimLease time.Duration
	TurnMaxSteps   int
	ToolTimeout    time.Duration
	// ToolMaxAttempts bounds deliveries of one tool call that registers no ceiling of its own.
	ToolMaxAttempts         int
	GuardrailMaxInputBytes  int
	GuardrailMaxOutputBytes int
	// ModelRegion stamps a candidate model with no route row of its own; empty leaves it unknown.
	ModelRegion string
}
