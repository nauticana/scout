package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const qRecordAgentOpsEvent = "scout_runtime_record_agent_ops_event"

var agentOpsEventQueries = map[string]string{
	qRecordAgentOpsEvent: `
INSERT INTO agent_ops_event (id, tenant_id, event, detail)
VALUES (nextval('agent_ops_event_seq'), ?, ?, ?)`,
}

// AgentOpsEventStore persists tenant-scoped operational events, including
// failures that occur before an agent profile has been provisioned.
type AgentOpsEventStore struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.AgentOperationalEventRecorder = (*AgentOpsEventStore)(nil)

func (store *AgentOpsEventStore) init(ctx context.Context) error {
	if store.qs != nil {
		return nil
	}
	if store.DB == nil {
		return fmt.Errorf("agent operations event store: database is required")
	}
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, agentOpsEventQueries) })
	return nil
}

// RecordOperationalEvent persists one event. Event names are open so products
// can contribute operational categories without changing Scout.
func (store *AgentOpsEventStore) RecordOperationalEvent(ctx context.Context, tenantID int64, event, detail string) error {
	event = strings.TrimSpace(event)
	detail = strings.TrimSpace(detail)
	if tenantID <= 0 || event == "" || len(event) > 80 {
		return fmt.Errorf("%w: tenant and an event name of at most 80 characters are required", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return err
	}
	var optionalDetail any
	if detail != "" {
		optionalDetail = detail
	}
	if _, err := store.qs.Query(ctx, qRecordAgentOpsEvent, tenantID, event, optionalDetail); err != nil {
		return fmt.Errorf("record agent operational event: %w", err)
	}
	return nil
}
