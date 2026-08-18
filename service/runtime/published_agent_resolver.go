package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/release"
)

const (
	qPublishedAgent        = "scout_runtime_published_agent"
	qPublishedAgentVersion = "scout_runtime_published_agent_version"
)

var publishedAgentQueries = map[string]string{
	qPublishedAgent: `
SELECT p.state_code = 'active', dep.stable_version, dep.canary_version, dep.canary_percentage,
       stable.definition, canary.definition, p.agent_id
  FROM agent_alias a
  JOIN agent_profile p
    ON p.tenant_id = a.tenant_id AND p.agent_type_id = a.agent_type_id AND p.agent_id = a.agent_id
  JOIN agent_deployment dep ON dep.tenant_id = p.tenant_id AND dep.agent_id = p.agent_id
  JOIN agent_version stable
    ON stable.tenant_id = dep.tenant_id AND stable.agent_id = dep.agent_id AND stable.agent_version = dep.stable_version
  LEFT JOIN agent_version canary
    ON canary.tenant_id = dep.tenant_id AND canary.agent_id = dep.agent_id AND canary.agent_version = dep.canary_version
 WHERE a.tenant_id = ? AND a.alias_id = ?`,
	qPublishedAgentVersion: `
SELECT definition
  FROM agent_version
 WHERE tenant_id = ? AND agent_id = ? AND agent_version = ?`,
}

// PublishedAgentResolver resolves one active alias to an immutable definition.
// Versions, when set, owns the version choice (pins, cohorts, canary); without
// it the resolver applies the reference stable/canary hash directly.
type PublishedAgentResolver struct {
	DB       keelport.DatabaseRepository
	Versions contract.AgentVersionTrafficManager

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
	version, encoded, err := resolver.selectVersion(ctx, tenantID, aliasID, conversationID, row)
	if err != nil {
		return domain.AgentDefinition{}, err
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

// selectVersion delegates to the traffic manager when one is configured and
// otherwise applies the reference stable/canary split; a version outside the
// deployment is loaded on demand.
func (resolver *PublishedAgentResolver) selectVersion(ctx context.Context, tenantID int64, aliasID, conversationID string, row []any) (string, any, error) {
	stableVersion, canaryVersion := common.AsString(row[1]), common.AsString(row[2])
	if resolver.Versions == nil {
		if canaryVersion != "" && row[5] != nil && release.CanarySelected(tenantID, aliasID, conversationID, int(common.AsInt64(row[3]))) {
			return canaryVersion, row[5], nil
		}
		return stableVersion, row[4], nil
	}
	version, err := resolver.Versions.ResolveVersion(ctx, tenantID, common.AsString(row[6]), conversationID)
	if err != nil {
		return "", nil, fmt.Errorf("resolve agent version: %w", err)
	}
	switch version {
	case stableVersion:
		return version, row[4], nil
	case canaryVersion:
		if row[5] == nil {
			return "", nil, fmt.Errorf("%w: canary version %s has no definition", domain.ErrNotReady, version)
		}
		return version, row[5], nil
	}
	pinned, err := resolver.qs.Query(ctx, qPublishedAgentVersion, tenantID, common.AsString(row[6]), version)
	if err != nil {
		return "", nil, fmt.Errorf("load agent version %s: %w", version, err)
	}
	if len(pinned.Rows) == 0 {
		return "", nil, fmt.Errorf("%w: agent version %s", domain.ErrNotFound, version)
	}
	return version, pinned.Rows[0][0], nil
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
