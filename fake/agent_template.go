package fake

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// AgentTemplateStore keeps templates in memory with the table store's visibility, idempotency, and
// limits. Version digests are taken as the caller computed them.
type AgentTemplateStore struct {
	// Tenants lists the tenants a publication may name; nil admits every tenant.
	Tenants map[int64]bool

	mu        sync.Mutex
	sequence  int
	templates map[string]*memoryTemplate
	platform  map[string]*memoryPlatform
}

type memoryTemplate struct {
	owner    int64
	typeID   string
	key      string
	creator  domain.PrincipalRef
	versions []domain.AgentTemplateVersion
	all      bool
	tenants  map[int64]bool
}

type memoryPlatform struct {
	typeID   string
	versions []domain.AgentTemplateVersion
}

var _ contract.AgentTemplateStore = (*AgentTemplateStore)(nil)

func (s *AgentTemplateStore) ensure() {
	if s.templates == nil {
		s.templates, s.platform = map[string]*memoryTemplate{}, map[string]*memoryPlatform{}
	}
}

func (t *memoryTemplate) usableBy(tenantID int64) bool {
	return t.owner == tenantID || t.all || t.tenants[tenantID]
}

func (t *memoryTemplate) view(version domain.AgentTemplateVersion, tenantID int64) domain.AgentTemplateVersion {
	version.OwnerTenantID = t.owner
	if t.owner == tenantID {
		version.Owner = domain.TemplateOwn
		return version
	}
	version.Owner, version.SourceAgentID, version.SourceDraftRevision = domain.TemplateShared, "", 0
	return version
}

func (t *memoryTemplate) audience() *domain.TemplateAudience {
	audience := &domain.TemplateAudience{All: t.all}
	for tenant := range t.tenants {
		audience.Tenants = append(audience.Tenants, tenant)
	}
	slices.Sort(audience.Tenants)
	return audience
}

// Usable returns the newest version of each template of the type the tenant may use.
func (s *AgentTemplateStore) Usable(_ context.Context, tenantID int64, agentTypeID string) ([]domain.AgentTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure()
	var templates []domain.AgentTemplate
	for _, id := range sortedKeys(s.templates) {
		template := s.templates[id]
		if template.typeID != agentTypeID || !template.usableBy(tenantID) {
			continue
		}
		item := domain.AgentTemplate{Latest: template.view(template.versions[len(template.versions)-1], tenantID)}
		if template.owner == tenantID {
			item.Audience = template.audience()
		}
		templates = append(templates, item)
	}
	for _, id := range sortedKeys(s.platform) {
		if entry := s.platform[id]; entry.typeID == agentTypeID {
			templates = append(templates, domain.AgentTemplate{Latest: entry.versions[len(entry.versions)-1]})
		}
	}
	return templates, nil
}

// Version returns the given or newest version of a template the tenant may use.
func (s *AgentTemplateStore) Version(_ context.Context, tenantID int64, templateID string, version int64) (domain.AgentTemplateVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure()
	if strings.HasPrefix(templateID, "tpl-") {
		template, ok := s.templates[templateID]
		if !ok || !template.usableBy(tenantID) {
			return domain.AgentTemplateVersion{}, domain.ErrNotFound
		}
		found, ok := pick(template.versions, version)
		if !ok {
			return domain.AgentTemplateVersion{}, domain.ErrNotFound
		}
		return template.view(found, tenantID), nil
	}
	entry, ok := s.platform[templateID]
	if !ok {
		return domain.AgentTemplateVersion{}, domain.ErrNotFound
	}
	found, ok := pick(entry.versions, version)
	if !ok {
		return domain.AgentTemplateVersion{}, domain.ErrNotFound
	}
	return found, nil
}

// Own returns a template the tenant owns.
func (s *AgentTemplateStore) Own(_ context.Context, tenantID int64, templateID string) (domain.AgentTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure()
	return s.own(tenantID, templateID)
}

func (s *AgentTemplateStore) own(tenantID int64, templateID string) (domain.AgentTemplate, error) {
	template, ok := s.templates[templateID]
	if !ok || template.owner != tenantID {
		return domain.AgentTemplate{}, domain.ErrNotFound
	}
	return domain.AgentTemplate{Latest: template.view(template.versions[len(template.versions)-1], tenantID), Audience: template.audience()}, nil
}

// Save stores a new template or version as the table store does.
func (s *AgentTemplateStore) Save(_ context.Context, actor domain.StudioActor, version domain.AgentTemplateVersion, createKey string, limits domain.TemplateLimits) (domain.AgentTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure()
	templateID := version.TemplateID
	if templateID == "" {
		for id, template := range s.templates {
			if template.owner == actor.TenantID && template.creator == actor.Principal && template.key == createKey {
				first := template.versions[0]
				if template.typeID != version.AgentTypeID || first.Digest != version.Digest || first.DisplayName != version.DisplayName || first.Description != version.Description || first.TypeVersion != version.TypeVersion {
					return domain.AgentTemplate{}, fmt.Errorf("%w: idempotency key %q was used for a different template", domain.ErrConflict, createKey)
				}
				return s.own(actor.TenantID, id)
			}
		}
		held := 0
		for _, template := range s.templates {
			if template.owner == actor.TenantID && template.typeID == version.AgentTypeID {
				held++
			}
		}
		if held >= limits.MaxPerType {
			return domain.AgentTemplate{}, fmt.Errorf("%w: template limit of type %q reached", domain.ErrConflict, version.AgentTypeID)
		}
		if limits.MaxVersions < 1 {
			return domain.AgentTemplate{}, fmt.Errorf("%w: template version limit reached", domain.ErrConflict)
		}
		s.sequence++
		templateID = fmt.Sprintf("tpl-%020x", s.sequence)
		s.templates[templateID] = &memoryTemplate{owner: actor.TenantID, typeID: version.AgentTypeID, key: createKey, creator: actor.Principal, tenants: map[int64]bool{}}
	}
	template, ok := s.templates[templateID]
	if !ok || template.owner != actor.TenantID || template.typeID != version.AgentTypeID {
		return domain.AgentTemplate{}, domain.ErrNotFound
	}
	if count := len(template.versions); count > 0 {
		newest := template.versions[count-1]
		if newest.Digest == version.Digest && newest.DisplayName == version.DisplayName && newest.Description == version.Description && newest.TypeVersion == version.TypeVersion {
			return s.own(actor.TenantID, templateID)
		}
		if count+1 > limits.MaxVersions {
			return domain.AgentTemplate{}, fmt.Errorf("%w: template version limit reached", domain.ErrConflict)
		}
	}
	version.TemplateID, version.Version, version.Owner, version.OwnerTenantID = templateID, int64(len(template.versions)+1), domain.TemplateOwn, actor.TenantID
	template.versions = append(template.versions, version)
	return s.own(actor.TenantID, templateID)
}

// Publish opens or withdraws an owned template's publications.
func (s *AgentTemplateStore) Publish(_ context.Context, actor domain.StudioActor, publication domain.AgentTemplatePublication) (domain.AgentTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure()
	template, ok := s.templates[publication.TemplateID]
	if !ok || template.owner != actor.TenantID {
		return domain.AgentTemplate{}, domain.ErrNotFound
	}
	if publication.Audience.All {
		template.all = !publication.Withdraw
		return s.own(actor.TenantID, publication.TemplateID)
	}
	for _, tenant := range publication.Audience.Tenants {
		if !publication.Withdraw && s.Tenants != nil && !s.Tenants[tenant] {
			return domain.AgentTemplate{}, fmt.Errorf("%w: tenant %d does not exist", domain.ErrValidation, tenant)
		}
	}
	for _, tenant := range publication.Audience.Tenants {
		if publication.Withdraw {
			delete(template.tenants, tenant)
		} else {
			template.tenants[tenant] = true
		}
	}
	return s.own(actor.TenantID, publication.TemplateID)
}

// RegisterPlatform stores the next immutable platform version.
func (s *AgentTemplateStore) RegisterPlatform(_ context.Context, version domain.AgentTemplateVersion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure()
	entry, ok := s.platform[version.TemplateID]
	if !ok {
		entry = &memoryPlatform{typeID: version.AgentTypeID}
		s.platform[version.TemplateID] = entry
	}
	if entry.typeID != version.AgentTypeID {
		return fmt.Errorf("%w: platform template %s belongs to another type", domain.ErrConflict, version.TemplateID)
	}
	if existing, found := pick(entry.versions, version.Version); found && version.Version > 0 {
		if existing.Digest != version.Digest || existing.DisplayName != version.DisplayName || existing.Description != version.Description ||
			existing.TypeVersion != version.TypeVersion || existing.TypeDigest != version.TypeDigest || existing.ChangeSummary != version.ChangeSummary {
			return fmt.Errorf("%w: platform template %s version %d is registered with other content", domain.ErrConflict, version.TemplateID, version.Version)
		}
		return nil
	}
	if version.Version != int64(len(entry.versions)+1) {
		return fmt.Errorf("%w: platform template %s is at version %d", domain.ErrConflict, version.TemplateID, len(entry.versions))
	}
	version.Owner, version.OwnerTenantID = domain.TemplatePlatform, 0
	entry.versions = append(entry.versions, version)
	return nil
}

func pick(versions []domain.AgentTemplateVersion, version int64) (domain.AgentTemplateVersion, bool) {
	if len(versions) == 0 {
		return domain.AgentTemplateVersion{}, false
	}
	if version == 0 {
		return versions[len(versions)-1], true
	}
	for _, candidate := range versions {
		if candidate.Version == version {
			return candidate, true
		}
	}
	return domain.AgentTemplateVersion{}, false
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
