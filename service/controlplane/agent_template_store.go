package controlplane

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qTemplateLockType          = "scout_template_lock_type"
	qTemplateByKey             = "scout_template_by_key"
	qTemplateFirstVersion      = "scout_template_first_version"
	qTemplateCount             = "scout_template_count"
	qTemplateInsert            = "scout_template_insert"
	qTemplateLock              = "scout_template_lock"
	qTemplateNewest            = "scout_template_newest"
	qTemplateInsertVersion     = "scout_template_insert_version"
	qTemplateOwnVersion        = "scout_template_own_version"
	qTemplateAudience          = "scout_template_audience"
	qTemplateOwnList           = "scout_template_own_list"
	qTemplateOwnAudiences      = "scout_template_own_audiences"
	qTemplateSharedList        = "scout_template_shared_list"
	qTemplatePlatformList      = "scout_template_platform_list"
	qTemplateUsableVersion     = "scout_template_usable_version"
	qTemplatePlatformVersion   = "scout_template_platform_version"
	qTemplateTenantsExist      = "scout_template_tenants_exist"
	qTemplatePublishAll        = "scout_template_publish_all"
	qTemplatePublishTenant     = "scout_template_publish_tenant"
	qTemplateWithdrawAll       = "scout_template_withdraw_all"
	qTemplateWithdrawTenant    = "scout_template_withdraw_tenant"
	qTemplateCatalogEnsure     = "scout_template_catalog_ensure"
	qTemplateCatalogLock       = "scout_template_catalog_lock"
	qTemplateCatalogVersion    = "scout_template_catalog_version"
	qTemplateCatalogMax        = "scout_template_catalog_max"
	qTemplateCatalogNewVersion = "scout_template_catalog_new_version"
)

// tenantVersionColumns is the row every tenant template read returns; a shared read selects NULL for
// the owner-only source columns.
const tenantVersionColumns = `v.template_id, v.template_version, v.agent_type_id, v.type_version, tv.definition_digest,
       v.display_name, v.description, v.change_summary, v.payload, v.payload_digest, v.created_at, v.partner_id`

const tenantVersionJoin = `
  FROM agent_template_version v
  LEFT JOIN agent_type_version tv
    ON tv.partner_id = v.partner_id AND tv.agent_type_id = v.agent_type_id AND tv.type_version = v.type_version`

// usableByTenant is the only cross-tenant read: a tenant uses its own templates and those with an open
// publication to it or to every tenant.
const usableByTenant = `EXISTS (SELECT 1 FROM agent_template_publication p
        WHERE p.partner_id = v.partner_id AND p.template_id = v.template_id AND p.endda IS NULL
          AND (p.audience_partner_id = ? OR p.audience_partner_id IS NULL))`

const platformVersionColumns = `c.template_id, v.template_version, c.agent_type_id, v.type_version, v.type_digest,
       v.display_name, v.description, v.change_summary, v.payload, v.payload_digest, v.created_at, 0, NULL, NULL`

var agentTemplateQueries = map[string]string{
	qTemplateLockType: `SELECT agent_type_id FROM agent_type WHERE partner_id = ? AND agent_type_id = ? FOR UPDATE`,
	qTemplateByKey: `
SELECT template_id, agent_type_id FROM agent_template
 WHERE partner_id = ? AND created_by_kind = ? AND created_by_id = ? AND create_key = ?`,
	qTemplateFirstVersion: `
SELECT payload_digest, display_name, description, type_version FROM agent_template_version
 WHERE partner_id = ? AND template_id = ? AND template_version = 1`,
	qTemplateCount: `SELECT COUNT(*) FROM agent_template WHERE partner_id = ? AND agent_type_id = ?`,
	qTemplateInsert: `
INSERT INTO agent_template (partner_id, template_id, agent_type_id, create_key, created_by_kind, created_by_id)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (partner_id, created_by_kind, created_by_id, create_key) DO NOTHING
RETURNING template_id`,
	qTemplateLock: `SELECT agent_type_id FROM agent_template WHERE partner_id = ? AND template_id = ? FOR UPDATE`,
	qTemplateNewest: `
SELECT template_version, payload_digest, display_name, description, type_version FROM agent_template_version
 WHERE partner_id = ? AND template_id = ? ORDER BY template_version DESC LIMIT 1`,
	qTemplateInsertVersion: `
INSERT INTO agent_template_version (partner_id, template_id, template_version, agent_type_id, display_name, description,
       type_version, payload, payload_digest, change_summary, source_agent_id, source_draft_revision,
       created_by_kind, created_by_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qTemplateOwnVersion: `SELECT ` + tenantVersionColumns + `, v.source_agent_id, v.source_draft_revision` + tenantVersionJoin + `
 WHERE v.partner_id = ? AND v.template_id = ?
 ORDER BY v.template_version DESC LIMIT 1`,
	qTemplateAudience: `
SELECT audience_partner_id FROM agent_template_publication
 WHERE partner_id = ? AND template_id = ? AND endda IS NULL ORDER BY audience_partner_id NULLS FIRST`,
	qTemplateOwnList: `SELECT DISTINCT ON (v.template_id) ` + tenantVersionColumns + `, v.source_agent_id, v.source_draft_revision` + tenantVersionJoin + `
 WHERE v.partner_id = ? AND v.agent_type_id = ?
 ORDER BY v.template_id, v.template_version DESC LIMIT ?`,
	qTemplateOwnAudiences: `
SELECT template_id, audience_partner_id FROM agent_template_publication
 WHERE partner_id = ? AND endda IS NULL ORDER BY template_id, audience_partner_id NULLS FIRST`,
	qTemplateSharedList: `SELECT DISTINCT ON (v.partner_id, v.template_id) ` + tenantVersionColumns + `, NULL, NULL` + tenantVersionJoin + `
 WHERE v.agent_type_id = ? AND v.partner_id <> ? AND ` + usableByTenant + `
 ORDER BY v.partner_id, v.template_id, v.template_version DESC LIMIT ?`,
	qTemplatePlatformList: `SELECT DISTINCT ON (c.template_id) ` + platformVersionColumns + `
  FROM agent_template_catalog_version v
  JOIN agent_template_catalog c ON c.template_id = v.template_id
 WHERE c.agent_type_id = ?
 ORDER BY c.template_id, v.template_version DESC LIMIT ?`,
	qTemplateUsableVersion: `SELECT ` + tenantVersionColumns + `, NULL, NULL` + tenantVersionJoin + `
 WHERE v.template_id = ? AND (? = 0 OR v.template_version = ?) AND (v.partner_id = ? OR ` + usableByTenant + `)
 ORDER BY v.template_version DESC LIMIT 1`,
	qTemplatePlatformVersion: `SELECT ` + platformVersionColumns + `
  FROM agent_template_catalog_version v
  JOIN agent_template_catalog c ON c.template_id = v.template_id
 WHERE v.template_id = ? AND (? = 0 OR v.template_version = ?)
 ORDER BY v.template_version DESC LIMIT 1`,
	qTemplateTenantsExist: `SELECT partner_id FROM agent_tenant WHERE partner_id = ANY(CAST(string_to_array(?, ',') AS BIGINT[]))`,
	qTemplatePublishAll: `
INSERT INTO agent_template_publication (id, partner_id, template_id, audience_partner_id, published_by_kind, published_by_id)
SELECT NEXTVAL('agent_template_publication_seq'), ?, ?, NULL, ?, ?
 WHERE NOT EXISTS (SELECT 1 FROM agent_template_publication
                    WHERE partner_id = ? AND template_id = ? AND audience_partner_id IS NULL AND endda IS NULL)`,
	qTemplatePublishTenant: `
INSERT INTO agent_template_publication (id, partner_id, template_id, audience_partner_id, published_by_kind, published_by_id)
SELECT NEXTVAL('agent_template_publication_seq'), ?, ?, ?, ?, ?
 WHERE NOT EXISTS (SELECT 1 FROM agent_template_publication
                    WHERE partner_id = ? AND template_id = ? AND audience_partner_id = ? AND endda IS NULL)`,
	qTemplateWithdrawAll: `
UPDATE agent_template_publication SET endda = CURRENT_TIMESTAMP, withdrawn_by_kind = ?, withdrawn_by_id = ?
 WHERE partner_id = ? AND template_id = ? AND audience_partner_id IS NULL AND endda IS NULL`,
	qTemplateWithdrawTenant: `
UPDATE agent_template_publication SET endda = CURRENT_TIMESTAMP, withdrawn_by_kind = ?, withdrawn_by_id = ?
 WHERE partner_id = ? AND template_id = ? AND audience_partner_id = ? AND endda IS NULL`,
	qTemplateCatalogEnsure: `INSERT INTO agent_template_catalog (template_id, agent_type_id) VALUES (?, ?) ON CONFLICT (template_id) DO NOTHING`,
	qTemplateCatalogLock:   `SELECT agent_type_id FROM agent_template_catalog WHERE template_id = ? FOR UPDATE`,
	qTemplateCatalogVersion: `
SELECT payload_digest, display_name, description, type_version, type_digest, change_summary
  FROM agent_template_catalog_version WHERE template_id = ? AND template_version = ?`,
	qTemplateCatalogMax: `SELECT COALESCE(MAX(template_version), 0) FROM agent_template_catalog_version WHERE template_id = ?`,
	qTemplateCatalogNewVersion: `
INSERT INTO agent_template_catalog_version (template_id, template_version, display_name, description, type_version,
       type_digest, payload, payload_digest, change_summary)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qStudioAudit: studioQueries[qStudioAudit],
}

// TableAgentTemplateStore keeps templates in agent_template* tables: tenant templates with immutable
// versions and publications, and the platform catalog no tenant owns.
type TableAgentTemplateStore struct {
	DB keelport.DatabaseRepository
	// Entropy generates new template ids; nil uses crypto/rand.
	Entropy io.Reader
	// UsableLimit bounds the templates of each owner class Usable returns; zero takes DefaultUsableTemplateLimit.
	UsableLimit int

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.AgentTemplateStore = (*TableAgentTemplateStore)(nil)

func (s *TableAgentTemplateStore) init(ctx context.Context) error {
	if s.DB == nil {
		return fmt.Errorf("%w: template store needs a database", domain.ErrNotReady)
	}
	if s.UsableLimit < 0 {
		return fmt.Errorf("%w: template store usable limit cannot be negative", domain.ErrValidation)
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, agentTemplateQueries) })
	return nil
}

// Usable returns the newest version of the type's templates the tenant may use, at most
// UsableLimit of each owner class; shared and platform rows carry no source or audience.
func (s *TableAgentTemplateStore) Usable(ctx context.Context, tenantID int64, agentTypeID string) ([]domain.AgentTemplate, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	usableTemplateLimit := orDefault(s.UsableLimit, DefaultUsableTemplateLimit)
	own, err := s.qs.Query(ctx, qTemplateOwnList, tenantID, agentTypeID, usableTemplateLimit)
	if err != nil {
		return nil, fmt.Errorf("list own templates: %w", err)
	}
	audiences, err := s.qs.Query(ctx, qTemplateOwnAudiences, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list template audiences: %w", err)
	}
	byTemplate := map[string]*domain.TemplateAudience{}
	for _, row := range audiences.Rows {
		templateID := common.AsString(row[0])
		audience := byTemplate[templateID]
		if audience == nil {
			audience = &domain.TemplateAudience{}
			byTemplate[templateID] = audience
		}
		addAudience(audience, row[1])
	}
	var templates []domain.AgentTemplate
	for _, row := range own.Rows {
		version, err := templateVersionRow(row, tenantID)
		if err != nil {
			return nil, err
		}
		audience := byTemplate[version.TemplateID]
		if audience == nil {
			audience = &domain.TemplateAudience{}
		}
		templates = append(templates, domain.AgentTemplate{Latest: version, Audience: audience})
	}
	shared, err := s.qs.Query(ctx, qTemplateSharedList, agentTypeID, tenantID, tenantID, usableTemplateLimit)
	if err != nil {
		return nil, fmt.Errorf("list shared templates: %w", err)
	}
	platform, err := s.qs.Query(ctx, qTemplatePlatformList, agentTypeID, usableTemplateLimit)
	if err != nil {
		return nil, fmt.Errorf("list platform templates: %w", err)
	}
	for _, rows := range [][][]any{shared.Rows, platform.Rows} {
		for _, row := range rows {
			version, err := templateVersionRow(row, tenantID)
			if err != nil {
				return nil, err
			}
			templates = append(templates, domain.AgentTemplate{Latest: version})
		}
	}
	return templates, nil
}

// Version returns a version the tenant may use; one it may not is ErrNotFound, like a missing one.
func (s *TableAgentTemplateStore) Version(ctx context.Context, tenantID int64, templateID string, version int64) (domain.AgentTemplateVersion, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentTemplateVersion{}, err
	}
	query, args := qTemplatePlatformVersion, []any{templateID, version, version}
	if strings.HasPrefix(templateID, "tpl-") {
		query, args = qTemplateUsableVersion, []any{templateID, version, version, tenantID, tenantID}
	}
	res, err := s.qs.Query(ctx, query, args...)
	if err != nil {
		return domain.AgentTemplateVersion{}, fmt.Errorf("load template %q: %w", templateID, err)
	}
	if len(res.Rows) == 0 {
		return domain.AgentTemplateVersion{}, domain.ErrNotFound
	}
	return templateVersionRow(res.Rows[0], tenantID)
}

// Own returns a template the tenant owns with its newest version and audience.
func (s *TableAgentTemplateStore) Own(ctx context.Context, tenantID int64, templateID string) (domain.AgentTemplate, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentTemplate{}, err
	}
	return s.own(ctx, s.qs, tenantID, templateID)
}

func (s *TableAgentTemplateStore) own(ctx context.Context, qs keelport.QueryService, tenantID int64, templateID string) (domain.AgentTemplate, error) {
	res, err := qs.Query(ctx, qTemplateOwnVersion, tenantID, templateID)
	if err != nil {
		return domain.AgentTemplate{}, fmt.Errorf("load template %q: %w", templateID, err)
	}
	if len(res.Rows) == 0 {
		return domain.AgentTemplate{}, domain.ErrNotFound
	}
	version, err := templateVersionRow(res.Rows[0], tenantID)
	if err != nil {
		return domain.AgentTemplate{}, err
	}
	audiences, err := qs.Query(ctx, qTemplateAudience, tenantID, templateID)
	if err != nil {
		return domain.AgentTemplate{}, fmt.Errorf("load template audience: %w", err)
	}
	audience := &domain.TemplateAudience{}
	for _, row := range audiences.Rows {
		addAudience(audience, row[0])
	}
	return domain.AgentTemplate{Latest: version, Audience: audience}, nil
}

// Save stores a new template or version in one transaction. A new template is idempotent per actor and
// key and bounded per type; a version equal to the newest is not stored again.
func (s *TableAgentTemplateStore) Save(ctx context.Context, actor domain.StudioActor, version domain.AgentTemplateVersion, createKey string, limits domain.TemplateLimits) (domain.AgentTemplate, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentTemplate{}, err
	}
	payload, err := canonicalTemplatePayload(version.Payload)
	if err != nil {
		return domain.AgentTemplate{}, err
	}
	digest := valueDigest(payload)
	tx, err := s.DB.BeginTx(ctx, agentTemplateQueries)
	if err != nil {
		return domain.AgentTemplate{}, fmt.Errorf("begin template transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	templateID := version.TemplateID
	if templateID == "" {
		created, replayed, err := s.createTemplate(ctx, tx, actor, version, createKey, digest, limits)
		if err != nil {
			return domain.AgentTemplate{}, err
		}
		if replayed {
			if err = tx.Commit(ctx); err != nil {
				return domain.AgentTemplate{}, fmt.Errorf("commit template: %w", err)
			}
			committed = true
			return s.Own(ctx, actor.TenantID, created)
		}
		templateID = created
	} else {
		locked, err := tx.Query(ctx, qTemplateLock, actor.TenantID, templateID)
		if err != nil {
			return domain.AgentTemplate{}, fmt.Errorf("lock template: %w", err)
		}
		if len(locked.Rows) == 0 || common.AsString(locked.Rows[0][0]) != version.AgentTypeID {
			return domain.AgentTemplate{}, domain.ErrNotFound
		}
	}
	newest, err := tx.Query(ctx, qTemplateNewest, actor.TenantID, templateID)
	if err != nil {
		return domain.AgentTemplate{}, fmt.Errorf("load newest template version: %w", err)
	}
	next := int64(1)
	if len(newest.Rows) > 0 {
		row := newest.Rows[0]
		if strings.TrimSpace(common.AsString(row[1])) == digest && common.AsString(row[2]) == version.DisplayName &&
			common.AsString(row[3]) == version.Description && common.AsString(row[4]) == version.TypeVersion {
			if err = tx.Commit(ctx); err != nil {
				return domain.AgentTemplate{}, fmt.Errorf("commit template: %w", err)
			}
			committed = true
			return s.Own(ctx, actor.TenantID, templateID)
		}
		next = common.AsInt64(row[0]) + 1
	}
	if next > int64(limits.MaxVersions) {
		return domain.AgentTemplate{}, fmt.Errorf("%w: template %s already has %d versions, the limit is %d", domain.ErrConflict, templateID, next-1, limits.MaxVersions)
	}
	if _, err = tx.Query(ctx, qTemplateInsertVersion, actor.TenantID, templateID, next, version.AgentTypeID, version.DisplayName,
		common.NullIfEmpty(version.Description), common.NullIfEmpty(version.TypeVersion), string(payload), digest, common.NullIfEmpty(version.ChangeSummary),
		common.NullIfEmpty(version.SourceAgentID), sourceRevision(version), string(actor.Principal.Kind), actor.Principal.ID); err != nil {
		return domain.AgentTemplate{}, fmt.Errorf("insert template version: %w", err)
	}
	if version.SourceAgentID != "" {
		if err = auditStudio(ctx, tx, actor, version.SourceAgentID, "TEMPLATE", fmt.Sprintf("template %s version %d", templateID, next)); err != nil {
			return domain.AgentTemplate{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentTemplate{}, fmt.Errorf("commit template: %w", err)
	}
	committed = true
	return s.Own(ctx, actor.TenantID, templateID)
}

// createTemplate makes the template row under the type's lock, or finds the one the actor's key made.
func (s *TableAgentTemplateStore) createTemplate(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, version domain.AgentTemplateVersion, createKey, digest string, limits domain.TemplateLimits) (string, bool, error) {
	locked, err := tx.Query(ctx, qTemplateLockType, actor.TenantID, version.AgentTypeID)
	if err != nil {
		return "", false, fmt.Errorf("lock agent type: %w", err)
	}
	if len(locked.Rows) == 0 {
		return "", false, validationError("agent_type", "unknown agent type")
	}
	prior, err := tx.Query(ctx, qTemplateByKey, actor.TenantID, string(actor.Principal.Kind), actor.Principal.ID, createKey)
	if err != nil {
		return "", false, fmt.Errorf("load template request: %w", err)
	}
	if len(prior.Rows) > 0 {
		templateID := common.AsString(prior.Rows[0][0])
		first, err := tx.Query(ctx, qTemplateFirstVersion, actor.TenantID, templateID)
		if err != nil {
			return "", false, fmt.Errorf("load first template version: %w", err)
		}
		if common.AsString(prior.Rows[0][1]) != version.AgentTypeID || len(first.Rows) == 0 ||
			strings.TrimSpace(common.AsString(first.Rows[0][0])) != digest || common.AsString(first.Rows[0][1]) != version.DisplayName ||
			common.AsString(first.Rows[0][2]) != version.Description || common.AsString(first.Rows[0][3]) != version.TypeVersion {
			return "", false, fmt.Errorf("%w: idempotency key %q was used for a different template", domain.ErrConflict, createKey)
		}
		return templateID, true, nil
	}
	count, err := tx.Query(ctx, qTemplateCount, actor.TenantID, version.AgentTypeID)
	if err != nil {
		return "", false, fmt.Errorf("count templates: %w", err)
	}
	if held := common.AsInt64(count.Rows[0][0]); held >= int64(limits.MaxPerType) {
		return "", false, fmt.Errorf("%w: the tenant already has %d templates of type %q, the limit is %d", domain.ErrConflict, held, version.AgentTypeID, limits.MaxPerType)
	}
	templateID, err := s.newTemplateID()
	if err != nil {
		return "", false, err
	}
	inserted, err := tx.Query(ctx, qTemplateInsert, actor.TenantID, templateID, version.AgentTypeID, createKey, string(actor.Principal.Kind), actor.Principal.ID)
	if err != nil {
		return "", false, fmt.Errorf("insert template: %w", err)
	}
	if len(inserted.Rows) == 0 {
		return "", false, fmt.Errorf("%w: idempotency key %q was used concurrently", domain.ErrConflict, createKey)
	}
	return templateID, false, nil
}

// Publish opens or withdraws publications of an owned template; publishing again is a no-op.
func (s *TableAgentTemplateStore) Publish(ctx context.Context, actor domain.StudioActor, publication domain.AgentTemplatePublication) (domain.AgentTemplate, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentTemplate{}, err
	}
	tx, err := s.DB.BeginTx(ctx, agentTemplateQueries)
	if err != nil {
		return domain.AgentTemplate{}, fmt.Errorf("begin publication transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	locked, err := tx.Query(ctx, qTemplateLock, actor.TenantID, publication.TemplateID)
	if err != nil {
		return domain.AgentTemplate{}, fmt.Errorf("lock template: %w", err)
	}
	if len(locked.Rows) == 0 {
		return domain.AgentTemplate{}, domain.ErrNotFound
	}
	owner, by, id := actor.TenantID, string(actor.Principal.Kind), actor.Principal.ID
	if publication.Audience.All {
		if publication.Withdraw {
			_, err = tx.Query(ctx, qTemplateWithdrawAll, by, id, owner, publication.TemplateID)
		} else {
			_, err = tx.Query(ctx, qTemplatePublishAll, owner, publication.TemplateID, by, id, owner, publication.TemplateID)
		}
		if err != nil {
			return domain.AgentTemplate{}, fmt.Errorf("publish template: %w", err)
		}
	} else {
		if !publication.Withdraw {
			if err = requireTenants(ctx, tx, publication.Audience.Tenants); err != nil {
				return domain.AgentTemplate{}, err
			}
		}
		for _, tenant := range publication.Audience.Tenants {
			if publication.Withdraw {
				_, err = tx.Query(ctx, qTemplateWithdrawTenant, by, id, owner, publication.TemplateID, tenant)
			} else {
				_, err = tx.Query(ctx, qTemplatePublishTenant, owner, publication.TemplateID, tenant, by, id, owner, publication.TemplateID, tenant)
			}
			if err != nil {
				return domain.AgentTemplate{}, fmt.Errorf("publish template to %d: %w", tenant, err)
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentTemplate{}, fmt.Errorf("commit publication: %w", err)
	}
	committed = true
	return s.Own(ctx, actor.TenantID, publication.TemplateID)
}

func requireTenants(ctx context.Context, tx keelport.TxQueryService, tenants []int64) error {
	ids := make([]string, 0, len(tenants))
	for _, tenant := range tenants {
		ids = append(ids, strconv.FormatInt(tenant, 10))
	}
	res, err := tx.Query(ctx, qTemplateTenantsExist, strings.Join(ids, ","))
	if err != nil {
		return fmt.Errorf("check audience tenants: %w", err)
	}
	found := make(map[int64]bool, len(res.Rows))
	for _, row := range res.Rows {
		found[common.AsInt64(row[0])] = true
	}
	for _, tenant := range tenants {
		if !found[tenant] {
			return validationError("tenants", fmt.Sprintf("tenant %d does not exist", tenant))
		}
	}
	return nil
}

// RegisterPlatform stores the next immutable version of a platform template.
func (s *TableAgentTemplateStore) RegisterPlatform(ctx context.Context, version domain.AgentTemplateVersion) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	payload, err := canonicalTemplatePayload(version.Payload)
	if err != nil {
		return err
	}
	digest := valueDigest(payload)
	tx, err := s.DB.BeginTx(ctx, agentTemplateQueries)
	if err != nil {
		return fmt.Errorf("begin platform template transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Query(ctx, qTemplateCatalogEnsure, version.TemplateID, version.AgentTypeID); err != nil {
		return fmt.Errorf("register platform template: %w", err)
	}
	locked, err := tx.Query(ctx, qTemplateCatalogLock, version.TemplateID)
	if err != nil {
		return fmt.Errorf("lock platform template: %w", err)
	}
	if len(locked.Rows) == 0 || common.AsString(locked.Rows[0][0]) != version.AgentTypeID {
		return fmt.Errorf("%w: platform template %s belongs to another agent type", domain.ErrConflict, version.TemplateID)
	}
	existing, err := tx.Query(ctx, qTemplateCatalogVersion, version.TemplateID, version.Version)
	if err != nil {
		return fmt.Errorf("load platform template version: %w", err)
	}
	if len(existing.Rows) > 0 {
		row := existing.Rows[0]
		if strings.TrimSpace(common.AsString(row[0])) != digest || common.AsString(row[1]) != version.DisplayName ||
			common.AsString(row[2]) != version.Description || common.AsString(row[3]) != version.TypeVersion ||
			strings.TrimSpace(common.AsString(row[4])) != version.TypeDigest || common.AsString(row[5]) != version.ChangeSummary {
			return fmt.Errorf("%w: platform template %s version %d is registered with other content", domain.ErrConflict, version.TemplateID, version.Version)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit platform template: %w", err)
		}
		committed = true
		return nil
	}
	newest, err := tx.Query(ctx, qTemplateCatalogMax, version.TemplateID)
	if err != nil {
		return fmt.Errorf("load newest platform template version: %w", err)
	}
	if max := common.AsInt64(newest.Rows[0][0]); version.Version != max+1 {
		return fmt.Errorf("%w: platform template %s is at version %d, so the next is %d", domain.ErrConflict, version.TemplateID, max, max+1)
	}
	if _, err = tx.Query(ctx, qTemplateCatalogNewVersion, version.TemplateID, version.Version, version.DisplayName, common.NullIfEmpty(version.Description),
		common.NullIfEmpty(version.TypeVersion), common.NullIfEmpty(version.TypeDigest), string(payload), digest, common.NullIfEmpty(version.ChangeSummary)); err != nil {
		return fmt.Errorf("insert platform template version: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit platform template: %w", err)
	}
	committed = true
	return nil
}

func (s *TableAgentTemplateStore) newTemplateID() (string, error) {
	entropy := s.Entropy
	if entropy == nil {
		entropy = rand.Reader
	}
	suffix := make([]byte, 10)
	if _, err := io.ReadFull(entropy, suffix); err != nil {
		return "", fmt.Errorf("generate template id: %w", err)
	}
	return "tpl-" + hex.EncodeToString(suffix), nil
}

// templateVersionRow decodes one template read; the owner-only columns are set only for the tenant's own.
func templateVersionRow(row []any, tenantID int64) (domain.AgentTemplateVersion, error) {
	version := domain.AgentTemplateVersion{
		TemplateID: common.AsString(row[0]), Version: common.AsInt64(row[1]), AgentTypeID: common.AsString(row[2]),
		TypeVersion: common.AsString(row[3]), DisplayName: common.AsString(row[5]),
		Description: common.AsString(row[6]), ChangeSummary: common.AsString(row[7]), Digest: strings.TrimSpace(common.AsString(row[9])),
		CreatedAt: common.AsTime(row[10]), OwnerTenantID: common.AsInt64(row[11]),
	}
	if version.TypeVersion != "" {
		version.TypeDigest = strings.TrimSpace(common.AsString(row[4]))
	}
	payload := []byte(common.AsString(row[8]))
	if valueDigest(payload) != version.Digest {
		return domain.AgentTemplateVersion{}, fmt.Errorf("%w: template %s version %d does not match its digest", domain.ErrConflict, version.TemplateID, version.Version)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&version.Payload); err != nil {
		return domain.AgentTemplateVersion{}, fmt.Errorf("%w: template %s version %d does not decode: %v", domain.ErrConflict, version.TemplateID, version.Version, err)
	}
	switch {
	case version.OwnerTenantID == 0:
		version.Owner = domain.TemplatePlatform
	case version.OwnerTenantID == tenantID:
		version.Owner = domain.TemplateOwn
		version.SourceAgentID, version.SourceDraftRevision = common.AsString(row[12]), common.AsInt64(row[13])
	default:
		version.Owner = domain.TemplateShared
	}
	return version, nil
}

func addAudience(audience *domain.TemplateAudience, value any) {
	if value == nil {
		audience.All = true
		return
	}
	audience.Tenants = append(audience.Tenants, common.AsInt64(value))
}

func sourceRevision(version domain.AgentTemplateVersion) any {
	if version.SourceAgentID == "" {
		return nil
	}
	return version.SourceDraftRevision
}
