package controlplane

const (
	qStudioListAgents       = "scout_studio_list_agents"
	qStudioGetDraft         = "scout_studio_get_draft"
	qStudioUpdateProfile    = "scout_studio_update_profile"
	qStudioSetProfileActive = "scout_studio_set_profile_active"
	qStudioUpdateDraft      = "scout_studio_update_draft"
	qStudioSetDraftEnabled  = "scout_studio_set_draft_enabled"
	qStudioDeleteOverrides  = "scout_studio_delete_overrides"
	qStudioInsertOverride   = "scout_studio_insert_override"
	qStudioLockAlias        = "scout_studio_lock_alias"
	qStudioListDefaults     = "scout_studio_list_defaults"
	qStudioBumpAlias        = "scout_studio_bump_alias"
	qStudioDeleteDefaults   = "scout_studio_delete_defaults"
	qStudioInsertDefault    = "scout_studio_insert_default"
	qStudioAudit            = "scout_studio_audit"
	qStudioLockDraft        = "scout_studio_lock_draft"
	qStudioNextVersion      = "scout_studio_next_version"
	qStudioInsertVersion    = "scout_studio_insert_version"
	qStudioDeployVersion    = "scout_studio_deploy_version"
	qStudioGetVersion       = "scout_studio_get_version"
	qStudioHistory          = "scout_studio_history"
	qStudioAuditLog         = "scout_studio_audit_log"
	qStudioSetAlias         = "scout_studio_set_alias"
	qStudioBumpDraft        = "scout_studio_bump_draft"
	qStudioResetOverrides   = "scout_studio_reset_overrides"
	qStudioResetOverrideSec = "scout_studio_reset_override_section"
	qStudioResetOverrideLng = "scout_studio_reset_override_language"
	qStudioResetOverrideOne = "scout_studio_reset_override_one"
	qStudioResetDefaults    = "scout_studio_reset_defaults"
	qStudioResetDefaultSec  = "scout_studio_reset_default_section"
	qStudioResetDefaultLng  = "scout_studio_reset_default_language"
	qStudioResetDefaultOne  = "scout_studio_reset_default_one"
	qStudioActiveDefinition = "scout_studio_active_definition"
	qStudioLastTest         = "scout_studio_last_test"
)

var studioQueries = map[string]string{
	qStudioListAgents: `
SELECT p.agent_id, p.agent_type_id, p.display_name, p.state_code = 'active', d.enabled,
       d.draft_revision, COALESCE(a.revision, 0), COALESCE(a.agent_id = p.agent_id, FALSE),
       dep.stable_version, v.published_at, v.definition
  FROM agent_profile p
  JOIN agent_draft d ON d.tenant_id = p.tenant_id AND d.agent_id = p.agent_id
  LEFT JOIN agent_alias a ON a.tenant_id = p.tenant_id AND a.agent_type_id = p.agent_type_id
  LEFT JOIN agent_deployment dep ON dep.tenant_id = p.tenant_id AND dep.agent_id = p.agent_id
  LEFT JOIN agent_version v ON v.tenant_id = dep.tenant_id AND v.agent_id = dep.agent_id AND v.agent_version = dep.stable_version
 WHERE p.tenant_id = ?
 ORDER BY p.agent_type_id, p.display_name, p.agent_id`,
	qStudioGetDraft: `
SELECT p.agent_type_id, p.display_name, p.state_code = 'active', d.enabled, d.require_approval,
       d.text_model_provider, d.text_model_id, d.image_model_provider, d.image_model_id,
       d.video_model_provider, d.video_model_id, d.extension, d.draft_revision,
       COALESCE(a.revision, 0), COALESCE(a.agent_id = p.agent_id, FALSE)
  FROM agent_profile p
  JOIN agent_draft d ON d.tenant_id = p.tenant_id AND d.agent_id = p.agent_id
  LEFT JOIN agent_alias a ON a.tenant_id = p.tenant_id AND a.agent_type_id = p.agent_type_id
 WHERE p.tenant_id = ? AND p.agent_id = ?`,
	qStudioUpdateProfile:    `UPDATE agent_profile SET display_name = ?, state_code = ? WHERE tenant_id = ? AND agent_id = ?`,
	qStudioSetProfileActive: `UPDATE agent_profile SET state_code = ?, state_reason = ?, state_changed_by = ?, state_changed_at = CURRENT_TIMESTAMP WHERE tenant_id = ? AND agent_id = ?`,
	qStudioUpdateDraft: `
UPDATE agent_draft
   SET enabled = ?, require_approval = ?, text_model_provider = ?, text_model_id = ?,
       image_model_provider = ?, image_model_id = ?, video_model_provider = ?, video_model_id = ?,
       extension = ?, modified_by = ?, modified_at = CURRENT_TIMESTAMP, draft_revision = draft_revision + 1
 WHERE tenant_id = ? AND agent_id = ? AND draft_revision = ?
 RETURNING draft_revision`,
	qStudioSetDraftEnabled: `
UPDATE agent_draft
   SET enabled = ?, modified_by = ?, modified_at = CURRENT_TIMESTAMP, draft_revision = draft_revision + 1
 WHERE tenant_id = ? AND agent_id = ? AND draft_revision = ?
 RETURNING draft_revision`,
	qStudioDeleteOverrides: `DELETE FROM agent_prompt_override WHERE tenant_id = ? AND agent_id = ?`,
	qStudioInsertOverride: `
INSERT INTO agent_prompt_override
       (tenant_id, agent_id, prompt_section_id, language_code, overwrite, instruction, output)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
	qStudioLockAlias: `SELECT agent_id, revision FROM agent_alias WHERE tenant_id = ? AND agent_type_id = ? FOR UPDATE`,
	qStudioListDefaults: `
SELECT prompt_section_id, language_code, instruction, output
  FROM tenant_prompt_default WHERE tenant_id = ? AND agent_type_id = ?`,
	qStudioBumpAlias: `
UPDATE agent_alias SET revision = revision + 1, modified_by = ?, modified_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND agent_type_id = ? AND revision = ? RETURNING revision`,
	qStudioDeleteDefaults: `DELETE FROM tenant_prompt_default WHERE tenant_id = ? AND agent_type_id = ?`,
	qStudioInsertDefault: `
INSERT INTO tenant_prompt_default
       (tenant_id, agent_type_id, prompt_section_id, language_code, instruction, output)
VALUES (?, ?, ?, ?, ?, ?)`,
	qStudioAudit: `
INSERT INTO agent_studio_event (id, tenant_id, agent_id, event, detail, actor_id)
VALUES (NEXTVAL('agent_studio_event_seq'), ?, ?, ?, ?, ?)`,
	qStudioLockDraft: `
SELECT d.draft_revision, p.agent_type_id
  FROM agent_draft d
  JOIN agent_profile p ON p.tenant_id = d.tenant_id AND p.agent_id = d.agent_id
 WHERE d.tenant_id = ? AND d.agent_id = ? FOR UPDATE OF d`,
	qStudioNextVersion: `
SELECT COALESCE(MAX(CASE WHEN agent_version ~ '^[0-9]+$' THEN CAST(agent_version AS BIGINT) END), 0) + 1
  FROM agent_version WHERE tenant_id = ? AND agent_id = ?`,
	qStudioInsertVersion: `
INSERT INTO agent_version
       (tenant_id, agent_id, agent_version, definition, definition_digest, draft_revision,
        prompt_profile_revision, change_summary, published_by, restored_from_version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qStudioDeployVersion: `
INSERT INTO agent_deployment (tenant_id, agent_id, stable_version)
VALUES (?, ?, ?)
ON CONFLICT (tenant_id, agent_id) DO UPDATE
SET stable_version = EXCLUDED.stable_version, canary_version = NULL,
    canary_percentage = 0, updated_at = CURRENT_TIMESTAMP`,
	qStudioGetVersion: `
SELECT v.definition, COALESCE(dep.stable_version = v.agent_version, FALSE)
  FROM agent_version v
  LEFT JOIN agent_deployment dep ON dep.tenant_id = v.tenant_id AND dep.agent_id = v.agent_id
 WHERE v.tenant_id = ? AND v.agent_id = ? AND v.agent_version = ?`,
	qStudioHistory: `
SELECT v.definition, COALESCE(dep.stable_version = v.agent_version, FALSE)
  FROM agent_version v
  LEFT JOIN agent_deployment dep ON dep.tenant_id = v.tenant_id AND dep.agent_id = v.agent_id
 WHERE v.tenant_id = ? AND v.agent_id = ?
 ORDER BY v.published_at DESC, v.agent_version DESC`,
	qStudioAuditLog: `
SELECT event, detail, actor_id, occurred_at
  FROM agent_studio_event WHERE tenant_id = ? AND agent_id = ?
 ORDER BY occurred_at DESC, id DESC`,
	qStudioSetAlias: `
UPDATE agent_alias
   SET agent_id = ?, revision = revision + 1, modified_by = ?, modified_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND agent_type_id = ? AND revision = ? RETURNING revision`,
	qStudioBumpDraft: `
UPDATE agent_draft SET draft_revision = draft_revision + 1, modified_by = ?, modified_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND agent_id = ? AND draft_revision = ? RETURNING draft_revision`,
	qStudioResetOverrides:   `DELETE FROM agent_prompt_override WHERE tenant_id = ? AND agent_id = ?`,
	qStudioResetOverrideSec: `DELETE FROM agent_prompt_override WHERE tenant_id = ? AND agent_id = ? AND prompt_section_id = ?`,
	qStudioResetOverrideLng: `DELETE FROM agent_prompt_override WHERE tenant_id = ? AND agent_id = ? AND language_code = ?`,
	qStudioResetOverrideOne: `DELETE FROM agent_prompt_override WHERE tenant_id = ? AND agent_id = ? AND prompt_section_id = ? AND language_code = ?`,
	qStudioResetDefaults:    `DELETE FROM tenant_prompt_default WHERE tenant_id = ? AND agent_type_id = ?`,
	qStudioResetDefaultSec:  `DELETE FROM tenant_prompt_default WHERE tenant_id = ? AND agent_type_id = ? AND prompt_section_id = ?`,
	qStudioResetDefaultLng:  `DELETE FROM tenant_prompt_default WHERE tenant_id = ? AND agent_type_id = ? AND language_code = ?`,
	qStudioResetDefaultOne:  `DELETE FROM tenant_prompt_default WHERE tenant_id = ? AND agent_type_id = ? AND prompt_section_id = ? AND language_code = ?`,
	qStudioLastTest: `
SELECT agent_id, MAX(occurred_at) FROM agent_studio_event
 WHERE tenant_id = ? AND event = 'TEST' GROUP BY agent_id`,
	qStudioActiveDefinition: `
SELECT v.agent_version, v.definition
  FROM agent_deployment dep
  JOIN agent_version v ON v.tenant_id = dep.tenant_id AND v.agent_id = dep.agent_id AND v.agent_version = dep.stable_version
 WHERE dep.tenant_id = ? AND dep.agent_id = ?`,
}
