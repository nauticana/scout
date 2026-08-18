package domain

import "time"

// ModelRequest contains one tenant-scoped inference request.
type ModelRequest struct {
	TenantContext TenantContext
	// Principal attributes routing decisions and spend; provider adapters must
	// never forward it to a vendor.
	Principal         PrincipalRef
	RequestID         string
	ConversationID    string
	ComplexitySignals map[string]float64
	Prompt            []byte
	MaxOutputTokens   int64
	// RequiredCapabilities names model capabilities the request cannot do without, e.g. "tools", "vision".
	RequiredCapabilities []string
	// AffinityKey groups requests that benefit from landing on the same route (session or prefix cache).
	AffinityKey string
	// Idempotent marks a request safe to hedge or retry on another route.
	Idempotent bool
	// ExcludedRouteIDs are routes a hedge or retry must avoid; the router treats them as ineligible.
	ExcludedRouteIDs []string
}

// FinishReasonInterrupted ends a stream that was cut after its first token; the
// output delivered so far is a partial completion, never restarted or spliced.
const FinishReasonInterrupted = "interrupted"

// ModelSelection identifies the chosen provider, model, and capacity pool, plus
// the routing provenance needed by usage, audit, rollout, and hedging consumers.
type ModelSelection struct {
	Provider     string
	Model        string
	ModelVersion string
	Region       string
	// RouteID identifies the replica or endpoint the request was bound to.
	RouteID      string
	CapacityPool string
	// RoutingGeneration is the catalog/snapshot generation the router decided from.
	RoutingGeneration int64
	// Reason is the auditable routing explanation; empty when selected by hand.
	Reason string
}

// ModelResult contains model output, termination reason, and usage.
type ModelResult struct {
	Output       []byte
	FinishReason string
	Usage        Usage
}

// ModelChunk is one ordered frame with incremental usage from a streaming response.
type ModelChunk struct {
	Sequence     int64
	Payload      []byte
	FinishReason string
	Usage        Usage
}

// ModelCandidate is one route the catalog offers a tenant, before capacity is considered.
type ModelCandidate struct {
	Provider         string
	Model            string
	ModelVersion     string
	Region           string
	RouteID          string
	QualityClass     int
	Capabilities     []string
	MaxContextTokens int64
	MaxOutputTokens  int64
}

// ModelCandidateSet is an immutable catalog view stamped with its generation.
type ModelCandidateSet struct {
	Generation int64
	Candidates []ModelCandidate
}

// CapacitySnapshot is the last health report published for one route.
// Draining routes admit nothing new; streams already running continue until
// DrainDeadline, after which the gateway ends them with an interrupted partial completion.
type CapacitySnapshot struct {
	Provider      string
	Model         string
	Region        string
	RouteID       string
	Healthy       bool
	Draining      bool
	DrainDeadline time.Time
	// Warm reports the model is loaded; a cold route ranks after warm ones.
	Warm bool
	// Owner names the serving control-plane unit that currently owns the route.
	Owner               string
	PredictedQueueDelay time.Duration
	TimeToFirstToken    time.Duration
	TimePerOutputToken  time.Duration
	// ServiceRate is the live decode throughput in tokens per second.
	ServiceRate     float64
	PrefillCapacity int64
	DecodeCapacity  int64
	// KVPressure is the provider-reported cache pressure in [0,1] when known.
	KVPressure float64
	ObservedAt time.Time
	Generation int64
}

// RoutingPolicy is the tenant's explicit routing and degradation preference.
type RoutingPolicy struct {
	// AllowedRegions restricts residency; empty allows every region.
	AllowedRegions []string
	// MinQualityClass rejects candidates below this class.
	MinQualityClass int
	// Fallbacks are tried in order when no preferred candidate is feasible; never implicit.
	Fallbacks []ModelReference
	// MaxSnapshotAge treats older capacity snapshots as unknown; zero uses the router default.
	MaxSnapshotAge time.Duration
	// PreferAffinity scores route stickiness while latency stays within budget.
	PreferAffinity bool
	// AllowUnknownCapacity admits routes with no fresh capacity snapshot; default is fail closed.
	AllowUnknownCapacity bool
}

// ServingSample is one route-attributed observation feeding serving-signal aggregation.
// Zero durations and counts mean "not observed", never "observed as zero".
type ServingSample struct {
	Selection          ModelSelection
	QueueWait          time.Duration
	TimeToFirstToken   time.Duration
	TimePerOutputToken time.Duration
	// PrefillTokens is the estimated prompt work admitted to the route.
	PrefillTokens int64
	// DecodeTokens is the requested output budget admitted to the route.
	DecodeTokens int64
	// AdmissionRejected marks a tenant-level admission rejection before any route work.
	AdmissionRejected bool
	// CapacityOutcome is a bounded outcome label such as "granted", "rejected", or "canceled".
	CapacityOutcome string
	// KVPressure is the provider-reported cache pressure in [0,1] when known.
	KVPressure float64
}
