package release

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const qPilotInForce = "scout_release_pilot_in_force"

// DefaultPilotPositionTimeout bounds position resolution when none is configured.
const DefaultPilotPositionTimeout = 2 * time.Second

// One read per new conversation: the person's own membership and every position membership,
// which the open-membership cap bounds. A quarantined release is never served.
var pilotResolverQueries = map[string]string{
	qPilotInForce: `
SELECT m.subject_kind, m.subject_id, m.agent_version, m.reason, m.added_by_kind, m.added_by_id, m.begda, m.endda
  FROM agent_pilot_member m
 WHERE m.partner_id = ? AND m.agent_id = ? AND m.begda <= ? AND (m.endda IS NULL OR m.endda > ?)
   AND ((m.subject_kind = '` + string(domain.PilotPrincipal) + `' AND m.subject_id = ?) OR m.subject_kind = '` + string(domain.PilotPosition) + `')
   AND NOT EXISTS (SELECT 1 FROM agent_version_quarantine q
                    WHERE q.partner_id = m.partner_id AND q.agent_id = m.agent_id
                      AND q.agent_version = m.agent_version AND q.lifted_at IS NULL)
 ORDER BY CASE m.subject_kind WHEN '` + string(domain.PilotPrincipal) + `' THEN 0 ELSE 1 END, m.begda DESC, m.subject_id`,
}

// TablePilotResolver reads agent_pilot_member. A person's own membership wins; otherwise the
// newest membership of a position the product says they hold, so the product is asked only
// when the agent has a position membership in force.
type TablePilotResolver struct {
	DB keelport.DatabaseRepository
	// Positions names a person's positions; nil leaves position memberships without effect.
	Positions contract.PositionResolver
	// PositionTimeout bounds one Positions call. Zero uses DefaultPilotPositionTimeout.
	PositionTimeout time.Duration

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.PilotResolver = (*TablePilotResolver)(nil)

func (resolver *TablePilotResolver) init(ctx context.Context) error {
	if resolver.DB == nil {
		return fmt.Errorf("pilot resolver: database is required")
	}
	resolver.once.Do(func() { resolver.qs = resolver.DB.GetQueryService(ctx, pilotResolverQueries) })
	if resolver.qs == nil {
		return fmt.Errorf("pilot resolver: query service is required")
	}
	return nil
}

func (resolver *TablePilotResolver) Resolve(ctx context.Context, tenantID int64, agentID string, person domain.PrincipalRef, at time.Time) (domain.PilotMember, bool, error) {
	if person.Kind != domain.PrincipalHuman || person.ID == "" {
		return domain.PilotMember{}, false, nil
	}
	if err := resolver.init(ctx); err != nil {
		return domain.PilotMember{}, false, err
	}
	result, err := resolver.qs.Query(ctx, qPilotInForce, tenantID, agentID, at, at, person.ID)
	if err != nil {
		return domain.PilotMember{}, false, fmt.Errorf("read pilot memberships: %w", err)
	}
	var positions []domain.PilotMember
	for _, row := range result.Rows {
		member := pilotMember(tenantID, agentID, row)
		if member.Subject.Kind == domain.PilotPrincipal {
			return member, true, nil
		}
		positions = append(positions, member)
	}
	if len(positions) == 0 || resolver.Positions == nil {
		return domain.PilotMember{}, false, nil
	}
	held, err := resolver.positions(ctx, tenantID, person, at)
	if err != nil {
		return domain.PilotMember{}, false, fmt.Errorf("resolve positions of person %s: %w", person.ID, err)
	}
	for _, member := range positions {
		if slices.Contains(held, member.Subject.ID) {
			return member, true, nil
		}
	}
	return domain.PilotMember{}, false, nil
}

func (resolver *TablePilotResolver) positions(ctx context.Context, tenantID int64, person domain.PrincipalRef, at time.Time) ([]string, error) {
	bounded, cancel := context.WithTimeout(ctx, resolver.positionTimeout())
	defer cancel()
	held, err := resolver.Positions.Positions(bounded, tenantID, person, at)
	if err == nil && bounded.Err() != nil {
		err = bounded.Err()
	}
	return held, err
}

func (resolver *TablePilotResolver) positionTimeout() time.Duration {
	if resolver.PositionTimeout > 0 {
		return resolver.PositionTimeout
	}
	return DefaultPilotPositionTimeout
}

// pilotMember decodes subject_kind, subject_id, agent_version, reason, added_by_kind,
// added_by_id, begda, endda.
func pilotMember(tenantID int64, agentID string, row []any) domain.PilotMember {
	return domain.PilotMember{
		TenantID: tenantID, AgentID: agentID, Version: common.AsString(row[2]), Reason: common.AsString(row[3]),
		Subject:   domain.PilotSubject{Kind: domain.PilotSubjectKind(common.AsString(row[0])), ID: common.AsString(row[1])},
		AddedBy:   domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[4])), ID: common.AsString(row[5])},
		ValidFrom: common.AsTime(row[6]), ValidTo: common.AsTime(row[7]),
	}
}
