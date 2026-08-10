package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// AgentStudioHTTPBackend is the single service dependency of authenticated Studio handlers.
type AgentStudioHTTPBackend interface {
	ListAgents(ctx context.Context, tenantID int64) ([]domain.AgentSummary, error)
	GetDraft(ctx context.Context, tenantID int64, agentID string) (domain.AgentDraft, error)
	SaveDraft(ctx context.Context, actor domain.StudioActor, draft domain.AgentDraft) (domain.AgentDraft, error)
	SetEnabled(ctx context.Context, actor domain.StudioActor, request domain.AgentSetEnabledRequest) (domain.AgentEnabledState, error)
	TestDraft(ctx context.Context, actor domain.StudioActor, request domain.AgentTestRequest) (domain.AgentTestResult, error)
	Publish(ctx context.Context, actor domain.StudioActor, request domain.AgentPublishRequest) (domain.AgentRelease, error)
	Restore(ctx context.Context, actor domain.StudioActor, request domain.AgentRestoreRequest) (domain.AgentRelease, error)
	Reset(ctx context.Context, actor domain.StudioActor, request domain.AgentResetRequest) (domain.AgentDraft, error)
	SetDefault(ctx context.Context, actor domain.StudioActor, request domain.AgentSetDefaultRequest) ([]domain.AgentSummary, error)
	History(ctx context.Context, tenantID int64, agentID string) ([]domain.AgentRelease, error)
	AuditLog(ctx context.Context, tenantID int64, agentID string) ([]domain.AgentStudioEvent, error)
	ReleaseSections(ctx context.Context, tenantID int64, agentID, version string) ([]domain.AgentReleaseSection, error)
	Models(ctx context.Context, tenantID int64) ([]domain.StudioModel, error)
}
