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
	// A zero value below takes the consumer's default; a negative one refuses composition.
	// CostWindow is the cost breaker's trailing window.
	CostWindow time.Duration
	// ReplyRetention is each reply frame's cache lifetime; ReplyPollInterval bounds delivery
	// latency after a lost wake-up; ReplyRecordReadInterval throttles the durable fallback read.
	ReplyRetention          time.Duration
	ReplyPollInterval       time.Duration
	ReplyRecordReadInterval time.Duration
	// CancelPollInterval bounds how long a running turn outlives its cancellation;
	// CancelMaxReadFailures consecutive failed reads stop the turn.
	CancelPollInterval    time.Duration
	CancelMaxReadFailures int
	// SessionRevisionMemory is how many conversations' last written revisions are remembered.
	SessionRevisionMemory int
	// ResultCardMaxBytes is the largest response copied into turn history; a larger one is referenced.
	ResultCardMaxBytes int
	// RunPageMax is the largest page of settled agent runs one query returns.
	RunPageMax int
	// AuditPageSize and AuditMaxPageSize size the decision audit's pages, SafetyEventMaxPage caps a
	// page of safety events, and RestrictionCacheTTL and RestrictionCacheEntries bound the cached
	// restriction layers; zero takes each component's default.
	AuditPageSize           int
	AuditMaxPageSize        int
	SafetyEventMaxPage      int
	RestrictionCacheTTL     time.Duration
	RestrictionCacheEntries int
}
