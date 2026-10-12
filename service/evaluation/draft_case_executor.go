package evaluation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
	"github.com/nauticana/scout/service/dataplane"
	"github.com/nauticana/scout/service/isolation"
	"github.com/nauticana/scout/service/scope"
)

// DraftCaseExecutor replays a scripted example against a frozen draft definition through the
// tool loop a release runs, with no release behind it: the graph is compiled from the
// definition, the request builder reads only that definition, and the loop lists exactly the
// tool versions it binds. Every tool call is answered by the example's sandbox.
type DraftCaseExecutor struct {
	// Compiler compiles the definition's graph; controlplane.ToolLoopGraphCompiler is Scout's.
	Compiler contract.AgentCompiler
	// Requests builds each step's opening request from the definition reader and skill registry
	// it is handed, which serve only the frozen draft: a dataplane.ReleaseLoopRequestBuilder copy
	// with its Definitions and Skills replaced is the usual one.
	Requests func(definitions contract.AgentDefinitionReader, skills contract.SkillRegistry) contract.ToolLoopRequestBuilder
	Router   contract.ModelRouter
	Models   contract.ModelGateway
	// Registry resolves the tool versions the draft binds.
	Registry contract.ToolRegistry
	// Skills resolves the skill versions the draft binds; nil serves none.
	Skills contract.SkillRegistry
	// Payloads reads an example's payload and script; both are verified against their digests.
	Payloads  contract.ObjectReader
	Sandboxes SandboxFactory
	Limits    domain.ToolLoopLimits
	// Pricer and Evidence are optional as they are in the runtime.
	Pricer   contract.ModelPricer
	Evidence contract.EvidenceValidator
	// Guardrails applies the policy frozen into the draft definition.
	Guardrails contract.GuardrailEnforcer
	// MaxSteps bounds each turn's graph walk; zero allows as many steps as the graph has.
	MaxSteps int
	Now      func() time.Time
}

var _ contract.DraftCaseExecutor = (*DraftCaseExecutor)(nil)

// draftVersion is the release a replayed draft runs under; publication numbers versions, so none collides.
func draftVersion(definitionDigest string) string { return "draft:" + definitionDigest }

func (executor *DraftCaseExecutor) validate() error {
	switch {
	case executor.Compiler == nil, executor.Requests == nil, executor.Router == nil, executor.Models == nil,
		executor.Registry == nil, executor.Payloads == nil, executor.Sandboxes == nil, executor.Guardrails == nil:
		return fmt.Errorf("draft case executor: compiler, request builder, router, model gateway, tool registry, payload reader, sandboxes, and guardrails are required")
	case executor.MaxSteps < 0:
		return fmt.Errorf("%w: max steps cannot be negative", domain.ErrValidation)
	}
	return nil
}

// ExecuteDraft replays the example against the frozen draft, bounded by its frozen limits and per-turn budget.
func (executor *DraftCaseExecutor) ExecuteDraft(ctx context.Context, tenantID int64, draft domain.AgentDefinition, settings []domain.EffectiveResource, example domain.GoldenExample) (domain.EvaluationCase, error) {
	if err := executor.validate(); err != nil {
		return domain.EvaluationCase{}, err
	}
	switch {
	case tenantID <= 0 || example.TenantID != tenantID:
		return domain.EvaluationCase{}, fmt.Errorf("%w: example belongs to another tenant", domain.ErrForbidden)
	case strings.TrimSpace(draft.AgentID) == "" || !isSHA256Hex(draft.DefinitionDigest):
		return domain.EvaluationCase{}, fmt.Errorf("%w: a frozen draft names its agent and definition digest", domain.ErrValidation)
	case draft.ToolLoop == nil:
		return domain.EvaluationCase{}, fmt.Errorf("%w: draft of %q declares no tool loop to replay", domain.ErrCapabilityUnsupported, draft.AgentID)
	case draft.SettingsDigest == "" || scope.SettingsDigest(settings) != draft.SettingsDigest:
		return domain.EvaluationCase{}, fmt.Errorf("%w: frozen draft settings do not match the definition", domain.ErrConflict)
	}
	limits, err := isolation.FrozenExecutionLimits(settings)
	if err != nil {
		return domain.EvaluationCase{}, err
	}
	budget, err := frozenBudget(settings)
	if err != nil {
		return domain.EvaluationCase{}, err
	}
	guardrails, err := controlplane.FrozenGuardrailConfig(draft)
	if err != nil {
		return domain.EvaluationCase{}, err
	}
	inputs, script, err := caseInputs(ctx, executor.Payloads, example)
	if err != nil {
		return domain.EvaluationCase{}, err
	}
	frozen := draft
	frozen.Version = draftVersion(draft.DefinitionDigest)
	graph, err := executor.Compiler.Compile(ctx, frozen)
	if err != nil {
		return domain.EvaluationCase{}, fmt.Errorf("compile draft of %q: %w", draft.AgentID, err)
	}
	// A compiled graph is unpersisted, so its steps are numbered for the replay's journal keys.
	for i := range graph.Steps {
		if graph.Steps[i].ExecutionStepID == 0 {
			graph.Steps[i].ExecutionStepID = int64(i + 1)
		}
	}
	sandbox, err := executor.Sandboxes(ctx, example, script)
	if err != nil {
		return domain.EvaluationCase{}, fmt.Errorf("sandbox for example %q: %w", example.ExampleID, err)
	}
	var skills contract.SkillRegistry
	if executor.Skills != nil {
		skills = frozenSkills{SkillRegistry: executor.Skills, draft: frozen}
	}
	fences := &dataplane.MemoryTurnFences{}
	journal := &dataplane.MemoryLoopJournal{Fences: fences}
	loop, err := dataplane.NewToolLoopExecutor(dataplane.ToolLoopExecutor{
		Requests: executor.Requests(frozenDefinition{draft: frozen}, skills),
		Router:   executor.Router, Models: executor.Models, Registry: frozenTools{ToolRegistry: executor.Registry, draft: frozen},
		Tools: sandbox, Journal: journal, Pricer: executor.Pricer, Evidence: executor.Evidence, Limits: executor.Limits, Now: executor.Now,
		Outcomes: &dataplane.MemoryCallOutcomeStore{Fences: fences, Journal: journal, Now: executor.Now}, Dispatch: sandboxDispatch(executor.Limits),
	})
	if err != nil {
		return domain.EvaluationCase{}, err
	}
	replay := &caseReplay{
		loop: loop, journal: journal, fences: fences, graph: graph, enforcer: executor.Guardrails, guardrails: guardrails,
		limits: limits, budget: budget, maxSteps: executor.MaxSteps, now: executor.Now,
		principal:      domain.Principal{Kind: domain.PrincipalAgent, ID: draft.AgentID, TenantID: tenantID, Release: frozen.Version},
		conversationID: "evaluation/" + frozen.Version + "/" + example.ExampleID,
	}
	if err := replay.run(ctx, inputs); err != nil {
		return domain.EvaluationCase{}, err
	}
	return replay.evaluationCase(example, nil), nil
}

// frozenBudget reads the budget binding of frozen settings; settings binding none bound nothing.
func frozenBudget(settings []domain.EffectiveResource) (domain.ReleaseBudget, error) {
	for _, resource := range settings {
		if resource.ResourceKind == domain.ResourceBudget {
			budget, err := scope.DecodeBudget(resource.Value)
			if err != nil {
				return domain.ReleaseBudget{}, fmt.Errorf("frozen draft budget is malformed: %w", err)
			}
			return budget, nil
		}
	}
	return domain.ReleaseBudget{}, nil
}

// frozenDefinition serves the frozen draft under its replay version and nothing else.
type frozenDefinition struct {
	draft domain.AgentDefinition
}

var _ contract.AgentDefinitionReader = frozenDefinition{}

func (reader frozenDefinition) Get(_ context.Context, _ int64, agentID, version string) (domain.AgentDefinition, error) {
	if agentID != reader.draft.AgentID || version != reader.draft.Version {
		return domain.AgentDefinition{}, fmt.Errorf("%w: definition %s@%s", domain.ErrNotFound, agentID, version)
	}
	return reader.draft, nil
}

// frozenTools lists exactly the tool versions the frozen draft binds, each read from the registry.
type frozenTools struct {
	contract.ToolRegistry
	draft domain.AgentDefinition
}

func (tools frozenTools) List(ctx context.Context, tenantID int64, agentID, agentVersion string) ([]domain.ToolDefinition, error) {
	if agentID != tools.draft.AgentID || agentVersion != tools.draft.Version {
		return nil, fmt.Errorf("%w: tools of %s@%s", domain.ErrNotFound, agentID, agentVersion)
	}
	bound := make([]domain.ToolDefinition, 0, len(tools.draft.Tools))
	for _, reference := range tools.draft.Tools {
		definition, err := tools.ToolRegistry.Get(ctx, tenantID, reference.ToolID, reference.Version)
		if err != nil {
			return nil, fmt.Errorf("read tool %s@%s: %w", reference.ToolID, reference.Version, err)
		}
		bound = append(bound, definition)
	}
	return bound, nil
}

func (frozenTools) Register(context.Context, int64, domain.ToolDefinition) error {
	return fmt.Errorf("%w: a draft replay registers no tool", domain.ErrForbidden)
}

// frozenSkills lists exactly the skill versions the frozen draft binds, each read from the registry.
type frozenSkills struct {
	contract.SkillRegistry
	draft domain.AgentDefinition
}

func (skills frozenSkills) List(ctx context.Context, tenantID int64, agentID, agentVersion string) ([]domain.SkillDefinition, error) {
	if agentID != skills.draft.AgentID || agentVersion != skills.draft.Version {
		return nil, fmt.Errorf("%w: skills of %s@%s", domain.ErrNotFound, agentID, agentVersion)
	}
	bound := make([]domain.SkillDefinition, 0, len(skills.draft.Skills))
	for _, reference := range skills.draft.Skills {
		definition, err := skills.SkillRegistry.Get(ctx, tenantID, reference.SkillID, reference.Version)
		if err != nil {
			return nil, fmt.Errorf("read skill %s@%s: %w", reference.SkillID, reference.Version, err)
		}
		bound = append(bound, definition)
	}
	return bound, nil
}

func (frozenSkills) Register(context.Context, int64, domain.SkillDefinition) error {
	return fmt.Errorf("%w: a draft replay registers no skill", domain.ErrForbidden)
}
