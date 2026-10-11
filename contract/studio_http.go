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

// StudioFeedbackBackend reports the ratings an agent's releases received over a bounded window.
type StudioFeedbackBackend interface {
	Feedback(ctx context.Context, actor domain.StudioActor, agentID string, from, to time.Time) ([]domain.FeedbackSummary, error)
}

// StudioRunsBackend pages an agent's settled runs, newest first.
type StudioRunsBackend interface {
	Runs(ctx context.Context, actor domain.StudioActor, filter domain.AgentRunFilter) ([]domain.AgentRun, error)
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
