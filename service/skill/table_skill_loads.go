package skill

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qSkillLoadInsert = "scout_skill_load_insert"
	qSkillLoadRuns   = "scout_skill_load_runs"
	qSkillLoadPurge  = "scout_skill_load_purge"
)

var skillLoadQueries = map[string]string{
	qSkillLoadInsert: `
INSERT INTO skill_load (partner_id, request_id, skill_id, agent_id, agent_version)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (partner_id, request_id, skill_id) DO NOTHING`,
	qSkillLoadRuns: `
SELECT COUNT(*), COUNT(l.skill_id)
  FROM agent_run r
  LEFT JOIN skill_load l ON l.partner_id = r.partner_id AND l.request_id = r.request_id AND l.skill_id = ?
                        AND l.agent_id = r.agent_id AND l.agent_version = r.agent_version
 WHERE r.partner_id = ? AND r.agent_id = ? AND r.agent_version = ? AND r.completed_at >= ? AND r.completed_at < ?`,
	qSkillLoadPurge: `
DELETE FROM skill_load
 WHERE (partner_id, request_id, skill_id) IN (
       SELECT partner_id, request_id, skill_id FROM skill_load
        WHERE loaded_at < CURRENT_TIMESTAMP - make_interval(days => ?)
        ORDER BY loaded_at
        LIMIT ?)
RETURNING request_id`,
}

// TableSkillLoads is the SkillLoads over skill_load. Runs joins agent_run, so it needs the
// runtime schema module; Purge applies the run retention horizon to the loads.
type TableSkillLoads struct {
	DB port.DatabaseRepository

	once sync.Once
	qs   port.QueryService
}

var _ contract.SkillLoads = (*TableSkillLoads)(nil)
var _ contract.AgentRunPurger = (*TableSkillLoads)(nil)

func (loads *TableSkillLoads) init(ctx context.Context) error {
	if loads.DB == nil {
		return fmt.Errorf("skill loads: database is required")
	}
	loads.once.Do(func() { loads.qs = loads.DB.GetQueryService(ctx, skillLoadQueries) })
	if loads.qs == nil {
		return fmt.Errorf("skill loads: query service is required")
	}
	return nil
}

// Record keeps one load of a skill the principal's release binds.
func (loads *TableSkillLoads) Record(ctx context.Context, principal domain.Principal, requestID, skillID string) error {
	if err := loads.init(ctx); err != nil {
		return err
	}
	if principal.TenantID <= 0 || principal.ID == "" || principal.Release == "" || strings.TrimSpace(requestID) == "" || skillID == "" {
		return fmt.Errorf("%w: tenant, release, request, and skill are required", domain.ErrValidation)
	}
	if _, err := loads.qs.Query(ctx, qSkillLoadInsert, principal.TenantID, requestID, skillID, principal.ID, principal.Release); err != nil {
		return fmt.Errorf("record load of skill %q: %w", skillID, err)
	}
	return nil
}

// Runs counts the release's runs settled in [from, to) and those that loaded the skill.
func (loads *TableSkillLoads) Runs(ctx context.Context, tenantID int64, agentID, agentVersion, skillID string, from, to time.Time) (domain.SkillRunCount, error) {
	if err := loads.init(ctx); err != nil {
		return domain.SkillRunCount{}, err
	}
	if tenantID <= 0 || strings.TrimSpace(agentID) == "" || strings.TrimSpace(agentVersion) == "" || strings.TrimSpace(skillID) == "" {
		return domain.SkillRunCount{}, fmt.Errorf("%w: tenant, agent, version, and skill are required", domain.ErrValidation)
	}
	if from.IsZero() || !from.Before(to) {
		return domain.SkillRunCount{}, fmt.Errorf("%w: the window needs a start before its end", domain.ErrValidation)
	}
	result, err := loads.qs.Query(ctx, qSkillLoadRuns, skillID, tenantID, agentID, agentVersion, from.UTC(), to.UTC())
	if err != nil {
		return domain.SkillRunCount{}, fmt.Errorf("count runs of %s@%s loading skill %q: %w", agentID, agentVersion, skillID, err)
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) < 2 {
		return domain.SkillRunCount{}, fmt.Errorf("count runs of %s@%s loading skill %q: malformed result", agentID, agentVersion, skillID)
	}
	return domain.SkillRunCount{Settled: common.AsInt64(result.Rows[0][0]), Loaded: common.AsInt64(result.Rows[0][1])}, nil
}

// Purge deletes at most limit loads older than the retention horizon; zero days keeps every load.
func (loads *TableSkillLoads) Purge(ctx context.Context, retentionDays, limit int) (int64, error) {
	switch {
	case retentionDays < 0:
		return 0, fmt.Errorf("%w: retention days cannot be negative", domain.ErrValidation)
	case retentionDays == 0:
		return 0, nil
	case limit <= 0:
		return 0, fmt.Errorf("%w: purge limit must be positive", domain.ErrValidation)
	}
	if err := loads.init(ctx); err != nil {
		return 0, err
	}
	result, err := loads.qs.Query(ctx, qSkillLoadPurge, retentionDays, limit)
	if err != nil {
		return 0, fmt.Errorf("purge skill loads: %w", err)
	}
	return int64(len(result.Rows)), nil
}
