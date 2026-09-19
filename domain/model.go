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
	// Messages continue the conversation after Prompt: earlier model turns with
	// their tool calls, and the observations answering them.
	Messages []ModelMessage
	// Tools are the pinned tool versions the model may call; a non-empty list
	// requires the CapabilityTools route capability.
	Tools []ModelTool
	// Output constrains the terminal answer; a set Mode requires CapabilityStructuredOutput
	// and is never downgraded to free text.
	Output OutputConstraint
}

// Route capabilities a request can require. Tools and a constrained Output imply
// theirs, so a caller cannot forget to ask.
const (
	CapabilityTools            = "tools"
	CapabilityStructuredOutput = "structured_output"
)

// ModelTool is one pinned tool version offered to the model. Name is the
// provider-safe identifier the model calls it by; InputSchema is JSON Schema.
type ModelTool struct {
	Name        string
	Description string
	ToolID      string
	ToolVersion string
	InputSchema []byte
}

// ModelToolCall is one structured call the model proposed. CallID pairs it with
// its observation; adapters synthesize one where the provider sends none.
type ModelToolCall struct {
	CallID    string
	Name      string
	Arguments []byte
}

// ModelToolObservation answers one ModelToolCall.
type ModelToolObservation struct {
	CallID  string
	Name    string
	Output  []byte
	IsError bool
}

// ModelRole names who produced a ModelMessage.
type ModelRole string

const (
	ModelRoleUser      ModelRole = "user"
	ModelRoleAssistant ModelRole = "assistant"
	ModelRoleTool      ModelRole = "tool"
)

// ModelMessage is one provider-neutral conversation entry. An assistant message
// carries Text and ToolCalls; a tool message carries Observations.
type ModelMessage struct {
	Role         ModelRole
	Text         []byte
	ToolCalls    []ModelToolCall
	Observations []ModelToolObservation
}

// OutputMode selects how the terminal answer is constrained.
type OutputMode string

const (
	OutputModeText       OutputMode = ""
	OutputModeJSONSchema OutputMode = "json_schema"
)

// OutputConstraint asks the provider to decode against Schema natively.
type OutputConstraint struct {
	Mode       OutputMode
	SchemaName string
	Schema     []byte
}

// FinishReasonInterrupted ends a stream that was cut after its first token; the
// output delivered so far is a partial completion, never restarted or spliced.
const FinishReasonInterrupted = "interrupted"

// FinishReasonToolCalls is the normalized reason every adapter reports when the
// model stopped to call tools; other reasons stay provider-specific.
const FinishReasonToolCalls = "tool_calls"

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
	ToolCalls    []ModelToolCall
	FinishReason string
	Usage        Usage
}

// ModelChunk is one ordered frame with incremental usage from a streaming response.
type ModelChunk struct {
	Sequence     int64
	Payload      []byte
	ToolCalls    []ModelToolCall
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
