package runtime

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// FlagAgentRunRetentionDays is the application_config_flag Scout seeds to bound
// agent run history.
const FlagAgentRunRetentionDays = "agent_run_retention_days"

const (
	qRecordAgentRun = "scout_runtime_record_agent_run"
	qAgentLastRun   = "scout_runtime_agent_last_run"
	qPurgeAgentRuns = "scout_runtime_purge_agent_runs"
)

var agentRunQueries = map[string]string{
	qRecordAgentRun: `
INSERT INTO agent_run (id, tenant_id, agent_id, agent_version, task_kind)
SELECT nextval('agent_run_seq'), ?, ?, ?, ?
  FROM agent_version
 WHERE tenant_id = ? AND agent_id = ? AND agent_version = ? AND definition_digest = ?
RETURNING id`,
	qAgentLastRun: `
SELECT agent_id, MAX(completed_at)
  FROM agent_run
 WHERE tenant_id = ?
 GROUP BY agent_id`,
	qPurgeAgentRuns: `
DELETE FROM agent_run
 WHERE id IN (SELECT id FROM agent_run
               WHERE completed_at < CURRENT_TIMESTAMP - make_interval(days => ?)
               ORDER BY completed_at
               LIMIT ?)
RETURNING id`,
}

// AgentRunStore records successful release executions and reports their newest
// completion per agent.
type AgentRunStore struct {
	DB keelport.DatabaseRepository
	// RetentionDays overrides the agent_run_retention_days flag; zero reads it.
	RetentionDays int

	once          sync.Once
	qs            keelport.QueryService
	retentionOnce sync.Once
	retention     int
	retentionErr  error
}

var _ contract.AgentRunRecorder = (*AgentRunStore)(nil)
var _ contract.AgentActivityReporter = (*AgentRunStore)(nil)
var _ contract.AgentRunPurger = (*AgentRunStore)(nil)

func (store *AgentRunStore) init(ctx context.Context) error {
	if store.qs != nil {
		return nil
	}
	if store.DB == nil {
		return fmt.Errorf("agent run store: database is required")
	}
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, agentRunQueries) })
	return nil
}

// Record persists one successful execution after verifying the full release
// reference against the immutable version row.
func (store *AgentRunStore) Record(ctx context.Context, tenantID int64, release domain.AgentReleaseReference, taskKind string) error {
	release.AgentID = strings.TrimSpace(release.AgentID)
	release.Version = strings.TrimSpace(release.Version)
	release.Digest = strings.TrimSpace(release.Digest)
	taskKind = strings.TrimSpace(taskKind)
	if tenantID <= 0 || release.AgentID == "" || release.Version == "" || len(release.Digest) != 64 || taskKind == "" || len(taskKind) > 80 {
		return fmt.Errorf("%w: tenant, release identity, digest, and task kind are required", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return err
	}
	result, err := store.qs.Query(ctx, qRecordAgentRun,
		tenantID, release.AgentID, release.Version, taskKind,
		tenantID, release.AgentID, release.Version, release.Digest,
	)
	if err != nil {
		return fmt.Errorf("record agent run: %w", err)
	}
	if len(result.Rows) == 0 {
		return fmt.Errorf("%w: agent release reference does not match", domain.ErrConflict)
	}
	return nil
}

// LastRun returns the newest successful execution time for every tenant agent.
func (store *AgentRunStore) LastRun(ctx context.Context, tenantID int64) (map[string]time.Time, error) {
	if tenantID <= 0 {
		return nil, fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return nil, err
	}
	result, err := store.qs.Query(ctx, qAgentLastRun, tenantID)
	if err != nil {
		return nil, fmt.Errorf("load agent run activity: %w", err)
	}
	lastRun := make(map[string]time.Time, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) < 2 {
			return nil, fmt.Errorf("%w: malformed agent run activity row", domain.ErrConflict)
		}
		agentID := strings.TrimSpace(common.AsString(row[0]))
		completedAt, ok := common.AsTimeOK(row[1])
		if agentID == "" || !ok {
			return nil, fmt.Errorf("%w: malformed agent run activity", domain.ErrConflict)
		}
		lastRun[agentID] = completedAt
	}
	return lastRun, nil
}

// Purge deletes run activity past the agent_run_retention_days horizon, at most
// limit rows per call. The flag needs a restart to take effect, so it is read
// once. A non-positive retention keeps activity forever and purges nothing.
func (store *AgentRunStore) Purge(ctx context.Context, limit int) (int64, error) {
	retentionDays, err := store.retentionDays(ctx)
	if err != nil {
		return 0, err
	}
	if retentionDays <= 0 {
		return 0, nil
	}
	if limit <= 0 {
		return 0, fmt.Errorf("%w: purge limit must be positive", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return 0, err
	}
	result, err := store.qs.Query(ctx, qPurgeAgentRuns, retentionDays, limit)
	if err != nil {
		return 0, fmt.Errorf("purge agent runs: %w", err)
	}
	return int64(len(result.Rows)), nil
}

func (store *AgentRunStore) retentionDays(ctx context.Context) (int, error) {
	if store.RetentionDays != 0 {
		return store.RetentionDays, nil
	}
	if store.DB == nil {
		return 0, fmt.Errorf("agent run store: database is required")
	}
	store.retentionOnce.Do(func() {
		rows, err := (&common.BaseConfig{}).LoadValues(ctx, store.DB)
		if err != nil {
			store.retentionErr = fmt.Errorf("read %s: %w", FlagAgentRunRetentionDays, err)
			return
		}
		row, ok := rows[FlagAgentRunRetentionDays]
		if !ok {
			store.retentionErr = fmt.Errorf("%w: %s is not seeded", domain.ErrValidation, FlagAgentRunRetentionDays)
			return
		}
		value := strings.TrimSpace(row.Value)
		if value == "" {
			value = strings.TrimSpace(row.Default)
		}
		days, err := strconv.Atoi(value)
		if err != nil {
			store.retentionErr = fmt.Errorf("%w: %s is not an integer: %q", domain.ErrValidation, FlagAgentRunRetentionDays, value)
			return
		}
		store.retention = days
	})
	return store.retention, store.retentionErr
}
