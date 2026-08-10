package runtime

import (
	"context"
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
	qRecordAgentRun = "scout_runtime_record_agent_run"
	qAgentLastRun   = "scout_runtime_agent_last_run"
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
}

// AgentRunStore records successful release executions and reports their newest
// completion per agent.
type AgentRunStore struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.AgentRunRecorder = (*AgentRunStore)(nil)
var _ contract.AgentActivityReporter = (*AgentRunStore)(nil)

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
