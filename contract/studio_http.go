package contract

import (
	"context"
	"time"

	"github.com/nauticana/scout/domain"
)

// StudioAuthorizer authorizes an actor's AGENT_STUDIO action on one agent; every Studio
// feature service shares it, so one grant model covers them all.
type StudioAuthorizer interface {
	Authorize(ctx context.Context, actor domain.StudioActor, action domain.StudioAction, agentID string) error
}

// StudioDecisionsBackend pages the actor's tenant's decision records, newest first. It
// authorizes AGENT_AUDIT VIEW at the tenant and binds the query to the actor's tenant.
type StudioDecisionsBackend interface {
	Decisions(ctx context.Context, actor domain.StudioActor, query domain.DecisionQuery) (domain.DecisionPage, error)
}

// StudioTrafficBackend moves an agent's traffic between its releases and explains which
// release a conversation runs on.
type StudioTrafficBackend interface {
	Canary(ctx context.Context, actor domain.StudioActor, agentID, version string, percentage int) (domain.AgentDeployment, error)
	Promote(ctx context.Context, actor domain.StudioActor, agentID, version string) (domain.AgentDeployment, error)
	Rollback(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentDeployment, error)
	Explain(ctx context.Context, actor domain.StudioActor, conversationID string) (domain.ConversationVersion, error)
}

// StudioDelegationBackend shows and changes which agents may hand work to which.
type StudioDelegationBackend interface {
	List(ctx context.Context, actor domain.StudioActor, agentID string) ([]domain.DelegationGrant, error)
	Grant(ctx context.Context, actor domain.StudioActor, grant domain.DelegationGrant) (domain.DelegationGrant, error)
	Revoke(ctx context.Context, actor domain.StudioActor, agentID, grantID string) error
}

// StudioUsageBackend reports an agent's settled usage over a bounded window.
type StudioUsageBackend interface {
	Usage(ctx context.Context, actor domain.StudioActor, agentID string, from, to time.Time) ([]domain.UsageLine, error)
}

// StudioPauseBackend stops an agent's admission and applies a policy to its live turns.
type StudioPauseBackend interface {
	Pause(ctx context.Context, actor domain.StudioActor, request domain.AgentPauseRequest) (domain.AgentPauseResult, error)
}

// StudioTurnControlBackend lets an operator see an agent's live turns, hold and release them,
// take them over, act in them, and hand them back or finish them, and settle their unknown call
// outcomes. Reading needs AGENT_OPERATION VIEW and changing OPERATE on the agent, a preempting
// hold PREEMPT too, and an attestation RECONCILE; an agent the actor cannot view in Studio, and a
// turn of another agent, read as not found. A stale fence or a lapsed lease is domain.ErrConflict
// or domain.ErrFenceLost.
type StudioTurnControlBackend interface {
	// TakeOver takes a turn held at the fence the actor saw, with no open call outcome.
	TakeOver(ctx context.Context, actor domain.StudioActor, request domain.TurnTakeoverRequest) (domain.LiveTurn, error)
	// Act appends one entry to a turn the actor operates and renews their lease.
	Act(ctx context.Context, actor domain.StudioActor, request domain.TurnActRequest) (domain.TurnActResult, error)
	// HandBack re-admits the turn's budget, then returns it to the agent with an optional instruction.
	HandBack(ctx context.Context, actor domain.StudioActor, request domain.TurnHandbackRequest) (domain.LiveTurn, error)
	// Finish completes a turn the actor operates with their guarded reply.
	Finish(ctx context.Context, actor domain.StudioActor, request domain.TurnFinishRequest) (domain.LiveTurn, error)
	LiveTurns(ctx context.Context, actor domain.StudioActor, filter domain.LiveTurnFilter) (domain.LiveTurnPage, error)
	// OpenOutcomes pages the tenant's open calls across turns, oldest deadline first; without an
	// agent it needs AGENT_OPERATION VIEW granted on every agent.
	OpenOutcomes(ctx context.Context, actor domain.StudioActor, filter domain.OpenOutcomeFilter) (domain.OpenOutcomePage, error)
	// Turn reads one turn with its call outcomes.
	Turn(ctx context.Context, actor domain.StudioActor, agentID, requestID string) (domain.LiveTurn, error)
	Hold(ctx context.Context, actor domain.StudioActor, request domain.TurnHoldRequest) (domain.TurnHoldOutcome, error)
	// Release re-admits the held turn's budget, then requeues it; a stale fence, or a call of the
	// turn whose outcome is open, is domain.ErrConflict.
	Release(ctx context.Context, actor domain.StudioActor, request domain.TurnReleaseRequest) (domain.LiveTurn, error)
	// LookupOutcome asks the reconciliation sources about one open call of a held or ended turn
	// and settles it when they answer; an answer that is still unknown leaves it open.
	LookupOutcome(ctx context.Context, actor domain.StudioActor, request domain.TurnOutcomeRequest) (domain.CallOutcome, error)
	// AttestOutcome records the actor's attestation of one open call after its deadline.
	AttestOutcome(ctx context.Context, actor domain.StudioActor, request domain.TurnAttestRequest) (domain.AttestationResult, error)
}

// StudioFeedbackBackend reports the ratings an agent's releases received over a bounded window.
type StudioFeedbackBackend interface {
	Feedback(ctx context.Context, actor domain.StudioActor, agentID string, from, to time.Time) ([]domain.FeedbackSummary, error)
}

// StudioRunsBackend pages an agent's settled runs, newest first, and traces one of them.
type StudioRunsBackend interface {
	Runs(ctx context.Context, actor domain.StudioActor, filter domain.AgentRunFilter) ([]domain.AgentRun, error)
	Trace(ctx context.Context, actor domain.StudioActor, agentID string, runID int64) (domain.RunTrace, error)
}

// AgentStudioHTTPBackend is the single service dependency of authenticated Studio
// handlers. Every method authorizes the actor itself, so a product service calling
// it directly is held to the same AGENT_STUDIO grants as a person.
type AgentStudioHTTPBackend interface {
	ListAgents(ctx context.Context, actor domain.StudioActor) ([]domain.AgentSummary, error)
	Create(ctx context.Context, actor domain.StudioActor, request domain.AgentCreateRequest) (domain.AgentSummary, error)
	GetDraft(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentDraft, error)
	SaveDraft(ctx context.Context, actor domain.StudioActor, draft domain.AgentDraft) (domain.AgentDraft, error)
	Settings(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentSettings, error)
	SaveSetting(ctx context.Context, actor domain.StudioActor, change domain.AgentSettingChange) (domain.AgentSettings, error)
	SetEnabled(ctx context.Context, actor domain.StudioActor, request domain.AgentSetEnabledRequest) (domain.AgentEnabledState, error)
	Rename(ctx context.Context, actor domain.StudioActor, request domain.AgentRenameRequest) (domain.AgentSummary, error)
	Archive(ctx context.Context, actor domain.StudioActor, request domain.AgentArchiveRequest) (domain.AgentSummary, error)
	Unarchive(ctx context.Context, actor domain.StudioActor, request domain.AgentArchiveRequest) (domain.AgentSummary, error)
	TestDraft(ctx context.Context, actor domain.StudioActor, request domain.AgentTestRequest) (domain.AgentTestResult, error)
	Publish(ctx context.Context, actor domain.StudioActor, request domain.AgentPublishRequest) (domain.AgentRelease, error)
	Restore(ctx context.Context, actor domain.StudioActor, request domain.AgentRestoreRequest) (domain.AgentRelease, error)
	Reset(ctx context.Context, actor domain.StudioActor, request domain.AgentResetRequest) (domain.AgentDraft, error)
	SetDefault(ctx context.Context, actor domain.StudioActor, request domain.AgentSetDefaultRequest) ([]domain.AgentSummary, error)
	History(ctx context.Context, actor domain.StudioActor, agentID string) ([]domain.AgentRelease, error)
	AuditLog(ctx context.Context, actor domain.StudioActor, agentID string) ([]domain.AgentStudioEvent, error)
	ReleaseSections(ctx context.Context, actor domain.StudioActor, agentID, version string) ([]domain.AgentReleaseSection, error)
	Models(ctx context.Context, actor domain.StudioActor) ([]domain.StudioModel, error)
}
