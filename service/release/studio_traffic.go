package release

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/clk"
)

const (
	qTrafficConversation = "scout_release_traffic_conversation"
	qTrafficEvent        = "scout_release_traffic_event"
)

var trafficQueries = map[string]string{
	qTrafficConversation: `
SELECT c.agent_id, COALESCE(c.alias_id, ''), c.agent_version, c.version_source, COALESCE(c.pin_reason, ''),
       v.definition_digest, c.created_at
  FROM agent_conversation c
  JOIN agent_version v ON v.partner_id = c.partner_id AND v.agent_id = c.agent_id AND v.agent_version = c.agent_version
 WHERE c.partner_id = ? AND c.conversation_id = ?`,
	qTrafficEvent: `
INSERT INTO agent_studio_event (id, partner_id, agent_id, event, detail, principal_kind, principal_id)
VALUES (NEXTVAL('agent_studio_event_seq'), ?, ?, ?, ?, ?, ?)`,
}

// StudioTraffic is the Studio surface over an agent's traffic policy: canary, promote,
// and rollback change only which release new conversations take; a live conversation
// stays on the release it recorded, which Explain reports with the rule that chose it.
// Where Separation requires reviewing restores, traffic moves only between the stable
// release and the current canary; returning any other version to service is a reviewed restore.
type StudioTraffic struct {
	DB          keelport.DatabaseRepository
	Authorizer  contract.StudioAuthorizer
	Traffic     contract.AgentVersionTrafficManager
	Deployments contract.AgentDeploymentStore
	Separation  contract.PublishSeparation
	Now         func() time.Time

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.StudioTrafficBackend = (*StudioTraffic)(nil)

func (s *StudioTraffic) init(ctx context.Context) error {
	if s.DB == nil || s.Authorizer == nil || s.Traffic == nil || s.Deployments == nil || s.Separation == nil {
		return fmt.Errorf("studio traffic: database, authorizer, traffic manager, deployment store, and publish separation are required")
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, trafficQueries) })
	return nil
}

// Canary gives version a percentage of new conversations beside the stable release.
func (s *StudioTraffic) Canary(ctx context.Context, actor domain.StudioActor, agentID, version string, percentage int) (domain.AgentDeployment, error) {
	return s.change(ctx, actor, agentID, "CANARY", fmt.Sprintf("version %s at %d%%", version, percentage), func() error {
		return s.Traffic.SetCanary(ctx, actor.TenantID, agentID, version, percentage)
	}, func(reviewed contract.ReviewedAgentVersionTrafficManager) error {
		return reviewed.ReweightCanary(ctx, actor.TenantID, agentID, version, percentage)
	})
}

// Promote makes version the stable release and ends any canary.
func (s *StudioTraffic) Promote(ctx context.Context, actor domain.StudioActor, agentID, version string) (domain.AgentDeployment, error) {
	return s.change(ctx, actor, agentID, "PROMOTE", "version "+version, func() error {
		return s.Traffic.Promote(ctx, actor.TenantID, agentID, version)
	}, func(reviewed contract.ReviewedAgentVersionTrafficManager) error {
		return reviewed.PromoteDeployed(ctx, actor.TenantID, agentID, version)
	})
}

// Rollback ends a canary, or else returns to the release before the stable one.
func (s *StudioTraffic) Rollback(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentDeployment, error) {
	return s.change(ctx, actor, agentID, "ROLLBACK", "", func() error {
		return s.Traffic.Rollback(ctx, actor.TenantID, agentID)
	}, func(reviewed contract.ReviewedAgentVersionTrafficManager) error {
		_, err := reviewed.EndCanary(ctx, actor.TenantID, agentID)
		return err
	})
}

func (s *StudioTraffic) change(ctx context.Context, actor domain.StudioActor, agentID, event, detail string, apply func() error, applyReviewed func(contract.ReviewedAgentVersionTrafficManager) error) (domain.AgentDeployment, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentDeployment{}, err
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioPublish, agentID); err != nil {
		return domain.AgentDeployment{}, err
	}
	reviewed, err := s.Separation.Required(ctx, actor.TenantID, agentID, domain.PublishReviewOfRestore, s.now())
	if err != nil {
		return domain.AgentDeployment{}, fmt.Errorf("resolve restore separation: %w", err)
	}
	if reviewed {
		traffic, ok := s.Traffic.(contract.ReviewedAgentVersionTrafficManager)
		if !ok {
			return domain.AgentDeployment{}, fmt.Errorf("%w: traffic manager does not support reviewed traffic changes", domain.ErrNotReady)
		}
		if err = applyReviewed(traffic); errors.Is(err, domain.ErrConflict) {
			err = fmt.Errorf("%w; restores of agent %q are reviewed, so returning another release to service takes an approved restore review and a restore", err, agentID)
		}
	} else {
		err = apply()
	}
	if err != nil {
		return domain.AgentDeployment{}, err
	}
	deployment, err := s.Deployments.Get(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentDeployment{}, err
	}
	if detail == "" {
		detail = "stable " + deployment.StableVersion
	}
	if _, err = s.qs.Query(ctx, qTrafficEvent, actor.TenantID, agentID, event, detail,
		string(actor.Principal.Kind), actor.Principal.ID); err != nil {
		return deployment, fmt.Errorf("record traffic event: %w", err)
	}
	return deployment, nil
}

func (s *StudioTraffic) now() time.Time {
	return clk.Now(s.Now)
}

// Explain reports the release a conversation runs on, why it was chosen, and its digest.
func (s *StudioTraffic) Explain(ctx context.Context, actor domain.StudioActor, conversationID string) (domain.ConversationVersion, error) {
	if err := s.init(ctx); err != nil {
		return domain.ConversationVersion{}, err
	}
	if strings.TrimSpace(conversationID) == "" {
		return domain.ConversationVersion{}, fmt.Errorf("%w: conversation_id is required", domain.ErrValidation)
	}
	result, err := s.qs.Query(ctx, qTrafficConversation, actor.TenantID, conversationID)
	if err != nil {
		return domain.ConversationVersion{}, fmt.Errorf("load conversation release: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.ConversationVersion{}, fmt.Errorf("%w: conversation %q", domain.ErrNotFound, conversationID)
	}
	row := result.Rows[0]
	explained := domain.ConversationVersion{
		ConversationID: conversationID, AgentID: common.AsString(row[0]), AliasID: common.AsString(row[1]),
		Version: common.AsString(row[2]), Source: domain.VersionSource(common.AsString(row[3])),
		PinReason: common.AsString(row[4]), DefinitionDigest: common.AsString(row[5]), StartedAt: common.AsTime(row[6]),
	}
	if err = s.Authorizer.Authorize(ctx, actor, domain.StudioView, explained.AgentID); err != nil {
		return domain.ConversationVersion{}, err
	}
	return explained, nil
}
