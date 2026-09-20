package controlplane

import (
	"context"
	"fmt"
	"sync"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const qDefinitionGet = "scout_agent_definition_get"

var definitionReaderQueries = map[string]string{
	qDefinitionGet: `
SELECT definition
  FROM agent_version
 WHERE tenant_id = ? AND agent_id = ? AND agent_version = ?`,
}

// TableAgentDefinitionReader reads immutable definitions from agent_version.
type TableAgentDefinitionReader struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

func (reader *TableAgentDefinitionReader) Get(ctx context.Context, tenantID int64, agentID, version string) (domain.AgentDefinition, error) {
	if reader.DB == nil {
		return domain.AgentDefinition{}, fmt.Errorf("agent definition reader: database is required")
	}
	reader.once.Do(func() { reader.qs = reader.DB.GetQueryService(ctx, definitionReaderQueries) })
	result, err := reader.qs.Query(ctx, qDefinitionGet, tenantID, agentID, version)
	if err != nil {
		return domain.AgentDefinition{}, fmt.Errorf("read agent definition: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.AgentDefinition{}, fmt.Errorf("%w: agent %s@%s", domain.ErrNotFound, agentID, version)
	}
	return decodeDefinition(result.Rows[0][0])
}

var _ contract.AgentDefinitionReader = (*TableAgentDefinitionReader)(nil)
