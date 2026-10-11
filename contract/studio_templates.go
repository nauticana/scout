package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// AgentTemplateStore keeps platform and tenant agent templates and who may use them. A template a
// tenant may not use answers domain.ErrNotFound, exactly like a missing one.
type AgentTemplateStore interface {
	// Usable returns the newest version of every template of the type the tenant may use: its own,
	// those shared with it, and the platform's.
	Usable(ctx context.Context, tenantID int64, agentTypeID string) ([]domain.AgentTemplate, error)
	// Version returns the given version, or with 0 the newest, of a template the tenant may use.
	Version(ctx context.Context, tenantID int64, templateID string, version int64) (domain.AgentTemplateVersion, error)
	// Own returns a template the tenant owns with its audience.
	Own(ctx context.Context, tenantID int64, templateID string) (domain.AgentTemplate, error)
	// Save stores a new template, when version.TemplateID is empty, or a new version of one the
	// actor's tenant owns; a save equal to the newest version returns it unchanged.
	Save(ctx context.Context, actor domain.StudioActor, version domain.AgentTemplateVersion, createKey string, limits domain.TemplateLimits) (domain.AgentTemplate, error)
	// Publish shares an owned template with an audience or withdraws it.
	Publish(ctx context.Context, actor domain.StudioActor, publication domain.AgentTemplatePublication) (domain.AgentTemplate, error)
	// RegisterPlatform stores an immutable platform version: equal content is a no-op, different content ErrConflict.
	RegisterPlatform(ctx context.Context, version domain.AgentTemplateVersion) error
}

// StudioTemplatesBackend lists, saves, and shares templates and reports what an agent derives from.
type StudioTemplatesBackend interface {
	ListTemplates(ctx context.Context, actor domain.StudioActor, agentTypeID string) ([]domain.AgentTemplate, error)
	SaveTemplate(ctx context.Context, actor domain.StudioActor, request domain.AgentTemplateSaveRequest) (domain.AgentTemplate, error)
	PublishTemplate(ctx context.Context, actor domain.StudioActor, publication domain.AgentTemplatePublication) (domain.AgentTemplate, error)
	TemplateStatus(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentTemplateStatus, error)
}
