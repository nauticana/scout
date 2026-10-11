package charter

import (
	"context"
	"time"

	charterkeel "github.com/nauticana/charter/sdk/adapter/keel"
	"github.com/nauticana/charter/sdk/agent"
	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/charter/sdk/process"

	"github.com/nauticana/scout/domain"
)

// Work is one unit a runtime is asked to perform: a task instance under exactly one assignment.
type Work struct {
	Namespace    string
	Identity     model.Ref
	Runtime      model.Ref
	TaskInstance model.Ref
	Capability   *model.Ref
	// Autonomy is the operating mode frozen into the agent's release; work that needs a
	// higher participation level than the mode reaches is refused. Empty skips the check.
	Autonomy domain.AutonomyMode
	At       time.Time
}

// participation maps Scout's ordered operating modes onto Charter's participation levels (CHR-PROC-008).
var participation = map[domain.AutonomyMode]string{
	domain.AutonomyHumanOnly:           model.ParticipationObserves,
	domain.AutonomyAdvise:              model.ParticipationRecommends,
	domain.AutonomyDraft:               model.ParticipationPrepares,
	domain.AutonomyExecuteWithApproval: model.ParticipationPerforms,
	domain.AutonomyBounded:             model.ParticipationExecutes,
}

// Participation is the Charter participation level an operating mode reaches.
func Participation(mode domain.AutonomyMode) (string, bool) {
	level, ok := participation[mode]
	return level, ok
}

// Admission resolves the execution context for a piece of work and admits it through Charter. The execution
// context id is keel's request correlation id, so admission fails closed outside a correlated request.
type Admission struct {
	Contexts process.ContextResolver
	Agents   agent.Provider
	Base     agent.Admission
}

func (a *Admission) Admit(ctx context.Context, work Work) (agent.ExecutionContext, agent.Decision) {
	if a.Contexts == nil || a.Agents == nil || a.Base == nil {
		return agent.ExecutionContext{}, agent.Decision{Result: agent.Error, Reason: "admission is not fully composed", Requirement: "CHR-AUTH-010"}
	}
	task, err := a.Contexts.TaskContext(ctx, work.Namespace, work.TaskInstance)
	if err != nil {
		return agent.ExecutionContext{}, agent.Decision{Result: agent.Error, Reason: err.Error(), Requirement: "CHR-PROC-005"}
	}
	runtime, err := a.Agents.Runtime(ctx, work.Namespace, work.Runtime)
	if err != nil {
		return agent.ExecutionContext{}, agent.Decision{Result: agent.Error, Reason: err.Error(), Requirement: "CHR-AGENT-006"}
	}
	rc, err := charterkeel.RuntimeContext(ctx, work.Runtime)
	if err != nil {
		return agent.ExecutionContext{}, agent.Decision{Result: agent.Error, Reason: err.Error(), Requirement: "CHR-AGENT-006"}
	}
	if work.Autonomy != "" {
		level, known := Participation(work.Autonomy)
		if !known || process.Exceeds(task.Instance.Participation, level) {
			return agent.ExecutionContext{}, agent.Decision{Result: agent.Refused, Requirement: "CHR-PROC-008",
				Reason: "the task needs participation " + task.Instance.Participation + ", above what operating mode " + string(work.Autonomy) + " reaches"}
		}
	}
	ec := agent.ExecutionContext{
		Namespace:          work.Namespace,
		Identity:           work.Identity,
		Runtime:            work.Runtime,
		ExecutionContextID: rc.ExecutionContextID,
		Definition:         runtime.AgentDefinitionID,
		DefinitionVersion:  runtime.DefinitionVersion,
		Assignment:         task.Instance.AssignmentID,
		Participation:      task.Instance.Participation,
		Capability:         work.Capability,
		At:                 work.At,
	}
	return ec, a.Base.Admit(ctx, ec)
}
