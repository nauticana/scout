package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/dataplane"
)

// ReleaseCaseExecutor replays a golden example against a published agent
// version — its compiled graph, prompt, tools and tool loop — with every tool
// call answered by a sandbox, so a replay has no path to a live tool. A scripted
// example replays turn by turn as one conversation.
type ReleaseCaseExecutor struct {
	Graphs   contract.DefinitionResolver
	Requests contract.ToolLoopRequestBuilder
	Router   contract.ModelRouter
	Models   contract.ModelGateway
	Registry contract.ToolRegistry
	// Payloads reads an example's payload, which is the turn input exactly as a conversation
	// would submit it, and its script; both are verified against their digests.
	Payloads contract.ObjectReader
	// Sandboxes returns a fresh sandbox per replay built from the example's script; ScriptSandbox
	// is the production one.
	Sandboxes SandboxFactory
	Limits    domain.ToolLoopLimits
	// ReleaseLimits reads the limits frozen into the replayed release, which only narrow Limits,
	// MaxSteps, and each turn's duration; pass the data plane's PrincipalLimits.
	ReleaseLimits contract.PrincipalLimits
	// Pricer, Evidence and Retrieval are optional as they are in the runtime.
	Pricer    contract.ModelPricer
	Evidence  contract.EvidenceValidator
	Retrieval contract.CaseRetriever
	// Guardrails and GuardrailConfigs apply the release's output guardrails; set both or neither.
	Guardrails       contract.GuardrailEnforcer
	GuardrailConfigs contract.GuardrailConfigRepository
	// MaxSteps bounds each turn's graph walk; zero allows as many steps as the graph has.
	MaxSteps int
	Now      func() time.Time
}

var _ contract.CaseExecutor = (*ReleaseCaseExecutor)(nil)

func (executor *ReleaseCaseExecutor) validate() error {
	switch {
	case executor.Graphs == nil, executor.Requests == nil, executor.Router == nil, executor.Models == nil,
		executor.Registry == nil, executor.Payloads == nil, executor.Sandboxes == nil, executor.ReleaseLimits == nil:
		return fmt.Errorf("release case executor: graphs, request builder, router, model gateway, tool registry, payload reader, sandboxes, and release limits are required")
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
	principal := domain.Principal{Kind: domain.PrincipalAgent, ID: subject.AgentID, TenantID: example.TenantID, Release: subject.AgentVersion}
	limits, err := executor.ReleaseLimits.ExecutionLimits(ctx, principal)
	if err != nil {
		return domain.EvaluationCase{}, fmt.Errorf("read limits of %s@%s: %w", subject.AgentID, subject.AgentVersion, err)
	}
	budget, err := executor.ReleaseLimits.Budget(ctx, principal)
	if err != nil {
		return domain.EvaluationCase{}, fmt.Errorf("read budget of %s@%s: %w", subject.AgentID, subject.AgentVersion, err)
	}
	inputs, script, err := caseInputs(ctx, executor.Payloads, example)
	if err != nil {
		return domain.EvaluationCase{}, err
	}
	var matches []domain.KnowledgeMatch
	switch {
	case len(preserved) > 0 && len(inputs) > 1:
		return domain.EvaluationCase{}, fmt.Errorf("%w: preserved retrieval answers a single turn", domain.ErrCapabilityUnsupported)
	case executor.Retrieval != nil:
		for i := range inputs {
			var found []domain.KnowledgeMatch
			if inputs[i], found, err = executor.Retrieval.Retrieve(ctx, subject, example, inputs[i], preserved); err != nil {
				return domain.EvaluationCase{}, fmt.Errorf("retrieve for example %q: %w", example.ExampleID, err)
			}
			matches = append(matches, found...)
		}
	case len(preserved) > 0:
		return domain.EvaluationCase{}, fmt.Errorf("%w: preserved retrieval needs a case retriever", domain.ErrCapabilityUnsupported)
	}
	graph, err := executor.Graphs.Resolve(ctx, example.TenantID, subject.AgentID, subject.AgentVersion)
	if err != nil {
		return domain.EvaluationCase{}, fmt.Errorf("resolve %s@%s: %w", subject.AgentID, subject.AgentVersion, err)
	}
	sandbox, err := executor.Sandboxes(ctx, example, script)
	if err != nil {
		return domain.EvaluationCase{}, fmt.Errorf("sandbox for example %q: %w", example.ExampleID, err)
	}
	fences := &dataplane.MemoryTurnFences{}
	journal := &dataplane.MemoryLoopJournal{Fences: fences}
	loop, err := dataplane.NewToolLoopExecutor(dataplane.ToolLoopExecutor{
		Requests: decodedRequests{ToolLoopRequestBuilder: executor.Requests, decoding: decoding},
		Router:   subjectRouter{ModelRouter: executor.Router, modelVersion: subject.Versions.Model},
		Models:   executor.Models, Registry: executor.Registry, Tools: sandbox,
		Journal: journal, Pricer: executor.Pricer, Evidence: executor.Evidence,
		Limits: executor.Limits, Now: executor.Now,
		Outcomes: &dataplane.MemoryCallOutcomeStore{Fences: fences, Journal: journal, Now: executor.Now}, Dispatch: sandboxDispatch(executor.Limits),
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
		loop: loop, journal: journal, fences: fences, graph: graph, enforcer: executor.Guardrails, guardrails: guardrails, principal: principal,
		limits: limits, budget: budget, maxSteps: executor.MaxSteps, now: executor.Now,
		conversationID: "evaluation/" + subject.AgentVersion + "/" + example.ExampleID,
	}
	if err := replay.run(ctx, inputs); err != nil {
		return domain.EvaluationCase{}, err
	}
	return replay.evaluationCase(example, matches), nil
}

// sandboxDispatch bounds a sandboxed call by the loop deadline: a scripted tool answers at once.
func sandboxDispatch(limits domain.ToolLoopLimits) domain.ToolDispatchWindow {
	return domain.ToolDispatchWindow{Timeout: limits.Deadline, Attempts: 1}
}

// caseInputs reads the example's payload and script, each checked against its digest, and
// returns every turn's input in order: the payload, then the script's follow-up turns.
func caseInputs(ctx context.Context, reader contract.ObjectReader, example domain.GoldenExample) ([][]byte, domain.CaseScript, error) {
	input, err := reader.Read(ctx, example.Payload)
	if err != nil {
		return nil, domain.CaseScript{}, fmt.Errorf("read payload of example %q: %w", example.ExampleID, err)
	}
	if digest := sha256Hex(input); digest != strings.ToLower(example.Payload.Digest) {
		return nil, domain.CaseScript{}, fmt.Errorf("%w: payload of example %q has digest %s, not %s", domain.ErrValidation, example.ExampleID, digest, example.Payload.Digest)
	}
	script, err := readScript(ctx, reader, example)
	if err != nil {
		return nil, domain.CaseScript{}, err
	}
	inputs := [][]byte{input}
	for _, turn := range script.Turns {
		inputs = append(inputs, []byte(turn.Input))
	}
	return inputs, script, nil
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
