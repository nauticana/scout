package controlplane

import (
	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/service/scope"
)

const (
	qStudioListBindings      = "scout_studio_list_prompt_bindings"
	qStudioInsertBinding     = "scout_studio_insert_prompt_binding"
	qStudioDropBinding       = "scout_studio_drop_prompt_binding"
	qStudioEndBinding        = "scout_studio_end_prompt_binding"
	qStudioResetDropBindings = "scout_studio_reset_drop_prompt_bindings"
	qStudioResetEndBindings  = "scout_studio_reset_end_prompt_bindings"
	qStudioListAgents        = "scout_studio_list_agents"
	qStudioGetDraft          = "scout_studio_get_draft"
	qStudioUpdateProfile     = "scout_studio_update_profile"
	qStudioSetProfileActive  = "scout_studio_set_profile_active"
	qStudioUpdateDraft       = "scout_studio_update_draft"
	qStudioSetDraftEnabled   = "scout_studio_set_draft_enabled"
	qStudioLockAlias         = "scout_studio_lock_alias"
	qStudioBumpAlias         = "scout_studio_bump_alias"
	qStudioAudit             = "scout_studio_audit"
	qStudioLockDraft         = "scout_studio_lock_draft"
	qStudioNextVersion       = "scout_studio_next_version"
	qStudioInsertVersion     = "scout_studio_insert_version"
	qStudioGrantModel        = "scout_studio_grant_model_access"
	qStudioDeployVersion     = "scout_studio_deploy_version"
	qStudioGetVersion        = "scout_studio_get_version"
	qStudioHistory           = "scout_studio_history"
	qStudioAuditLog          = "scout_studio_audit_log"
	qStudioSetAlias          = "scout_studio_set_alias"
	qStudioBumpDraft         = "scout_studio_bump_draft"
	qStudioActiveDefinition  = "scout_studio_active_definition"
	qStudioLastTest          = "scout_studio_last_test"
	qStudioLockType          = "scout_studio_lock_type"
	qStudioTypeCensus        = "scout_studio_type_census"
	qStudioNameTaken         = "scout_studio_name_taken"
	qStudioCreateReplay      = "scout_studio_create_replay"
	qStudioCreateRecord      = "scout_studio_create_record"
	qStudioCreateDraft       = "scout_studio_create_draft"
	qStudioCopyBindings      = "scout_studio_copy_bindings"
	qStudioRename            = "scout_studio_rename"
	qStudioDisableDraft      = "scout_studio_disable_draft"
	qStudioCanaryVersion     = "scout_studio_canary_version"
	qStudioDropSetting       = "scout_studio_drop_setting"
	qStudioEndSetting        = "scout_studio_end_setting"
	qStudioInsertSetting     = "scout_studio_insert_setting"
	qStudioGetSettings       = "scout_studio_get_settings"
)

var studioQueries = common.MergeMaps(scope.ReleaseWriteQueries, map[string]string{
	// Prompt bindings are temporal: an edit ends the open row and binds a new one. A row bound
	// in this same instant was never in force, so it is dropped rather than ended at its own begda.
	qStudioListBindings: `
SELECT resource_id, merge_mode_code, sealed, resource_value
  FROM config_scope_binding
 WHERE partner_id = ? AND scope_id = ? AND resource_kind_code = 'prompt_section' AND endda IS NULL`,
	qStudioInsertBinding: `
INSERT INTO config_scope_binding (partner_id, scope_id, resource_kind_code, resource_id, resource_version,
                                  merge_mode_code, sealed, resource_value, resource_value_digest, bound_by)
VALUES (?, ?, 'prompt_section', ?, ?, ?, ?, ?, ?, ?)`,
	qStudioDropBinding: `
DELETE FROM config_scope_binding
 WHERE partner_id = ? AND scope_id = ? AND resource_kind_code = 'prompt_section' AND resource_id = ?
   AND endda IS NULL AND begda >= CURRENT_TIMESTAMP`,
	qStudioEndBinding: `
UPDATE config_scope_binding
   SET endda = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND scope_id = ? AND resource_kind_code = 'prompt_section' AND resource_id = ?
   AND endda IS NULL`,
	qStudioResetDropBindings: `
DELETE FROM config_scope_binding
 WHERE partner_id = ? AND scope_id = ? AND resource_kind_code = 'prompt_section'
   AND (? = '' OR split_part(resource_id, '/', 1) = ?)
   AND (? = '' OR split_part(resource_id, '/', 2) = ?)
   AND endda IS NULL AND begda >= CURRENT_TIMESTAMP`,
	qStudioResetEndBindings: `
UPDATE config_scope_binding
   SET endda = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND scope_id = ? AND resource_kind_code = 'prompt_section'
   AND (? = '' OR split_part(resource_id, '/', 1) = ?)
   AND (? = '' OR split_part(resource_id, '/', 2) = ?)
   AND endda IS NULL`,
	qStudioListAgents: `
SELECT p.agent_id, p.agent_type_id, p.display_name, p.state_code, d.enabled,
       d.draft_revision, COALESCE(a.revision, 0), COALESCE(a.agent_id = p.agent_id, FALSE),
       dep.stable_version, v.published_at, v.definition
  FROM agent_profile p
  JOIN agent_draft d ON d.partner_id = p.partner_id AND d.agent_id = p.agent_id
  LEFT JOIN agent_alias a ON a.partner_id = p.partner_id AND a.agent_type_id = p.agent_type_id
  LEFT JOIN agent_deployment dep ON dep.partner_id = p.partner_id AND dep.agent_id = p.agent_id
  LEFT JOIN agent_version v ON v.partner_id = dep.partner_id AND v.agent_id = dep.agent_id AND v.agent_version = dep.stable_version
 WHERE p.partner_id = ?
 ORDER BY p.agent_type_id, p.display_name, p.agent_id`,
	qStudioGetDraft: `
SELECT p.agent_type_id, p.display_name, p.state_code, d.enabled,
       d.text_model_provider, d.text_model_id, d.image_model_provider, d.image_model_id,
       d.video_model_provider, d.video_model_id, d.extension, d.draft_revision,
       COALESCE(a.revision, 0), COALESCE(a.agent_id = p.agent_id, FALSE)
  FROM agent_profile p
  JOIN agent_draft d ON d.partner_id = p.partner_id AND d.agent_id = p.agent_id
  LEFT JOIN agent_alias a ON a.partner_id = p.partner_id AND a.agent_type_id = p.agent_type_id
 WHERE p.partner_id = ? AND p.agent_id = ?`,
	qStudioUpdateProfile:    `UPDATE agent_profile SET display_name = ?, state_code = ? WHERE partner_id = ? AND agent_id = ?`,
	qStudioSetProfileActive: `UPDATE agent_profile SET state_code = ?, state_reason = ?, state_changed_by = ?, state_changed_at = CURRENT_TIMESTAMP WHERE partner_id = ? AND agent_id = ?`,
	qStudioUpdateDraft: `
UPDATE agent_draft
   SET enabled = ?, text_model_provider = ?, text_model_id = ?,
       image_model_provider = ?, image_model_id = ?, video_model_provider = ?, video_model_id = ?,
       extension = ?, modified_by = ?, modified_at = CURRENT_TIMESTAMP, draft_revision = draft_revision + 1
 WHERE partner_id = ? AND agent_id = ? AND draft_revision = ?
 RETURNING draft_revision`,
	qStudioSetDraftEnabled: `
UPDATE agent_draft
   SET enabled = ?, modified_by = ?, modified_at = CURRENT_TIMESTAMP, draft_revision = draft_revision + 1
 WHERE partner_id = ? AND agent_id = ? AND draft_revision = ?
 RETURNING draft_revision`,
	qStudioLockAlias: `SELECT agent_id, revision FROM agent_alias WHERE partner_id = ? AND agent_type_id = ? FOR UPDATE`,
	qStudioBumpAlias: `
UPDATE agent_alias SET revision = revision + 1, modified_by = ?, modified_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND agent_type_id = ? AND revision = ? RETURNING revision`,
	qStudioAudit: `
INSERT INTO agent_studio_event (id, partner_id, agent_id, event, detail, principal_kind, principal_id)
VALUES (NEXTVAL('agent_studio_event_seq'), ?, ?, ?, ?, ?, ?)`,
	qStudioLockDraft: `
SELECT d.draft_revision, p.agent_type_id
  FROM agent_draft d
  JOIN agent_profile p ON p.partner_id = d.partner_id AND p.agent_id = d.agent_id
 WHERE d.partner_id = ? AND d.agent_id = ? FOR UPDATE OF d`,
	qStudioNextVersion: `
SELECT COALESCE(MAX(CASE WHEN agent_version ~ '^[0-9]+$' THEN CAST(agent_version AS BIGINT) END), 0) + 1
  FROM agent_version WHERE partner_id = ? AND agent_id = ?`,
	qStudioGrantModel: `
INSERT INTO tenant_model_access (partner_id, provider_id, model_id, priority_class_code)
VALUES (?, ?, ?, 'standard')
ON CONFLICT (partner_id, provider_id, model_id) DO NOTHING`,
	qStudioInsertVersion: `
INSERT INTO agent_version
       (partner_id, agent_id, agent_version, definition, definition_digest, draft_revision,
        prompt_profile_revision, change_summary, published_by, restored_from_version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qStudioDeployVersion: `
INSERT INTO agent_deployment (partner_id, agent_id, stable_version)
VALUES (?, ?, ?)
ON CONFLICT (partner_id, agent_id) DO UPDATE
SET stable_version = EXCLUDED.stable_version, canary_version = NULL,
    canary_percentage = 0, updated_at = CURRENT_TIMESTAMP`,
	// A setting is one binding of any kind; like a prompt binding, an edit ends the open row.
	qStudioDropSetting: `
DELETE FROM config_scope_binding
 WHERE partner_id = ? AND scope_id = ? AND resource_kind_code = ? AND resource_id = ?
   AND endda IS NULL AND begda >= CURRENT_TIMESTAMP`,
	qStudioEndSetting: `
UPDATE config_scope_binding SET endda = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND scope_id = ? AND resource_kind_code = ? AND resource_id = ? AND endda IS NULL`,
	qStudioInsertSetting: `
INSERT INTO config_scope_binding (partner_id, scope_id, resource_kind_code, resource_id, resource_version,
                                  merge_mode_code, sealed, resource_value, resource_value_digest, bound_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qStudioGetSettings: `
SELECT scope_id, payload, compiled_at, compiled_by
  FROM effective_agent_release WHERE partner_id = ? AND agent_id = ? AND agent_version = ?`,
	qStudioCanaryVersion: `
UPDATE agent_deployment SET canary_version = ?, canary_percentage = ?, updated_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND agent_id = ? RETURNING stable_version`,
	qStudioGetVersion: `
SELECT v.definition, COALESCE(dep.stable_version = v.agent_version, FALSE)
  FROM agent_version v
  LEFT JOIN agent_deployment dep ON dep.partner_id = v.partner_id AND dep.agent_id = v.agent_id
 WHERE v.partner_id = ? AND v.agent_id = ? AND v.agent_version = ?`,
	qStudioHistory: `
SELECT v.definition, COALESCE(dep.stable_version = v.agent_version, FALSE)
  FROM agent_version v
  LEFT JOIN agent_deployment dep ON dep.partner_id = v.partner_id AND dep.agent_id = v.agent_id
 WHERE v.partner_id = ? AND v.agent_id = ?
 ORDER BY v.published_at DESC, v.agent_version DESC`,
	qStudioAuditLog: `
SELECT event, detail, principal_kind, principal_id, occurred_at
  FROM agent_studio_event WHERE partner_id = ? AND agent_id = ?
 ORDER BY occurred_at DESC, id DESC`,
	qStudioSetAlias: `
UPDATE agent_alias
   SET agent_id = ?, revision = revision + 1, modified_by = ?, modified_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND agent_type_id = ? AND revision = ? RETURNING revision`,
	qStudioBumpDraft: `
UPDATE agent_draft SET draft_revision = draft_revision + 1, modified_by = ?, modified_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND agent_id = ? AND draft_revision = ? RETURNING draft_revision`,
	qStudioLastTest: `
SELECT agent_id, MAX(occurred_at) FROM agent_studio_event
 WHERE partner_id = ? AND event = 'TEST' GROUP BY agent_id`,
	qStudioLockType: `SELECT agent_type_id FROM agent_type WHERE partner_id = ? AND agent_type_id = ? FOR UPDATE`,
	qStudioTypeCensus: `
SELECT COUNT(*), COUNT(*) FILTER (WHERE LOWER(display_name) = LOWER(?))
  FROM agent_profile WHERE partner_id = ? AND agent_type_id = ? AND state_code <> 'retired'`,
	qStudioNameTaken: `
SELECT 1 FROM agent_profile
 WHERE partner_id = ? AND agent_type_id = ? AND LOWER(display_name) = LOWER(?) AND agent_id <> ? AND state_code <> 'retired'`,
	qStudioCreateReplay: `
SELECT request_digest, agent_id FROM agent_create_request
 WHERE partner_id = ? AND principal_kind = ? AND principal_id = ? AND idempotency_key = ?`,
	qStudioCreateRecord: `
INSERT INTO agent_create_request (partner_id, principal_kind, principal_id, idempotency_key, request_digest, agent_id)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (partner_id, principal_kind, principal_id, idempotency_key) DO NOTHING
RETURNING agent_id`,
	qStudioCreateDraft: `
INSERT INTO agent_draft (partner_id, agent_id, enabled,
       text_model_provider, text_model_id, image_model_provider, image_model_id,
       video_model_provider, video_model_id, extension, modified_by)
VALUES (?, ?, FALSE, ?, ?, ?, ?, ?, ?, ?, ?)`,
	// A copy takes every kind of binding in force at the source agent's own scope.
	qStudioCopyBindings: `
INSERT INTO config_scope_binding (partner_id, scope_id, resource_kind_code, resource_id, resource_version,
                                  merge_mode_code, sealed, resource_value, resource_value_digest, bound_by)
SELECT partner_id, ?, resource_kind_code, resource_id, resource_version,
       merge_mode_code, sealed, resource_value, resource_value_digest, ?
  FROM config_scope_binding
 WHERE partner_id = ? AND scope_id = ? AND endda IS NULL`,
	qStudioRename:       `UPDATE agent_profile SET display_name = ? WHERE partner_id = ? AND agent_id = ?`,
	qStudioDisableDraft: `UPDATE agent_draft SET enabled = FALSE WHERE partner_id = ? AND agent_id = ?`,
	qProvisionScope:     provisionQueries[qProvisionScope],
	qProvisionProfile:   provisionQueries[qProvisionProfile],
	qStudioActiveDefinition: `
SELECT v.agent_version, v.definition
  FROM agent_deployment dep
  JOIN agent_version v ON v.partner_id = dep.partner_id AND v.agent_id = dep.agent_id AND v.agent_version = dep.stable_version
 WHERE dep.partner_id = ? AND dep.agent_id = ?`,
})
