package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qGovernanceAgent = "scout_governance_agent"
	qGovernanceGet   = "scout_governance_get"
	qGovernancePut   = "scout_governance_put"
)

var governanceQueries = map[string]string{
	qGovernanceAgent: `
SELECT p.state_code, d.draft_revision
  FROM agent_profile p
  JOIN agent_draft d ON d.partner_id = p.partner_id AND d.agent_id = p.agent_id
 WHERE p.partner_id = ? AND p.agent_id = ?`,
	qGovernanceGet: `SELECT governance FROM agent_governance WHERE partner_id = ? AND agent_id = ?`,
	qGovernancePut: `
INSERT INTO agent_governance (partner_id, agent_id, governance, modified_by)
VALUES (?, ?, ?, ?)
ON CONFLICT (partner_id, agent_id) DO UPDATE
SET governance = EXCLUDED.governance, modified_by = EXCLUDED.modified_by, modified_at = CURRENT_TIMESTAMP`,
	qStudioBumpDraft: studioQueries[qStudioBumpDraft],
	qStudioAudit:     studioQueries[qStudioAudit],
}

// StudioGovernance keeps the governance fields of an agent's Charter definition. A product composes
// it as a StudioService Section, which freezes the governance into every release, and as a Guard,
// which refuses a release whose governance is malformed or incomplete.
type StudioGovernance struct {
	DB         keelport.DatabaseRepository
	Authorizer contract.StudioAuthorizer
	// Source, when set, owns every field its snapshot supplies; Studio shows those read-only.
	Source contract.GovernanceSource
	// Bindings proves schedule and event bindings; without it such a trigger is refused.
	Bindings contract.TriggerBindingVerifier
	// Required are the fields the active Charter profile requires of a release: nil requires every
	// field Charter's AgentDefinition requires, an empty list none.
	Required []domain.GovernanceField

	once sync.Once
	qs   keelport.QueryService
}

var (
	_ contract.StudioGovernanceBackend = (*StudioGovernance)(nil)
	_ contract.DefinitionSection       = (*StudioGovernance)(nil)
	_ contract.PublishGuard            = (*StudioGovernance)(nil)
)

// Governance returns the agent's effective governance, Studio's draft of it, and what
// publication would refuse.
func (s *StudioGovernance) Governance(ctx context.Context, actor domain.StudioActor, agentID string) (domain.AgentGovernanceView, error) {
	if err := s.authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return domain.AgentGovernanceView{}, err
	}
	_, revision, err := s.agent(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentGovernanceView{}, err
	}
	stored, err := s.stored(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentGovernanceView{}, err
	}
	snapshot, err := s.snapshot(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentGovernanceView{}, err
	}
	return s.view(agentID, stored, snapshot, revision)
}

// SaveGovernance replaces Studio's governance draft behind the agent's draft revision. It refuses
// a field the enterprise source owns, and a schedule or topic the product does not prove.
func (s *StudioGovernance) SaveGovernance(ctx context.Context, actor domain.StudioActor, change domain.AgentGovernanceChange) (domain.AgentGovernanceView, error) {
	if err := s.authorize(ctx, actor, domain.StudioEdit, change.AgentID); err != nil {
		return domain.AgentGovernanceView{}, err
	}
	if _, err := s.required(); err != nil {
		return domain.AgentGovernanceView{}, err
	}
	state, _, err := s.agent(ctx, actor.TenantID, change.AgentID)
	if err != nil {
		return domain.AgentGovernanceView{}, err
	}
	if state == domain.AgentStateArchived {
		return domain.AgentGovernanceView{}, fmt.Errorf("%w: agent %q is archived", domain.ErrConflict, change.AgentID)
	}
	snapshot, err := s.snapshot(ctx, actor.TenantID, change.AgentID)
	if err != nil {
		return domain.AgentGovernanceView{}, err
	}
	problems := draftProblems(change.Governance, snapshot)
	if change.ExpectedDraftRevision <= 0 {
		problems = append(problems, domain.AgentFieldError{Field: "expected_agent_revision", Message: "must be positive"})
	}
	if len(problems) == 0 {
		if problems, err = s.bindingProblems(ctx, actor.TenantID, change.AgentID, change.Governance); err != nil {
			return domain.AgentGovernanceView{}, err
		}
	}
	if len(problems) > 0 {
		return domain.AgentGovernanceView{}, &StudioValidationError{Fields: problems}
	}
	before, err := s.stored(ctx, actor.TenantID, change.AgentID)
	if err != nil {
		return domain.AgentGovernanceView{}, err
	}
	changed, err := changedFields(before, change.Governance)
	if err != nil {
		return domain.AgentGovernanceView{}, err
	}
	encoded, err := json.Marshal(change.Governance)
	if err != nil {
		return domain.AgentGovernanceView{}, fmt.Errorf("encode agent governance: %w", err)
	}

	tx, err := s.DB.BeginTx(ctx, governanceQueries)
	if err != nil {
		return domain.AgentGovernanceView{}, fmt.Errorf("begin governance transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	bumped, err := tx.Query(ctx, qStudioBumpDraft, actorUser(actor), actor.TenantID, change.AgentID, change.ExpectedDraftRevision)
	if err != nil {
		return domain.AgentGovernanceView{}, fmt.Errorf("bump draft revision: %w", err)
	}
	if len(bumped.Rows) == 0 {
		return domain.AgentGovernanceView{}, domain.ErrRevisionConflict
	}
	if _, err = tx.Query(ctx, qGovernancePut, actor.TenantID, change.AgentID, string(encoded), actorUser(actor)); err != nil {
		return domain.AgentGovernanceView{}, fmt.Errorf("store agent governance: %w", err)
	}
	detail := "unchanged"
	if len(changed) > 0 {
		detail = "changed " + strings.Join(changed, ", ")
	}
	if err = auditStudio(ctx, tx, actor, change.AgentID, "GOVERNANCE", detail); err != nil {
		return domain.AgentGovernanceView{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AgentGovernanceView{}, fmt.Errorf("commit governance: %w", err)
	}
	committed = true
	return s.view(change.AgentID, change.Governance, snapshot, common.AsInt64(bumped.Rows[0][0]))
}

// Name is the release section the governance freezes into.
func (s *StudioGovernance) Name() string { return domain.GovernanceSection }

// Section freezes the agent's effective governance; an agent with none contributes no section.
func (s *StudioGovernance) Section(ctx context.Context, tenantID int64, draft domain.AgentDraft) (json.RawMessage, error) {
	stored, err := s.stored(ctx, tenantID, draft.AgentID)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.snapshot(ctx, tenantID, draft.AgentID)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(effectiveGovernance(stored, snapshot))
	if err != nil || string(encoded) == "{}" {
		return nil, err
	}
	return encoded, nil
}

// CheckPublish refuses a release, or a restore, whose frozen governance is malformed, lacks a
// field the active Charter profile requires, binds a schedule or topic the product does not
// prove, or froze enterprise governance other than the source's current snapshot. A restore of a
// release frozen with no governance section predates governance, so the profile does not apply to it.
func (s *StudioGovernance) CheckPublish(ctx context.Context, candidate domain.PublishCandidate) error {
	required, err := s.required()
	if err != nil {
		return err
	}
	tenantID, agentID := candidate.Actor.TenantID, candidate.Definition.AgentID
	var frozen domain.AgentGovernance
	raw := candidate.Definition.Sections[domain.GovernanceSection]
	if candidate.RestoredFrom != "" && len(raw) == 0 {
		required = nil
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &frozen); err != nil {
			return fmt.Errorf("%w: governance section of agent %q: %v", domain.ErrValidation, agentID, err)
		}
	}
	snapshot, err := s.snapshot(ctx, tenantID, agentID)
	if err != nil {
		return err
	}
	if err = sameSnapshot(frozen.Enterprise, snapshot); err != nil {
		return err
	}
	problems := append(frozenProblems(frozen), missingFields(frozen, required)...)
	if len(problems) == 0 {
		if problems, err = s.bindingProblems(ctx, tenantID, agentID, frozen); err != nil {
			return err
		}
	}
	if len(problems) > 0 {
		return &StudioValidationError{Fields: problems}
	}
	return nil
}

func (s *StudioGovernance) view(agentID string, stored domain.AgentGovernance, snapshot *domain.GovernanceSnapshot, revision int64) (domain.AgentGovernanceView, error) {
	required, err := s.required()
	if err != nil {
		return domain.AgentGovernanceView{}, err
	}
	effective := effectiveGovernance(stored, snapshot)
	problems := append(frozenProblems(effective), missingFields(effective, required)...)
	view := domain.AgentGovernanceView{
		AgentID: agentID, Effective: effective, Draft: studioDraft(stored, snapshot),
		Problems: append(problems, s.unprovable(effective)...), DraftRevision: revision,
	}
	if snapshot != nil {
		view.Link = snapshot.Link
	}
	return view, nil
}

// unprovable names each schedule or event trigger while no verifier is composed to prove its binding.
func (s *StudioGovernance) unprovable(g domain.AgentGovernance) []domain.AgentFieldError {
	var problems fieldProblems
	for index, trigger := range g.TriggerChannels {
		if s.Bindings == nil && bound(trigger) {
			problems.add(bindingField(index), "no verifier of schedule and event bindings is composed")
		}
	}
	return problems
}

// bindingProblems refuses each schedule or event trigger the product does not prove exists.
func (s *StudioGovernance) bindingProblems(ctx context.Context, tenantID int64, agentID string, g domain.AgentGovernance) ([]domain.AgentFieldError, error) {
	if problems := s.unprovable(g); len(problems) > 0 {
		return problems, nil
	}
	var problems fieldProblems
	for index, trigger := range g.TriggerChannels {
		if !bound(trigger) {
			continue
		}
		known, err := s.Bindings.Verify(ctx, tenantID, agentID, trigger)
		if err != nil {
			return nil, fmt.Errorf("verify %s binding %q: %w", trigger.Channel, trigger.Binding, err)
		}
		if !known {
			problems.add(bindingField(index), "the product has no such "+string(trigger.Channel)+" for this agent")
		}
	}
	return problems, nil
}

func bound(trigger domain.AgentTrigger) bool {
	return trigger.Channel == domain.TriggerSchedule || trigger.Channel == domain.TriggerEvent
}

func bindingField(index int) string { return fmt.Sprintf("triggerChannels[%d].binding", index) }

func (s *StudioGovernance) required() ([]domain.GovernanceField, error) {
	if s.Required == nil {
		return domain.CharterGovernanceFields(), nil
	}
	for _, field := range s.Required {
		if !slices.Contains(domain.CharterGovernanceFields(), field) {
			return nil, fmt.Errorf("studio governance: %q is not a Charter governance field", field)
		}
	}
	return s.Required, nil
}

func (s *StudioGovernance) authorize(ctx context.Context, actor domain.StudioActor, action domain.StudioAction, agentID string) error {
	if s.Authorizer == nil {
		return fmt.Errorf("studio governance: authorizer is required")
	}
	return s.Authorizer.Authorize(ctx, actor, action, agentID)
}

func (s *StudioGovernance) queries(ctx context.Context) (keelport.QueryService, error) {
	if s.DB == nil {
		return nil, fmt.Errorf("studio governance: database is required")
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, governanceQueries) })
	return s.qs, nil
}

func (s *StudioGovernance) agent(ctx context.Context, tenantID int64, agentID string) (domain.AgentState, int64, error) {
	qs, err := s.queries(ctx)
	if err != nil {
		return "", 0, err
	}
	res, err := qs.Query(ctx, qGovernanceAgent, tenantID, agentID)
	if err != nil {
		return "", 0, fmt.Errorf("load agent %q: %w", agentID, err)
	}
	if len(res.Rows) == 0 {
		return "", 0, domain.ErrNotFound
	}
	return domain.AgentState(common.AsString(res.Rows[0][0])), common.AsInt64(res.Rows[0][1]), nil
}

// stored is Studio's governance draft of the agent; an agent with none has an empty one.
func (s *StudioGovernance) stored(ctx context.Context, tenantID int64, agentID string) (domain.AgentGovernance, error) {
	qs, err := s.queries(ctx)
	if err != nil {
		return domain.AgentGovernance{}, err
	}
	res, err := qs.Query(ctx, qGovernanceGet, tenantID, agentID)
	if err != nil {
		return domain.AgentGovernance{}, fmt.Errorf("load governance of agent %q: %w", agentID, err)
	}
	var governance domain.AgentGovernance
	if len(res.Rows) == 0 {
		return governance, nil
	}
	if err = json.Unmarshal([]byte(common.AsString(res.Rows[0][0])), &governance); err != nil {
		return domain.AgentGovernance{}, fmt.Errorf("decode governance of agent %q: %w", agentID, err)
	}
	return governance, nil
}

// snapshot is the source's current snapshot of the agent's governance; nil when the enterprise
// owns none of it. An unusable snapshot fails closed.
func (s *StudioGovernance) snapshot(ctx context.Context, tenantID int64, agentID string) (*domain.GovernanceSnapshot, error) {
	if s.Source == nil {
		return nil, nil
	}
	snapshot, ok, err := s.Source.Snapshot(ctx, tenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("resolve governance of agent %q: %w", agentID, err)
	}
	if !ok {
		return nil, nil
	}
	if problems := snapshotProblems(snapshot); len(problems) > 0 {
		return nil, fmt.Errorf("governance source: snapshot of agent %q is unusable: %s %s", agentID, problems[0].Field, problems[0].Message)
	}
	return &snapshot, nil
}

// sameSnapshot refuses a release that froze other enterprise governance than the source's
// current snapshot, or none while the enterprise owns some.
func sameSnapshot(frozen *domain.GovernanceSnapshotRef, current *domain.GovernanceSnapshot) error {
	switch {
	case frozen == nil && current == nil:
		return nil
	case frozen == nil:
		return fmt.Errorf("%w: the enterprise owns this agent's governance since the release was frozen; publish the draft", domain.ErrConflict)
	case current == nil:
		return fmt.Errorf("%w: the release froze enterprise governance the source no longer supplies", domain.ErrConflict)
	case frozen.Document != current.Document || frozen.Version != current.Version || frozen.Digest != current.Digest:
		return fmt.Errorf("%w: the release froze enterprise governance version %q, the source's is now %q", domain.ErrConflict, frozen.Version, current.Version)
	}
	return nil
}
