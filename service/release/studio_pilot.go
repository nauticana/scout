package release

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qPilotLock    = "scout_release_pilot_lock"
	qPilotList    = "scout_release_pilot_list"
	qPilotVersion = "scout_release_pilot_version"
	qPilotOpen    = "scout_release_pilot_open"
	qPilotCount   = "scout_release_pilot_count"
	qPilotInsert  = "scout_release_pilot_insert"
	qPilotDrop    = "scout_release_pilot_drop"
	qPilotEnd     = "scout_release_pilot_end"
	qPilotEvent   = "scout_release_pilot_event"
)

// Column widths of agent_pilot_member and agent_studio_event.detail.
const (
	pilotIDLimit     = 80
	pilotSubjectSize = 200
	pilotReasonSize  = 400
	pilotDetailSize  = 400
)

var studioPilotQueries = map[string]string{
	qPilotLock: `SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`,
	qPilotList: `
SELECT subject_kind, subject_id, agent_version, reason, added_by_kind, added_by_id, begda, endda
  FROM agent_pilot_member
 WHERE partner_id = ? AND agent_id = ? AND endda IS NULL
 ORDER BY subject_kind, subject_id`,
	qPilotVersion: `
SELECT EXISTS (SELECT 1 FROM agent_version_quarantine q
                WHERE q.partner_id = v.partner_id AND q.agent_id = v.agent_id
                  AND q.agent_version = v.agent_version AND q.lifted_at IS NULL)
  FROM agent_version v
 WHERE v.partner_id = ? AND v.agent_id = ? AND v.agent_version = ?`,
	qPilotOpen: `
SELECT subject_kind, subject_id, agent_version, reason, added_by_kind, added_by_id, begda, endda
  FROM agent_pilot_member
 WHERE partner_id = ? AND agent_id = ? AND subject_kind = ? AND subject_id = ? AND endda IS NULL`,
	qPilotCount: `
SELECT COUNT(*) FROM agent_pilot_member WHERE partner_id = ? AND agent_id = ? AND endda IS NULL`,
	qPilotInsert: `
INSERT INTO agent_pilot_member (partner_id, agent_id, subject_kind, subject_id, agent_version, reason, added_by_kind, added_by_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING begda`,
	// A membership added at or after this transaction's instant was never in force, so it is
	// dropped rather than ended before its own begda.
	qPilotDrop: `
DELETE FROM agent_pilot_member
 WHERE partner_id = ? AND agent_id = ? AND subject_kind = ? AND subject_id = ? AND agent_version = ?
   AND endda IS NULL AND begda >= CURRENT_TIMESTAMP
RETURNING subject_kind, subject_id, agent_version, reason, added_by_kind, added_by_id, begda, begda`,
	qPilotEnd: `
UPDATE agent_pilot_member SET endda = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND agent_id = ? AND subject_kind = ? AND subject_id = ? AND agent_version = ?
   AND endda IS NULL
RETURNING subject_kind, subject_id, agent_version, reason, added_by_kind, added_by_id, begda, endda`,
	qPilotEvent: `
INSERT INTO agent_studio_event (id, partner_id, agent_id, event, detail, principal_kind, principal_id)
VALUES (NEXTVAL('agent_studio_event_seq'), ?, ?, ?, ?, ?, ?)`,
}

// StudioPilots is the Studio surface over an agent's supervised pilots: a published release
// served to listed people and positions ahead of experiment cohorts and the canary. A change
// affects only the release new conversations take, and every change records a Studio event.
type StudioPilots struct {
	DB         keelport.DatabaseRepository
	Authorizer contract.StudioAuthorizer
	// MaxMembers caps an agent's open memberships, which bounds what resolving a new
	// conversation reads; 0 refuses every add.
	MaxMembers int

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.StudioPilotBackend = (*StudioPilots)(nil)

func (s *StudioPilots) init(ctx context.Context) error {
	if s.DB == nil || s.Authorizer == nil || s.MaxMembers < 0 {
		return fmt.Errorf("studio pilots: database and authorizer are required and the member cap is non-negative")
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, studioPilotQueries) })
	if s.qs == nil {
		return fmt.Errorf("studio pilots: query service is required")
	}
	return nil
}

// List returns the agent's open memberships.
func (s *StudioPilots) List(ctx context.Context, actor domain.StudioActor, agentID string) ([]domain.PilotMember, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(agentID) == "" {
		return nil, fmt.Errorf("%w: agent_name is required", domain.ErrValidation)
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return nil, err
	}
	result, err := s.qs.Query(ctx, qPilotList, actor.TenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("list pilot memberships: %w", err)
	}
	members := make([]domain.PilotMember, 0, len(result.Rows))
	for _, row := range result.Rows {
		members = append(members, pilotMember(actor.TenantID, agentID, row))
	}
	return members, nil
}

// Add serves change.Version to the subject. Repeating an open membership returns it
// unchanged; a subject already piloting another version is a conflict until removed.
func (s *StudioPilots) Add(ctx context.Context, actor domain.StudioActor, change domain.PilotChange) (domain.PilotMember, error) {
	change, err := s.prepare(ctx, actor, change)
	if err != nil {
		return domain.PilotMember{}, err
	}
	tx, err := s.begin(ctx, actor, change.AgentID)
	if err != nil {
		return domain.PilotMember{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if err = pilotReleasable(ctx, tx, actor.TenantID, change); err != nil {
		return domain.PilotMember{}, err
	}
	open, err := tx.Query(ctx, qPilotOpen, actor.TenantID, change.AgentID, string(change.Subject.Kind), change.Subject.ID)
	if err != nil {
		return domain.PilotMember{}, fmt.Errorf("read open pilot membership: %w", err)
	}
	if len(open.Rows) > 0 {
		existing := pilotMember(actor.TenantID, change.AgentID, open.Rows[0])
		if existing.Version != change.Version {
			return domain.PilotMember{}, fmt.Errorf("%w: %s %s already pilots version %s of agent %q; remove it first",
				domain.ErrConflict, change.Subject.Kind, change.Subject.ID, existing.Version, change.AgentID)
		}
		return existing, nil
	}
	census, err := tx.Query(ctx, qPilotCount, actor.TenantID, change.AgentID)
	if err != nil {
		return domain.PilotMember{}, fmt.Errorf("count pilot memberships: %w", err)
	}
	if len(census.Rows) == 0 {
		return domain.PilotMember{}, fmt.Errorf("count pilot memberships: no row")
	}
	if count := common.AsInt64(census.Rows[0][0]); count >= int64(s.MaxMembers) {
		return domain.PilotMember{}, fmt.Errorf("%w: agent %q holds %d open pilot memberships, the most allowed is %d",
			domain.ErrConflict, change.AgentID, count, s.MaxMembers)
	}
	inserted, err := tx.Query(ctx, qPilotInsert, actor.TenantID, change.AgentID, string(change.Subject.Kind), change.Subject.ID,
		change.Version, change.Reason, string(actor.Principal.Kind), actor.Principal.ID)
	if err != nil {
		return domain.PilotMember{}, fmt.Errorf("add pilot membership: %w", err)
	}
	if len(inserted.Rows) == 0 {
		return domain.PilotMember{}, fmt.Errorf("add pilot membership: no begda returned")
	}
	member := domain.PilotMember{
		TenantID: actor.TenantID, AgentID: change.AgentID, Version: change.Version, Subject: change.Subject,
		Reason: change.Reason, AddedBy: actor.Principal, ValidFrom: common.AsTime(inserted.Rows[0][0]),
	}
	if err = recordPilotEvent(ctx, tx, actor, "PILOT_ADD", change); err != nil {
		return domain.PilotMember{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.PilotMember{}, fmt.Errorf("commit pilot membership: %w", err)
	}
	committed = true
	return member, nil
}

// Remove ends the subject's open membership of change.Version.
func (s *StudioPilots) Remove(ctx context.Context, actor domain.StudioActor, change domain.PilotChange) (domain.PilotMember, error) {
	change, err := s.prepare(ctx, actor, change)
	if err != nil {
		return domain.PilotMember{}, err
	}
	tx, err := s.begin(ctx, actor, change.AgentID)
	if err != nil {
		return domain.PilotMember{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	key := []any{actor.TenantID, change.AgentID, string(change.Subject.Kind), change.Subject.ID, change.Version}
	removed, err := tx.Query(ctx, qPilotDrop, key...)
	if err != nil {
		return domain.PilotMember{}, fmt.Errorf("drop pilot membership: %w", err)
	}
	if len(removed.Rows) == 0 {
		if removed, err = tx.Query(ctx, qPilotEnd, key...); err != nil {
			return domain.PilotMember{}, fmt.Errorf("end pilot membership: %w", err)
		}
	}
	if len(removed.Rows) == 0 {
		return domain.PilotMember{}, fmt.Errorf("%w: %s %s has no open pilot membership of version %s of agent %q",
			domain.ErrNotFound, change.Subject.Kind, change.Subject.ID, change.Version, change.AgentID)
	}
	member := pilotMember(actor.TenantID, change.AgentID, removed.Rows[0])
	if err = recordPilotEvent(ctx, tx, actor, "PILOT_REMOVE", change); err != nil {
		return domain.PilotMember{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.PilotMember{}, fmt.Errorf("commit pilot removal: %w", err)
	}
	committed = true
	return member, nil
}

// prepare validates a change and authorizes PUBLISH on its agent before anything is read.
func (s *StudioPilots) prepare(ctx context.Context, actor domain.StudioActor, change domain.PilotChange) (domain.PilotChange, error) {
	if err := s.init(ctx); err != nil {
		return domain.PilotChange{}, err
	}
	change.Subject.ID = strings.TrimSpace(change.Subject.ID)
	change.Reason = strings.TrimSpace(change.Reason)
	if err := validatePilotChange(change); err != nil {
		return domain.PilotChange{}, err
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioPublish, change.AgentID); err != nil {
		return domain.PilotChange{}, err
	}
	return change, nil
}

// begin opens the change's transaction under a per-agent lock, so the open-membership check,
// the cap, and the write cannot race another change of the same agent.
func (s *StudioPilots) begin(ctx context.Context, actor domain.StudioActor, agentID string) (keelport.TxQueryService, error) {
	tx, err := s.DB.BeginTx(ctx, studioPilotQueries)
	if err != nil {
		return nil, fmt.Errorf("begin pilot change: %w", err)
	}
	if _, err = tx.Query(ctx, qPilotLock, fmt.Sprintf("agent_pilot:%d:%s", actor.TenantID, agentID)); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("lock agent pilots: %w", err)
	}
	return tx, nil
}

// pilotReleasable refuses a version the tenant's agent never published and one that is quarantined.
func pilotReleasable(ctx context.Context, tx keelport.TxQueryService, tenantID int64, change domain.PilotChange) error {
	version, err := tx.Query(ctx, qPilotVersion, tenantID, change.AgentID, change.Version)
	if err != nil {
		return fmt.Errorf("read pilot release: %w", err)
	}
	if len(version.Rows) == 0 {
		return fmt.Errorf("%w: agent %q has no release %q", domain.ErrNotFound, change.AgentID, change.Version)
	}
	if common.AsBool(version.Rows[0][0]) {
		return fmt.Errorf("%w: release %q of agent %q is quarantined", domain.ErrConflict, change.Version, change.AgentID)
	}
	return nil
}

func recordPilotEvent(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, event string, change domain.PilotChange) error {
	detail := common.TruncateRunes(fmt.Sprintf("%s %s on version %s: %s", change.Subject.Kind, change.Subject.ID, change.Version, change.Reason), pilotDetailSize)
	if _, err := tx.Query(ctx, qPilotEvent, actor.TenantID, change.AgentID, event, detail,
		string(actor.Principal.Kind), actor.Principal.ID); err != nil {
		return fmt.Errorf("record pilot event: %w", err)
	}
	return nil
}

// validatePilotChange reports every invalid field of a change at once.
func validatePilotChange(change domain.PilotChange) error {
	var problems []string
	if strings.TrimSpace(change.AgentID) == "" || len(change.AgentID) > pilotIDLimit {
		problems = append(problems, fmt.Sprintf("agent_name must contain 1 to %d bytes", pilotIDLimit))
	}
	if strings.TrimSpace(change.Version) == "" || len(change.Version) > pilotIDLimit {
		problems = append(problems, fmt.Sprintf("version must contain 1 to %d bytes", pilotIDLimit))
	}
	switch change.Subject.Kind {
	case domain.PilotPrincipal:
		if id, err := strconv.ParseInt(change.Subject.ID, 10, 64); err != nil || id <= 0 || strconv.FormatInt(id, 10) != change.Subject.ID {
			problems = append(problems, "subject_id of a principal must be a person's decimal user id")
		}
	case domain.PilotPosition:
		if change.Subject.ID == "" || len(change.Subject.ID) > pilotSubjectSize {
			problems = append(problems, fmt.Sprintf("subject_id of a position must contain 1 to %d bytes", pilotSubjectSize))
		}
	default:
		problems = append(problems, "subject_kind must be P (principal) or O (position)")
	}
	if change.Reason == "" || utf8.RuneCountInString(change.Reason) > pilotReasonSize {
		problems = append(problems, fmt.Sprintf("reason must contain 1 to %d characters", pilotReasonSize))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", domain.ErrValidation, strings.Join(problems, "; "))
	}
	return nil
}
