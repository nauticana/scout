package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const qDeployedAgents = "scout_runtime_deployed_agents"

var deployedAgentQueries = map[string]string{
	qDeployedAgents: `
SELECT a.alias_id, a.agent_type_id, a.agent_id, p.state_code = 'active', d.enabled,
       dep.stable_version, v.definition
  FROM agent_alias a
  JOIN agent_profile p
    ON p.tenant_id = a.tenant_id AND p.agent_type_id = a.agent_type_id AND p.agent_id = a.agent_id
  JOIN agent_draft d ON d.tenant_id = p.tenant_id AND d.agent_id = p.agent_id
  LEFT JOIN agent_deployment dep ON dep.tenant_id = p.tenant_id AND dep.agent_id = p.agent_id
  LEFT JOIN agent_version v
    ON v.tenant_id = dep.tenant_id AND v.agent_id = dep.agent_id AND v.agent_version = dep.stable_version
 WHERE a.tenant_id = ?`,
}

// DeployedAgentIndex lists every alias a tenant owns with its operational
// state and deployed definition.
type DeployedAgentIndex struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.DeployedAgentIndex = (*DeployedAgentIndex)(nil)

func (index *DeployedAgentIndex) List(ctx context.Context, tenantID int64) (map[string]domain.DeployedAgent, error) {
	if tenantID <= 0 {
		return nil, fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	if index.qs == nil {
		if index.DB == nil {
			return nil, fmt.Errorf("deployed agent index: database is required")
		}
		index.once.Do(func() { index.qs = index.DB.GetQueryService(ctx, deployedAgentQueries) })
	}
	res, err := index.qs.Query(ctx, qDeployedAgents, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list deployed agents: %w", err)
	}
	deployed := make(map[string]domain.DeployedAgent, len(res.Rows))
	for _, row := range res.Rows {
		agent := domain.DeployedAgent{
			AliasID: common.AsString(row[0]), AgentTypeID: common.AsString(row[1]),
			AgentID: common.AsString(row[2]), Active: common.AsBool(row[3]),
			Enabled: common.AsBool(row[4]), Version: common.AsString(row[5]),
		}
		if agent.Version != "" && row[6] != nil {
			var definition domain.AgentDefinition
			if err := json.Unmarshal([]byte(common.AsString(row[6])), &definition); err != nil {
				return nil, fmt.Errorf("decode deployed definition for %q: %w", agent.AgentID, err)
			}
			agent.Definition = &definition
		}
		deployed[agent.AliasID] = agent
	}
	return deployed, nil
}
