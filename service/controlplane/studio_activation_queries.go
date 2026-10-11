package controlplane

import "github.com/nauticana/scout/domain"

const (
	sqlActivationPending   = "'" + string(domain.ActivationPending) + "'"
	sqlActivationActivated = "'" + string(domain.ActivationActivated) + "'"
	sqlActivationRefused   = "'" + string(domain.ActivationRefused) + "'"
	sqlActivationCancelled = "'" + string(domain.ActivationCancelled) + "'"
)

const (
	qScheduleDeployment    = "scout_activation_schedule_deployment"
	qSchedulePending       = "scout_activation_schedule_pending"
	qScheduleInsert        = "scout_activation_schedule_insert"
	qActivationList        = "scout_activation_list"
	qActivationGet         = "scout_activation_get"
	qActivationCancel      = "scout_activation_cancel"
	qActivationDue         = "scout_activation_due"
	qActivationClaim       = "scout_activation_claim"
	qActivationVersion     = "scout_activation_version"
	qActivationFirstStable = "scout_activation_first_stable"
	qActivationStable      = "scout_activation_stable"
	qActivationCanary      = "scout_activation_canary"
	qActivationDeployment  = "scout_activation_deployment"
	qActivationDone        = "scout_activation_done"
	qActivationRefuse      = "scout_activation_refuse"
	qActivationDefer       = "scout_activation_defer"
	qActivationLockDraft   = "scout_activation_lock_draft"
	qActivationAgentState  = "scout_activation_agent_state"
	activationColumns      = `id, partner_id, agent_id, agent_version, definition_digest, canary_percentage, base_stable_version, base_canary_version, effective_at, state_code, scheduled_by_kind, scheduled_by_id, scheduled_at, attempts, next_attempt_at, reason_code, detail, decided_by_kind, decided_by_id, decided_at`
	activationDuePredicate = `state_code = ` + sqlActivationPending + ` AND effective_at <= CURRENT_TIMESTAMP AND (next_attempt_at IS NULL OR next_attempt_at <= CURRENT_TIMESTAMP)`
)

// scheduleQueries run inside the publish transaction.
var scheduleQueries = map[string]string{
	qScheduleDeployment: `SELECT stable_version, canary_version FROM agent_deployment WHERE partner_id = ? AND agent_id = ?`,
	qSchedulePending:    `SELECT id FROM agent_release_activation WHERE partner_id = ? AND agent_id = ? AND state_code = ` + sqlActivationPending,
	qScheduleInsert: `
INSERT INTO agent_release_activation (id, partner_id, agent_id, agent_version, definition_digest, canary_percentage,
       base_stable_version, base_canary_version, effective_at, scheduled_by_kind, scheduled_by_id)
VALUES (NEXTVAL('agent_release_activation_seq'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + activationColumns,
}

// activationQueries back the activation service and its worker. Every deployment write
// compares the stable and canary versions the activation was scheduled over.
var activationQueries = map[string]string{
	qActivationList: `SELECT ` + activationColumns + ` FROM agent_release_activation
 WHERE partner_id = ? AND agent_id = ? ORDER BY effective_at DESC, id DESC LIMIT 100`,
	qActivationGet: `SELECT ` + activationColumns + ` FROM agent_release_activation WHERE partner_id = ? AND agent_id = ? AND id = ?`,
	qActivationCancel: `
UPDATE agent_release_activation
   SET state_code = ` + sqlActivationCancelled + `, decided_by_kind = ?, decided_by_id = ?, decided_at = CURRENT_TIMESTAMP,
       detail = ?, next_attempt_at = NULL
 WHERE partner_id = ? AND agent_id = ? AND id = ? AND state_code = ` + sqlActivationPending + `
RETURNING ` + activationColumns,
	qActivationDue: `SELECT id FROM agent_release_activation WHERE ` + activationDuePredicate + ` ORDER BY effective_at, id LIMIT ?`,
	qActivationClaim: `SELECT ` + activationColumns + ` FROM agent_release_activation
 WHERE id = ? AND ` + activationDuePredicate + ` FOR UPDATE SKIP LOCKED`,
	qActivationVersion: `SELECT definition, definition_digest FROM agent_version WHERE partner_id = ? AND agent_id = ? AND agent_version = ?`,
	qActivationFirstStable: `
INSERT INTO agent_deployment (partner_id, agent_id, stable_version) VALUES (?, ?, ?)
ON CONFLICT (partner_id, agent_id) DO NOTHING RETURNING stable_version`,
	qActivationStable: `
UPDATE agent_deployment
   SET stable_version = ?, canary_version = NULL, canary_percentage = 0, updated_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND agent_id = ? AND stable_version = ? AND canary_version IS NOT DISTINCT FROM ?
RETURNING stable_version`,
	qActivationCanary: `
UPDATE agent_deployment
   SET canary_version = ?, canary_percentage = ?, updated_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND agent_id = ? AND stable_version = ? AND canary_version IS NOT DISTINCT FROM ?
RETURNING stable_version`,
	qActivationDeployment: `SELECT stable_version, canary_version, canary_percentage FROM agent_deployment WHERE partner_id = ? AND agent_id = ?`,
	qActivationDone: `
UPDATE agent_release_activation
   SET state_code = ` + sqlActivationActivated + `, detail = ?, decided_at = CURRENT_TIMESTAMP, next_attempt_at = NULL
 WHERE id = ? AND state_code = ` + sqlActivationPending + ` RETURNING id`,
	qActivationRefuse: `
UPDATE agent_release_activation
   SET state_code = ` + sqlActivationRefused + `, reason_code = ?, detail = ?, attempts = attempts + ?,
       decided_at = CURRENT_TIMESTAMP, next_attempt_at = NULL
 WHERE id = ? AND state_code = ` + sqlActivationPending + ` RETURNING id`,
	qActivationDefer: `
UPDATE agent_release_activation
   SET attempts = attempts + 1, next_attempt_at = CURRENT_TIMESTAMP + (? * INTERVAL '1 second'), detail = ?
 WHERE id = ? AND state_code = ` + sqlActivationPending + ` RETURNING attempts`,
	// Archive and Publish write the draft row; a separate state read then sees a committed archive.
	qActivationLockDraft:  `SELECT draft_revision FROM agent_draft WHERE partner_id = ? AND agent_id = ? FOR SHARE`,
	qActivationAgentState: `SELECT state_code FROM agent_profile WHERE partner_id = ? AND agent_id = ?`,
	qStudioAudit:          studioQueries[qStudioAudit],
	qStudioGetSettings:    studioQueries[qStudioGetSettings],
	qCurrencyExponent:     studioQueries[qCurrencyExponent],
}
