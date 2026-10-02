package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qRecordAgentRun    = "scout_runtime_record_agent_run"
	qAgentRunByRequest = "scout_runtime_agent_run_by_request"
	qAgentRuns         = "scout_runtime_agent_runs"
	qAgentLastRun      = "scout_runtime_agent_last_run"
	qPurgeAgentRuns    = "scout_runtime_purge_agent_runs"

	// MaxAgentRunPage bounds one Runs page.
	MaxAgentRunPage = 500
)

var agentRunQueries = map[string]string{
	qRecordAgentRun: `
INSERT INTO agent_run (id, tenant_id, agent_id, agent_version, task_kind, request_id, status_code)
SELECT nextval('agent_run_seq'), ?, ?, ?, ?, ?, ?
  FROM agent_version
 WHERE tenant_id = ? AND agent_id = ? AND agent_version = ? AND (? = '' OR definition_digest = ?)
ON CONFLICT (tenant_id, request_id) DO NOTHING
RETURNING id`,
	qAgentRunByRequest: `
SELECT agent_id, agent_version, task_kind, status_code
  FROM agent_run
 WHERE tenant_id = ? AND request_id = ?`,
	qAgentRuns: `
SELECT id, agent_id, agent_version, task_kind, request_id, status_code, completed_at
  FROM agent_run
 WHERE tenant_id = ?
   AND (? = '' OR agent_id = ?)
   AND (? = '' OR request_id = ?)
   AND (? = '' OR status_code = ?)
   AND (? = 0 OR id < ?)
 ORDER BY id DESC
 LIMIT ?`,
	qAgentLastRun: `
SELECT agent_id, MAX(completed_at)
  FROM agent_run
 WHERE tenant_id = ? AND status_code = 'completed'
 GROUP BY agent_id`,
	qPurgeAgentRuns: `
DELETE FROM agent_run
 WHERE id IN (SELECT id FROM agent_run
               WHERE completed_at < CURRENT_TIMESTAMP - make_interval(days => ?)
               ORDER BY completed_at
               LIMIT ?)
RETURNING id`,
}

// AgentRunStore records settled release executions, lists them, and reports the
// newest successful completion per agent.
type AgentRunStore struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.AgentRunRecorder = (*AgentRunStore)(nil)
var _ contract.AgentRunQuery = (*AgentRunStore)(nil)
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

// Record persists one settled execution after verifying the release against the
// immutable version row. Replaying the same request, release, task, and status is
// a no-op; any mismatch is ErrConflict.
func (store *AgentRunStore) Record(ctx context.Context, tenantID int64, run domain.AgentRun) error {
	release := run.Release
	release.AgentID, release.Version = strings.TrimSpace(release.AgentID), strings.TrimSpace(release.Version)
	release.Digest = strings.TrimSpace(release.Digest)
	taskKind, requestID := strings.TrimSpace(run.TaskKind), strings.TrimSpace(run.RequestID)
	switch {
	case tenantID <= 0 || release.AgentID == "" || release.Version == "" || taskKind == "" || len(taskKind) > 80 || len(requestID) > 120:
		return fmt.Errorf("%w: tenant, release identity, and task kind are required", domain.ErrValidation)
	case release.Digest != "" && !validRunDigest(release.Digest):
		return fmt.Errorf("%w: release digest must be 64 hex characters", domain.ErrValidation)
	case !terminalRunStatus(run.Status):
		return fmt.Errorf("%w: run status %q is not terminal", domain.ErrValidation, run.Status)
	}
	if err := store.init(ctx); err != nil {
		return err
	}
	var request any
	if requestID != "" {
		request = requestID
	}
	result, err := store.qs.Query(ctx, qRecordAgentRun,
		tenantID, release.AgentID, release.Version, taskKind, request, string(run.Status),
		tenantID, release.AgentID, release.Version, release.Digest, release.Digest,
	)
	if err != nil {
		return fmt.Errorf("record agent run: %w", err)
	}
	if len(result.Rows) > 0 {
		return nil
	}
	if requestID != "" {
		recorded, err := store.qs.Query(ctx, qAgentRunByRequest, tenantID, requestID)
		if err != nil {
			return fmt.Errorf("read agent run of request %q: %w", requestID, err)
		}
		if len(recorded.Rows) > 0 {
			row := recorded.Rows[0]
			if common.AsString(row[0]) == release.AgentID && common.AsString(row[1]) == release.Version &&
				common.AsString(row[2]) == taskKind && common.AsString(row[3]) == string(run.Status) {
				return nil
			}
			return fmt.Errorf("%w: request %q already recorded another run", domain.ErrConflict, requestID)
		}
	}
	return fmt.Errorf("%w: agent release reference does not match", domain.ErrConflict)
}

func validRunDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// Runs returns one page of the tenant's settled runs, newest first.
func (store *AgentRunStore) Runs(ctx context.Context, tenantID int64, filter domain.AgentRunFilter) ([]domain.AgentRun, error) {
	switch {
	case tenantID <= 0:
		return nil, fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	case filter.Limit <= 0 || filter.Limit > MaxAgentRunPage:
		return nil, fmt.Errorf("%w: run page limit must be between 1 and %d", domain.ErrValidation, MaxAgentRunPage)
	case filter.Before < 0:
		return nil, fmt.Errorf("%w: run cursor cannot be negative", domain.ErrValidation)
	case filter.Status != "" && !terminalRunStatus(filter.Status):
		return nil, fmt.Errorf("%w: run status %q is not terminal", domain.ErrValidation, filter.Status)
	}
	if err := store.init(ctx); err != nil {
		return nil, err
	}
	agentID, requestID, status := strings.TrimSpace(filter.AgentID), strings.TrimSpace(filter.RequestID), string(filter.Status)
	result, err := store.qs.Query(ctx, qAgentRuns, tenantID, agentID, agentID, requestID, requestID, status, status,
		filter.Before, filter.Before, filter.Limit)
	if err != nil {
		return nil, fmt.Errorf("list agent runs: %w", err)
	}
	runs := make([]domain.AgentRun, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) < 7 {
			return nil, fmt.Errorf("%w: malformed agent run row", domain.ErrConflict)
		}
		completedAt, ok := common.AsTimeOK(row[6])
		if !ok {
			return nil, fmt.Errorf("%w: malformed agent run completion time", domain.ErrConflict)
		}
		runs = append(runs, domain.AgentRun{
			ID:          common.AsInt64(row[0]),
			Release:     domain.AgentReleaseReference{AgentID: common.AsString(row[1]), Version: common.AsString(row[2])},
			TaskKind:    common.AsString(row[3]),
			RequestID:   common.AsString(row[4]),
			Status:      domain.RunStatus(common.AsString(row[5])),
			CompletedAt: completedAt,
		})
	}
	return runs, nil
}

func terminalRunStatus(status domain.RunStatus) bool {
	return status == domain.RunCompleted || status == domain.RunFailed || status == domain.RunCancelled
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

// Purge deletes run activity past the injected retention horizon.
func (store *AgentRunStore) Purge(ctx context.Context, retentionDays, limit int) (int64, error) {
	if retentionDays < 0 {
		return 0, fmt.Errorf("%w: retention days cannot be negative", domain.ErrValidation)
	}
	if retentionDays == 0 {
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
