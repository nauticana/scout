package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
	"github.com/nauticana/scout/service/dataplane"
)

// ReleaseCaseExecutor replays a golden example against a published agent
// version — its compiled graph, prompt, tools and tool loop — with every tool
// call answered by a sandbox, so a replay has no path to a live tool.
type ReleaseCaseExecutor struct {
	Graphs   contract.DefinitionResolver
	Requests contract.ToolLoopRequestBuilder
	Router   contract.ModelRouter
	Models   contract.ModelGateway
	Registry contract.ToolRegistry
	// Payloads reads an example's payload, which is the turn input exactly as a conversation would submit it.
	Payloads contract.ObjectReader
	// Sandboxes returns a fresh sandbox per replay, primed with that example's tool results.
	Sandboxes func(ctx context.Context, example domain.GoldenExample) (contract.ToolSandbox, error)
	Limits    domain.ToolLoopLimits
	// Pricer, Evidence and Retrieval are optional as they are in the runtime.
	Pricer    contract.ModelPricer
	Evidence  contract.EvidenceValidator
	Retrieval contract.CaseRetriever
	// Guardrails and GuardrailConfigs apply the release's output guardrails; set both or neither.
	Guardrails       contract.GuardrailEnforcer
	GuardrailConfigs contract.GuardrailConfigRepository
	// MaxSteps bounds the graph walk; zero allows as many steps as the graph has.
	MaxSteps int
	Now      func() time.Time
}

var _ contract.CaseExecutor = (*ReleaseCaseExecutor)(nil)

// Trajectory state names a replay ends in.
const (
	ReplayCompleted = "completed"
	ReplaySuspended = "suspended"
	ReplayBlocked   = "blocked"
)

// replayOutcomes are failures of the agent under test rather than of the
// replay: they are scored as a failed trajectory, not returned as errors.
var replayOutcomes = []error{
	domain.ErrExecutionLimit, domain.ErrLoopDetected, domain.ErrInvalidModelOutput, domain.ErrUnsupportedClaim,
}

func (executor *ReleaseCaseExecutor) now() time.Time {
	if executor.Now != nil {
		return executor.Now()
	}
	return time.Now()
}

func (executor *ReleaseCaseExecutor) validate() error {
	switch {
	case executor.Graphs == nil, executor.Requests == nil, executor.Router == nil, executor.Models == nil,
		executor.Registry == nil, executor.Payloads == nil, executor.Sandboxes == nil:
		return fmt.Errorf("release case executor: graphs, request builder, router, model gateway, tool registry, payload reader, and sandboxes are required")
	case (executor.Guardrails == nil) != (executor.GuardrailConfigs == nil):
		return fmt.Errorf("release case executor: guardrails and guardrail configs are set together or not at all")
	case executor.MaxSteps < 0:
		return fmt.Errorf("%w: max steps cannot be negative", domain.ErrValidation)
	}
	return nil
}

// Execute replays the example against the subject's release. Component
// versions other than the agent and model are labels of what that release
// pins; an arm that varies one must name a release pinning it.
func (executor *ReleaseCaseExecutor) Execute(ctx context.Context, subject domain.EvaluationSubject, example domain.GoldenExample, preserved []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
	if err := executor.validate(); err != nil {
		return domain.EvaluationCase{}, err
	}
	if example.TenantID <= 0 || strings.TrimSpace(example.ExampleID) == "" {
		return domain.EvaluationCase{}, fmt.Errorf("%w: example tenant and id are required", domain.ErrValidation)
	}
	if strings.TrimSpace(subject.AgentID) == "" || strings.TrimSpace(subject.AgentVersion) == "" {
		return domain.EvaluationCase{}, fmt.Errorf("%w: subject agent and version are required", domain.ErrValidation)
	}
	if subject.Versions.Agent != "" && subject.Versions.Agent != subject.AgentVersion {
		return domain.EvaluationCase{}, fmt.Errorf("%w: subject pins agent version %q but replays %q", domain.ErrConflict, subject.Versions.Agent, subject.AgentVersion)
	}
	decoding, err := parseReplayDecoding(subject.Decoding)
	if err != nil {
		return domain.EvaluationCase{}, err
	}
	input, err := executor.payload(ctx, example)
	if err != nil {
		return domain.EvaluationCase{}, err
	}
	var matches []domain.KnowledgeMatch
	switch {
	case executor.Retrieval != nil:
		if input, matches, err = executor.Retrieval.Retrieve(ctx, subject, example, input, preserved); err != nil {
			return domain.EvaluationCase{}, fmt.Errorf("retrieve for example %q: %w", example.ExampleID, err)
		}
	case len(preserved) > 0:
		return domain.EvaluationCase{}, fmt.Errorf("%w: preserved retrieval needs a case retriever", domain.ErrCapabilityUnsupported)
	}
	graph, err := executor.Graphs.Resolve(ctx, example.TenantID, subject.AgentID, subject.AgentVersion)
	if err != nil {
		return domain.EvaluationCase{}, fmt.Errorf("resolve %s@%s: %w", subject.AgentID, subject.AgentVersion, err)
	}
	sandbox, err := executor.Sandboxes(ctx, example)
	if err != nil {
		return domain.EvaluationCase{}, fmt.Errorf("sandbox for example %q: %w", example.ExampleID, err)
	}
	journal := &dataplane.MemoryLoopJournal{}
	loop, err := dataplane.NewToolLoopExecutor(dataplane.ToolLoopExecutor{
		Requests: decodedRequests{ToolLoopRequestBuilder: executor.Requests, decoding: decoding},
		Router:   subjectRouter{ModelRouter: executor.Router, modelVersion: subject.Versions.Model},
		Models:   executor.Models, Registry: executor.Registry, Tools: sandbox,
		Journal: journal, Pricer: executor.Pricer, Evidence: executor.Evidence,
		Limits: executor.Limits, Now: executor.Now,
	})
	if err != nil {
		return domain.EvaluationCase{}, err
	}
	var guardrails domain.GuardrailConfig
	if executor.GuardrailConfigs != nil {
		if guardrails, err = executor.GuardrailConfigs.Get(ctx, example.TenantID, subject.AgentID, subject.AgentVersion); err != nil {
			return domain.EvaluationCase{}, fmt.Errorf("load guardrail config: %w", err)
		}
	}
	replay := &caseReplay{
		executor: executor, loop: loop, journal: journal, graph: graph, guardrails: guardrails, input: input, started: executor.now(),
		principal: domain.Principal{Kind: domain.PrincipalAgent, ID: subject.AgentID, TenantID: example.TenantID, Release: subject.AgentVersion},
		requestID: "evaluation/" + subject.AgentVersion + "/" + example.ExampleID,
	}
	if err := replay.run(ctx); err != nil {
		return domain.EvaluationCase{}, err
	}
	return domain.EvaluationCase{
		Example: example, Output: replay.output, Retrieval: matches, Trajectory: replay.trajectory,
		Latency: executor.now().Sub(replay.started), Usage: replay.usage,
	}, nil
}

func (executor *ReleaseCaseExecutor) payload(ctx context.Context, example domain.GoldenExample) ([]byte, error) {
	input, err := executor.Payloads.Read(ctx, example.Payload)
	if err != nil {
		return nil, fmt.Errorf("read payload of example %q: %w", example.ExampleID, err)
	}
	if digest := sha256Hex(input); digest != strings.ToLower(example.Payload.Digest) {
		return nil, fmt.Errorf("%w: payload of example %q has digest %s, not %s", domain.ErrValidation, example.ExampleID, digest, example.Payload.Digest)
	}
	return input, nil
}

// caseReplay walks one release graph for one example.
type caseReplay struct {
	executor   *ReleaseCaseExecutor
	loop       *dataplane.ToolLoopExecutor
	journal    contract.LoopJournal
	graph      domain.ExecutionGraph
	guardrails domain.GuardrailConfig
	principal  domain.Principal
	requestID  string
	input      []byte
	started    time.Time

	output     []byte
	usage      domain.Usage
	trajectory []domain.TrajectoryEvent
}

func (replay *caseReplay) run(ctx context.Context) error {
	steps := make(map[string]domain.ExecutionStep, len(replay.graph.Steps))
	for _, step := range replay.graph.Steps {
		steps[step.StepID] = step
	}
	maxSteps := replay.executor.MaxSteps
	if maxSteps == 0 {
		maxSteps = len(replay.graph.Steps)
	}
	snapshot := domain.SessionSnapshot{ConversationID: replay.requestID, AgentVersion: replay.principal.Release}
	stepID := replay.graph.EntryStepID
	for stepNo := 1; ; stepNo++ {
		if stepNo > maxSteps {
			return replay.end(fmt.Errorf("%w: replay exceeded %d steps", domain.ErrExecutionLimit, maxSteps))
		}
		step, found := steps[stepID]
		if !found {
			return fmt.Errorf("%w: execution step %q of agent %q", domain.ErrNotFound, stepID, replay.graph.AgentID)
		}
		if step.Kind != domain.StepKindToolLoop {
			return fmt.Errorf("%w: replay runs %s steps, not %q", domain.ErrCapabilityUnsupported, domain.StepKindToolLoop, step.Kind)
		}
		result, err := replay.loop.Execute(ctx, domain.StepInput{
			Step: step, Snapshot: snapshot, Principal: replay.principal, RequestID: replay.requestID, Input: replay.input,
		})
		if recordErr := replay.record(ctx, step); recordErr != nil {
			return errors.Join(err, recordErr)
		}
		var spent interface{ SpentUsage() domain.Usage }
		if errors.As(err, &spent) {
			replay.usage = addUsage(replay.usage, spent.SpentUsage())
		}
		if err != nil {
			return replay.end(err)
		}
		replay.usage = addUsage(replay.usage, result.Usage)
		state, blocked, err := replay.guard(ctx, result.State)
		if err != nil || blocked {
			return err
		}
		snapshot.State, snapshot.LastCompletedStepID = state, step.StepID
		if strings.TrimSpace(result.NextStepID) == "" {
			replay.output = state
			replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryState, Name: ReplayCompleted, Payload: state, Offset: replay.offset()})
			return nil
		}
		stepID = result.NextStepID
	}
}

// guard applies the release's output guardrails; a blocked output ends the replay.
func (replay *caseReplay) guard(ctx context.Context, state []byte) ([]byte, bool, error) {
	if replay.executor.Guardrails == nil {
		return state, false, nil
	}
	guarded, err := replay.executor.Guardrails.AfterModelChunk(ctx, replay.guardrails, domain.GuardrailSubject{
		TenantID: replay.principal.TenantID, Principal: domain.PrincipalRef{Kind: replay.principal.Kind, ID: replay.principal.ID},
		RequestID: replay.requestID, ConversationID: replay.requestID, ReleaseVersion: replay.principal.Release,
	}, domain.ModelChunk{Payload: state})
	if errors.Is(err, domain.ErrForbidden) {
		replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryPolicy, Name: "guardrail", Failed: true, Offset: replay.offset()})
		replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryState, Name: ReplayBlocked, Failed: true, Offset: replay.offset()})
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("apply output guardrails: %w", err)
	}
	return guarded.Payload, false, nil
}

// end records a failure of the agent under test as the trajectory's final
// state and returns every other failure.
func (replay *caseReplay) end(err error) error {
	if errors.Is(err, domain.ErrApprovalPending) {
		replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryState, Name: ReplaySuspended, Offset: replay.offset()})
		return nil
	}
	for _, outcome := range replayOutcomes {
		if errors.Is(err, outcome) {
			replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryState, Name: stage.ErrorClass(err), Failed: true, Offset: replay.offset()})
			return nil
		}
	}
	return err
}

// record reads the step's loop journal onto the trajectory, which also keeps
// the calls of a loop that failed before returning its events.
func (replay *caseReplay) record(ctx context.Context, step domain.ExecutionStep) error {
	entries, err := replay.journal.Load(ctx, domain.LoopKey{TenantID: replay.principal.TenantID, RequestID: replay.requestID, ExecutionStepID: step.ExecutionStepID})
	if err != nil {
		return fmt.Errorf("read replay journal: %w", err)
	}
	toolIDs := make(map[string]string)
	for _, entry := range entries {
		if entry.Kind == domain.LoopEntryObservation && entry.Observation != nil {
			toolIDs[entry.Observation.CallID] = entry.Tool.ToolID
		}
	}
	failed := make(map[string]bool)
	for _, entry := range entries {
		switch entry.Kind {
		case domain.LoopEntryModel:
			if entry.Model == nil {
				continue
			}
			if len(entry.Model.ToolCalls) == 0 {
				replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryState, Name: "answer", Payload: entry.Model.Output, Offset: entry.Offset, Usage: entry.Usage})
			}
			for i, call := range entry.Model.ToolCalls {
				name := toolIDs[call.CallID]
				if name == "" {
					name = call.Name
				}
				usage := entry.Usage
				if i > 0 {
					usage = domain.Usage{}
				}
				replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryToolCall, Name: name, Payload: call.Arguments, Offset: entry.Offset, Usage: usage})
			}
		case domain.LoopEntryApprovalPending:
			replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryPolicy, Name: entry.Tool.ToolID, Offset: entry.Offset})
		case domain.LoopEntryObservation:
			if entry.Observation == nil {
				continue
			}
			toolID, observation := entry.Tool.ToolID, entry.Observation
			replay.event(domain.TrajectoryEvent{Kind: domain.TrajectoryObservation, Name: toolID, Payload: observation.Output, Offset: entry.Offset,
				Usage: entry.Usage, Failed: observation.IsError, Recovered: !observation.IsError && failed[toolID]})
			failed[toolID] = observation.IsError
		}
	}
	return nil
}

func (replay *caseReplay) event(event domain.TrajectoryEvent) {
	event.Sequence = int64(len(replay.trajectory) + 1)
	replay.trajectory = append(replay.trajectory, event)
}

func (replay *caseReplay) offset() time.Duration {
	return replay.executor.now().Sub(replay.started)
}

// replayDecoding is the part of a subject's decoding a replay can apply; any
// other key fails the replay rather than being silently ignored.
type replayDecoding struct {
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int64    `json:"max_tokens,omitempty"`
}

func parseReplayDecoding(raw []byte) (replayDecoding, error) {
	var decoding replayDecoding
	if len(bytes.TrimSpace(raw)) == 0 {
		return decoding, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoding); err != nil {
		return replayDecoding{}, fmt.Errorf("%w: decoding: %v", domain.ErrCapabilityUnsupported, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return replayDecoding{}, fmt.Errorf("%w: decoding has trailing JSON", domain.ErrCapabilityUnsupported)
	}
	if decoding.Temperature != nil && (*decoding.Temperature < 0 || *decoding.Temperature > 2) || decoding.MaxTokens < 0 {
		return replayDecoding{}, fmt.Errorf("%w: decoding temperature must be within 0..2 and max tokens non-negative", domain.ErrValidation)
	}
	return decoding, nil
}

// decodedRequests applies the subject's decoding to the release's opening request.
type decodedRequests struct {
	contract.ToolLoopRequestBuilder
	decoding replayDecoding
}

func (builder decodedRequests) Build(ctx context.Context, input domain.StepInput, config domain.ToolLoopConfig) (domain.ModelRequest, error) {
	request, err := builder.ToolLoopRequestBuilder.Build(ctx, input, config)
	if err != nil {
		return domain.ModelRequest{}, err
	}
	if builder.decoding.Temperature != nil {
		request.Temperature = builder.decoding.Temperature
	}
	if builder.decoding.MaxTokens > 0 {
		request.MaxOutputTokens = builder.decoding.MaxTokens
	}
	return request, nil
}

// subjectRouter refuses a route serving another model version than the subject pins.
type subjectRouter struct {
	contract.ModelRouter
	modelVersion string
}

func (router subjectRouter) Select(ctx context.Context, request domain.ModelRequest) (domain.ModelSelection, error) {
	selection, err := router.ModelRouter.Select(ctx, request)
	if err == nil && router.modelVersion != "" && selection.ModelVersion != router.modelVersion {
		return domain.ModelSelection{}, fmt.Errorf("%w: subject pins model version %q but the release routed to %q", domain.ErrConflict, router.modelVersion, selection.ModelVersion)
	}
	return selection, err
}
