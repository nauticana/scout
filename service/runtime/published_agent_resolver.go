package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const qPublishedAgent = "scout_runtime_published_agent"

var publishedAgentQueries = map[string]string{
	qPublishedAgent: `
SELECT p.is_active, dep.stable_version, dep.canary_version, dep.canary_percentage,
       stable.definition, canary.definition
  FROM agent_alias a
  JOIN agent_profile p
    ON p.tenant_id = a.tenant_id AND p.agent_kind = a.agent_kind AND p.agent_id = a.agent_id
  JOIN agent_deployment dep ON dep.tenant_id = p.tenant_id AND dep.agent_id = p.agent_id
  JOIN agent_version stable
    ON stable.tenant_id = dep.tenant_id AND stable.agent_id = dep.agent_id AND stable.agent_version = dep.stable_version
  LEFT JOIN agent_version canary
    ON canary.tenant_id = dep.tenant_id AND canary.agent_id = dep.agent_id AND canary.agent_version = dep.canary_version
 WHERE a.tenant_id = ? AND a.alias_id = ?`,
}

// PublishedAgentResolver resolves one active alias to an immutable definition.
type PublishedAgentResolver struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

func (resolver *PublishedAgentResolver) init(ctx context.Context) error {
	if resolver.qs != nil {
		return nil
	}
	if resolver.DB == nil {
		return fmt.Errorf("published agent resolver: database is required")
	}
	resolver.once.Do(func() { resolver.qs = resolver.DB.GetQueryService(ctx, publishedAgentQueries) })
	return nil
}

func (resolver *PublishedAgentResolver) Resolve(ctx context.Context, tenantID int64, aliasID, languageCode, conversationID string) (domain.AgentDefinition, error) {
	if tenantID <= 0 || strings.TrimSpace(aliasID) == "" {
		return domain.AgentDefinition{}, fmt.Errorf("%w: tenant and alias are required", domain.ErrValidation)
	}
	if err := resolver.init(ctx); err != nil {
		return domain.AgentDefinition{}, err
	}
	result, err := resolver.qs.Query(ctx, qPublishedAgent, tenantID, aliasID)
	if err != nil {
		return domain.AgentDefinition{}, fmt.Errorf("resolve published agent: %w", err)
	}
	if len(result.Rows) == 0 || !common.AsBool(result.Rows[0][0]) {
		return domain.AgentDefinition{}, domain.ErrNotReady
	}
	row := result.Rows[0]
	version := common.AsString(row[1])
	encoded := row[4]
	canaryVersion := common.AsString(row[2])
	if canaryVersion != "" && row[5] != nil && canarySelected(tenantID, aliasID, conversationID, int(common.AsInt64(row[3]))) {
		version = canaryVersion
		encoded = row[5]
	}
	var definition domain.AgentDefinition
	if err := json.Unmarshal([]byte(common.AsString(encoded)), &definition); err != nil {
		return domain.AgentDefinition{}, fmt.Errorf("decode published agent: %w", err)
	}
	if definition.Version != version || definition.AgentID == "" || definition.DefinitionDigest == "" {
		return domain.AgentDefinition{}, fmt.Errorf("%w: published definition identity mismatch", domain.ErrConflict)
	}
	if languageCode != "" && !definitionHasLanguage(definition, languageCode) {
		return domain.AgentDefinition{}, domain.ErrNotReady
	}
	return definition, nil
}

func canarySelected(tenantID int64, aliasID, conversationID string, percentage int) bool {
	if percentage <= 0 || conversationID == "" {
		return false
	}
	if percentage >= 100 {
		return true
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s", tenantID, aliasID, conversationID)))
	return int(binary.BigEndian.Uint64(digest[:8])%100) < percentage
}

func definitionHasLanguage(definition domain.AgentDefinition, languageCode string) bool {
	for _, language := range definition.Languages {
		if language.LanguageCode == languageCode {
			return true
		}
	}
	return false
}

var _ contract.PublishedAgentResolver = (*PublishedAgentResolver)(nil)
