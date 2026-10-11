package controlplane

import (
	"context"
	"encoding/json"
	"fmt"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

// effectiveGate compiles the agent's settings and returns its evaluation_gate value; nil when none applies.
func (s *StudioService) effectiveGate(ctx context.Context, tenantID int64, agentID, agentTypeID string) ([]byte, error) {
	scopes, err := s.promptScopes(ctx, tenantID, agentID, agentTypeID)
	if err != nil {
		return nil, err
	}
	release, err := s.compileSettings(ctx, tenantID, scopes.AgentScopeID, agentID, nil, nil)
	if err != nil {
		return nil, err
	}
	if err = validateSingletonSettings(release.Resources); err != nil {
		return nil, err
	}
	return scope.EvaluationGateValue(release.Resources), nil
}

// checkGateInTx runs every composed EvaluationGateGuard inside the publish or restore transaction.
// An agent bound to an evaluation gate with no such guard composed is refused, so a bound gate is
// never silently ignored.
func (s *StudioService) checkGateInTx(ctx context.Context, tx keelport.TxQueryService, candidate domain.PublishCandidate) error {
	composed := false
	for _, guard := range s.Guards {
		gate, ok := guard.(EvaluationGateGuard)
		if !ok {
			continue
		}
		composed = true
		if err := gate.CheckRelease(ctx, tx, candidate); err != nil {
			return err
		}
	}
	if composed {
		return nil
	}
	value, err := s.effectiveGate(ctx, candidate.Actor.TenantID, candidate.Definition.AgentID, candidate.Definition.AgentTypeID)
	if err != nil {
		return err
	}
	if value != nil {
		return fmt.Errorf("%w: agent %q is bound to an evaluation gate but no evaluation gate guard is composed", domain.ErrNotReady, candidate.Definition.AgentID)
	}
	return nil
}

// gateLoosening is a settings change that removes or loosens the evaluation gate at a scope.
type gateLoosening struct {
	agentID, scopeID string
	before, after    []byte
}

// checkGateChange compiles the evaluation gate in force at scopeID now and as after answers the
// bindings. A change that removes or loosens it needs OVERRIDE_GATE and an audit sink; it is
// returned for recordGateLoosening, and nil when the gate is kept or tightened.
func (s *StudioService) checkGateChange(ctx context.Context, actor domain.StudioActor, agentID, scopeID string, after contract.ScopeRepository) (*gateLoosening, error) {
	before, err := s.compileOver(ctx, s.scopeRepository(), actor.TenantID, scopeID, agentID, nil)
	if err != nil {
		return nil, err
	}
	changed, err := s.compileOver(ctx, after, actor.TenantID, scopeID, agentID, nil)
	if err != nil {
		return nil, err
	}
	loosening := &gateLoosening{agentID: agentID, scopeID: scopeID,
		before: scope.EvaluationGateValue(before.Resources), after: scope.EvaluationGateValue(changed.Resources)}
	loosened, err := scope.EvaluationGateLoosened(loosening.before, loosening.after)
	if err != nil || !loosened {
		return nil, err
	}
	if err = s.Authorize(ctx, actor, domain.StudioOverrideGate, agentID); err != nil {
		return nil, fmt.Errorf("removing or loosening the evaluation gate needs OVERRIDE_GATE: %w", err)
	}
	if s.Audit == nil {
		return nil, fmt.Errorf("%w: removing or loosening an evaluation gate needs an audit sink", domain.ErrNotReady)
	}
	return loosening, nil
}

// recordGateLoosening records a loosening as a Studio event in tx and an audit decision, written
// before the caller commits; its key replays it if the change must be retried.
func (s *StudioService) recordGateLoosening(ctx context.Context, tx keelport.TxQueryService, actor domain.StudioActor, loosening *gateLoosening) error {
	if loosening == nil {
		return nil
	}
	detail := "evaluation_gate at " + loosening.scopeID + " removed"
	if loosening.after != nil {
		detail = "evaluation_gate at " + loosening.scopeID + " loosened to " + string(loosening.after)
	}
	if err := auditStudio(ctx, tx, actor, loosening.agentID, StudioEventGateLoosen, truncate(detail, maxGateReason)); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"scope_id": loosening.scopeID, "before": json.RawMessage(loosening.before),
		"after": json.RawMessage(nullJSON(loosening.after))})
	if err != nil {
		return fmt.Errorf("encode gate loosening evidence: %w", err)
	}
	if err = s.Audit.Record(ctx, domain.DecisionRecord{
		TenantID: actor.TenantID, Principal: actor.Principal, Category: domain.DecisionCategoryPublication,
		Action: "loosen_evaluation_gate", Resource: agentScopeValue(loosening.agentID), ScopeID: loosening.scopeID,
		PolicyID: string(domain.ResourceEvaluationGate), Outcome: domain.DecisionAllow, Reason: detail, Payload: payload,
		OccurredAt: s.now(),
		DecisionKey: sha256Hex(fmt.Sprintf("loosen\x1f%d\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s", actor.TenantID, loosening.agentID, loosening.scopeID,
			loosening.before, loosening.after, actor.Principal.ID)),
	}); err != nil {
		return fmt.Errorf("audit evaluation gate loosening: %w", err)
	}
	return nil
}

func nullJSON(value []byte) []byte {
	if value == nil {
		return []byte("null")
	}
	return value
}
