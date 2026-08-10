package contract

import (
	"context"
	"time"

	"github.com/nauticana/scout/domain"
)

// PromptCompiler merges resolved prompt levels and creates canonical digests.
type PromptCompiler interface {
	Compile(languageCode string, rows []domain.PromptSourceRow) (domain.CompiledPrompt, error)
	DefinitionDigest(definition domain.AgentDefinition) (string, error)
}

// PromptDraftAssembler combines resolved provenance with effective prompt content.
type PromptDraftAssembler interface {
	Assemble(resolved domain.ResolvedPrompts) (domain.AgentLanguageDraft, error)
}

// PromptSourceRepository resolves persisted prompt levels for compilation and editing.
type PromptSourceRepository interface {
	Resolve(ctx context.Context, tenantID int64, agentID, languageCode string) (domain.ResolvedPrompts, error)
	Languages(ctx context.Context, tenantID int64, agentID string) ([]string, error)
}

// PromptBaselineSelector returns product-specific baseline keys in precedence order.
type PromptBaselineSelector interface {
	Select(ctx context.Context, tenantID int64, agentID, agentKind string) (domain.PromptBaselineSelection, error)
}

// AgentDraftValidator contributes product-specific validation without owning persistence.
type AgentDraftValidator interface {
	Validate(ctx context.Context, tenantID int64, draft domain.AgentDraft, phase domain.ValidationPhase) ([]domain.AgentFieldError, error)
}

// AgentActivityReporter supplies product-owned last-successful-run times per
// agent id. Studio surfaces them on summaries; execution history stays with
// the product that runs the agents.
type AgentActivityReporter interface {
	LastRun(ctx context.Context, tenantID int64) (map[string]time.Time, error)
}

// AgentDraftTestExecutor runs one saved draft through a downstream governed runtime.
type AgentDraftTestExecutor interface {
	Execute(ctx context.Context, actor domain.StudioActor, request domain.AgentTestRequest, definition domain.AgentDefinition) (domain.AgentTestResult, error)
}

// AgentKindCatalog supplies product-owned labels and purposes for open agent kinds.
type AgentKindCatalog interface {
	Get(ctx context.Context, agentKind string) (domain.AgentKindDescriptor, error)
	List(ctx context.Context) ([]domain.AgentKindDescriptor, error)
}

// StudioModelCatalog lists tenant-selectable models without provider SDK types.
type StudioModelCatalog interface {
	List(ctx context.Context, tenantID int64) ([]domain.StudioModel, error)
	Validate(ctx context.Context, tenantID int64, selection domain.AgentModelSelection) ([]domain.AgentFieldError, error)
}

// PublishedAgentResolver resolves an alias and locale to one immutable definition.
type PublishedAgentResolver interface {
	Resolve(ctx context.Context, tenantID int64, aliasID, languageCode, conversationID string) (domain.AgentDefinition, error)
}

// AgentProvisioner idempotently registers a tenant and seeds its agent
// identities, drafts, and aliases. Which agents a product seeds, and whether
// the resulting set is usable, remain product decisions.
type AgentProvisioner interface {
	Provision(ctx context.Context, tenantID int64, identity domain.TenantIdentity, seeds []domain.AgentSeed) error
}

// DeployedAgentIndex reports operational state and the deployed definition for
// every alias a tenant owns, so products can derive their own readiness views.
type DeployedAgentIndex interface {
	List(ctx context.Context, tenantID int64) (map[string]domain.DeployedAgent, error)
}
