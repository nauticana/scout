package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

const (
	qGateActive         = "scout_gate_active"
	qGateActiveLocked   = "scout_gate_active_locked"
	qGateSet            = "scout_gate_set"
	qGateRun            = "scout_gate_run"
	qGateResults        = "scout_gate_results"
	qGateDecision       = "scout_gate_decision"
	qGateDecisionRuns   = "scout_gate_decision_runs"
	qGateDecisionLocked = "scout_gate_decision_locked"
	qGateDropRuns       = "scout_gate_drop_runs"
	qGateDrop           = "scout_gate_drop"
	qGateInsert         = "scout_gate_insert"
	qGateInsertRun      = "scout_gate_insert_run"
	qGateAudit          = "scout_gate_audit"
)

// Studio events the evaluation gate records.
const (
	StudioEventGateEvaluate = "GATE_EVALUATE"
	StudioEventGateOverride = "GATE_OVERRIDE"
	// StudioEventGateRestore records a restore admitted on OVERRIDE_GATE alone.
	StudioEventGateRestore = "GATE_RESTORE_OVERRIDE"
	// StudioEventGateLoosen records a setting change that removed or loosened an evaluation gate.
	StudioEventGateLoosen = "GATE_LOOSEN"
)

// gateCatalogID binds the gate's queries to a publish or restore transaction.
const gateCatalogID = "scout.controlplane.evaluation_gate"

// EvaluationGateGuard is a publish guard that decides an evaluation gate again inside the publish or
// restore transaction, after the draft lock. Publish and Restore refuse an agent bound to an
// evaluation_gate setting when no such guard is composed.
type EvaluationGateGuard interface {
	contract.PublishGuard
	CheckRelease(ctx context.Context, tx keelport.TxQueryService, candidate domain.PublishCandidate) error
}

// DefaultGateMaxResults is how many results of one run a gate decision reads when StudioEvaluationGate.MaxResults is zero.
const DefaultGateMaxResults = 2000

const maxGateReason = 400

const gateActive = `
SELECT dep.stable_version, v.definition_digest
  FROM agent_deployment dep
  JOIN agent_version v ON v.partner_id = dep.partner_id AND v.agent_id = dep.agent_id AND v.agent_version = dep.stable_version
 WHERE dep.partner_id = ? AND dep.agent_id = ?`

var gateQueries = map[string]string{
	qGateActive: gateActive,
	// In a publish or restore transaction the deployment stays locked until the release deploys over it.
	qGateActiveLocked: gateActive + `
   FOR UPDATE OF dep`,
	qGateSet: `
SELECT a.golden_set_id, MAX(v.set_version)
  FROM agent_golden_set a
  LEFT JOIN golden_set_version v ON v.partner_id = a.partner_id AND v.golden_set_id = a.golden_set_id
 WHERE a.partner_id = ? AND a.agent_id = ?
 GROUP BY a.golden_set_id`,
	// The newest completed run of a subject with the digest, whether it ran as a draft or a release.
	// A comparison run has a baseline arm and is never gate evidence.
	qGateRun: `
SELECT r.id
  FROM evaluation_run r
  JOIN evaluation_run_subject s ON s.partner_id = r.partner_id AND s.run_id = r.id AND s.role_code = 'candidate'
 WHERE r.partner_id = ? AND r.agent_id = ? AND r.golden_set_id = ? AND r.set_version = ? AND r.scope_code = ?
   AND r.status_code = 'completed' AND s.definition_digest = ?
   AND NOT EXISTS (SELECT 1 FROM evaluation_run_subject b
                    WHERE b.partner_id = r.partner_id AND b.run_id = r.id AND b.role_code = 'baseline')
 ORDER BY r.completed_at DESC, r.id DESC
 LIMIT 1`,
	qGateResults: `
SELECT scores FROM evaluation_result
 WHERE partner_id = ? AND run_id = ? AND role_code = 'candidate'
 ORDER BY example_id
 LIMIT ?`,
	qGateDecision: `
SELECT verdict_code, reason, policy_digest, policy_json, inputs_digest, base_version, golden_set_id, set_version,
       decided_by_kind, decided_by_id, decided_at
  FROM agent_gate_decision
 WHERE partner_id = ? AND agent_id = ? AND definition_digest = ?`,
	qGateDecisionRuns: `
SELECT d.run_id, d.role_code, r.scope_code, d.score_bp, d.sample_count, d.critical_count
  FROM agent_gate_decision_run d
  JOIN evaluation_run r ON r.partner_id = d.partner_id AND r.id = d.run_id
 WHERE d.partner_id = ? AND d.agent_id = ? AND d.definition_digest = ?
 ORDER BY d.role_code DESC, r.scope_code, d.run_id`,
	// Locks only the decision being replaced; a first decision is guarded by the primary key.
	qGateDecisionLocked: `
SELECT verdict_code, reason, policy_digest, policy_json, inputs_digest, base_version, golden_set_id, set_version,
       decided_by_kind, decided_by_id, decided_at
  FROM agent_gate_decision
 WHERE partner_id = ? AND agent_id = ? AND definition_digest = ?
   FOR UPDATE`,
	qGateDropRuns: `
DELETE FROM agent_gate_decision_run WHERE partner_id = ? AND agent_id = ? AND definition_digest = ?`,
	qGateDrop: `
DELETE FROM agent_gate_decision WHERE partner_id = ? AND agent_id = ? AND definition_digest = ?`,
	qGateInsert: `
INSERT INTO agent_gate_decision (partner_id, agent_id, definition_digest, verdict_code, reason, policy_digest, policy_json,
       inputs_digest, base_version, golden_set_id, set_version, decided_by_kind, decided_by_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (partner_id, agent_id, definition_digest) DO NOTHING
RETURNING decided_at`,
	qGateInsertRun: `
INSERT INTO agent_gate_decision_run (partner_id, agent_id, definition_digest, run_id, role_code, score_bp, sample_count, critical_count)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
	qGateAudit: `
INSERT INTO agent_studio_event (id, partner_id, agent_id, event, detail, principal_kind, principal_id)
VALUES (NEXTVAL('agent_studio_event_seq'), ?, ?, ?, ?, ?, ?)`,
}

// StudioEvaluationGate holds publication to the agent's evaluation_gate setting. A decision for
// a definition digest is made from stored evaluation results: for each required golden scope,
// the newest completed run of that digest on the newest frozen version of the agent's golden
// set must have no critical failure, enough samples, and a mean metric score at the minimum,
// and, when the gate bounds regression and a release is active, no further below the newest
// completed run of the active release on the same version and scope. Composed as a publish
// guard it refuses a publish whose digest has no decision, a failing one, or one made under
// another policy, active release, or set version. A person holding OVERRIDE_GATE may admit one
// digest with a reason, recorded as a Studio event and an AuditSink decision.
type StudioEvaluationGate struct {
	Studio *StudioService
	// Audit records every override; an override is refused without it.
	Audit contract.AuditSink
	// MaxResults bounds the results of one run a decision reads; zero takes DefaultGateMaxResults.
	MaxResults int

	once sync.Once
	qs   keelport.QueryService
}

var (
	_ contract.StudioEvaluationGateBackend = (*StudioEvaluationGate)(nil)
	_ EvaluationGateGuard                  = (*StudioEvaluationGate)(nil)
)

func (g *StudioEvaluationGate) init(ctx context.Context) error {
	if g.Studio == nil {
		return fmt.Errorf("%w: the evaluation gate needs the studio service", domain.ErrNotReady)
	}
	if g.MaxResults < 0 {
		return fmt.Errorf("%w: the evaluation gate's max results cannot be negative", domain.ErrValidation)
	}
	if err := g.Studio.init(ctx); err != nil {
		return err
	}
	g.once.Do(func() { g.qs = g.Studio.DB.GetQueryService(ctx, gateQueries) })
	if g.qs == nil {
		return fmt.Errorf("%w: the evaluation gate has no query service", domain.ErrNotReady)
	}
	return nil
}

func (g *StudioEvaluationGate) maxResults() int {
	if g.MaxResults == 0 {
		return DefaultGateMaxResults
	}
	return g.MaxResults
}

// gateContext is what a decision is compared with now.
type gateContext struct {
	policy        *domain.EvaluationGatePolicy
	policyDigest  string
	activeVersion string
	activeDigest  string
	goldenSetID   string
	setVersion    int64
}

// current compiles the agent's effective gate and reads, through qs, its active release with
// activeQuery and the newest frozen version of its golden set.
func (g *StudioEvaluationGate) current(ctx context.Context, qs keelport.QueryService, activeQuery string, tenantID int64, agentID, agentTypeID string) (gateContext, error) {
	var current gateContext
	value, err := g.Studio.effectiveGate(ctx, tenantID, agentID, agentTypeID)
	if err != nil || value == nil {
		return current, err
	}
	policy, err := scope.DecodeEvaluationGate(value)
	if err != nil {
		return current, err
	}
	if current.policyDigest, err = scope.EvaluationGateDigest(policy); err != nil {
		return current, err
	}
	current.policy = &policy
	active, err := qs.Query(ctx, activeQuery, tenantID, agentID)
	if err != nil {
		return current, fmt.Errorf("load active release: %w", err)
	}
	if len(active.Rows) > 0 {
		current.activeVersion = common.AsString(active.Rows[0][0])
		current.activeDigest = strings.TrimSpace(common.AsString(active.Rows[0][1]))
	}
	set, err := qs.Query(ctx, qGateSet, tenantID, agentID)
	if err != nil {
		return current, fmt.Errorf("load golden set version: %w", err)
	}
	if len(set.Rows) > 0 {
		current.goldenSetID, current.setVersion = common.AsString(set.Rows[0][0]), common.AsInt64(set.Rows[0][1])
	}
	return current, nil
}

// Gate reports the gate of the agent's current draft and the decision recorded for its digest.
func (g *StudioEvaluationGate) Gate(ctx context.Context, actor domain.StudioActor, agentID string) (domain.EvaluationGateState, error) {
	if err := g.init(ctx); err != nil {
		return domain.EvaluationGateState{}, err
	}
	if err := g.Studio.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return domain.EvaluationGateState{}, err
	}
	state := domain.EvaluationGateState{AgentID: agentID}
	definition, current, err := g.subject(ctx, actor.TenantID, agentID)
	if candidateProblem(err) {
		state.Problem = err.Error()
		return state, nil
	}
	if err != nil {
		return domain.EvaluationGateState{}, err
	}
	decision, err := g.decision(ctx, g.qs, actor.TenantID, agentID, definition.DefinitionDigest)
	if err != nil {
		return domain.EvaluationGateState{}, err
	}
	return gateState(agentID, definition.DefinitionDigest, current, decision), nil
}

// EvaluateGate decides the gate for the agent's current draft from its stored evaluations and
// records the decision, replacing any earlier one for the digest, an override included. A
// decision on unchanged inputs is returned as it was recorded.
func (g *StudioEvaluationGate) EvaluateGate(ctx context.Context, actor domain.StudioActor, agentID string) (domain.EvaluationGateState, error) {
	if err := g.init(ctx); err != nil {
		return domain.EvaluationGateState{}, err
	}
	if _, err := g.Studio.editable(ctx, actor, domain.StudioTest, agentID); err != nil {
		return domain.EvaluationGateState{}, err
	}
	definition, current, err := g.subject(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.EvaluationGateState{}, err
	}
	if current.policy == nil {
		return domain.EvaluationGateState{}, fmt.Errorf("%w: agent %q has no evaluation gate", domain.ErrConflict, agentID)
	}
	decision, err := g.decide(ctx, actor.TenantID, agentID, definition.DefinitionDigest, current)
	if err != nil {
		return domain.EvaluationGateState{}, err
	}
	decision.DecidedBy = actor.Principal
	detail := fmt.Sprintf("%s of definition %s", decision.Verdict, decision.DefinitionDigest)
	if decision.Reason != "" {
		detail += ": " + decision.Reason
	}
	recorded, err := g.record(ctx, actor, current, decision, StudioEventGateEvaluate, detail, nil)
	if err != nil {
		return domain.EvaluationGateState{}, err
	}
	return gateState(agentID, definition.DefinitionDigest, current, &recorded), nil
}

// OverrideGate admits the agent's current draft, named by its digest, although its gate does not.
func (g *StudioEvaluationGate) OverrideGate(ctx context.Context, actor domain.StudioActor, override domain.EvaluationGateOverride) (domain.EvaluationGateState, error) {
	if err := g.init(ctx); err != nil {
		return domain.EvaluationGateState{}, err
	}
	if g.Audit == nil {
		return domain.EvaluationGateState{}, fmt.Errorf("%w: evaluation gate overrides need an audit sink", domain.ErrNotReady)
	}
	reason := strings.TrimSpace(override.Reason)
	var fields []domain.AgentFieldError
	if reason == "" || len(reason) > maxGateReason {
		fields = append(fields, domain.AgentFieldError{Field: "reason", Message: "must contain 1 to 400 characters"})
	}
	if !validSHA256(override.DefinitionDigest) {
		fields = append(fields, domain.AgentFieldError{Field: "definition_digest", Message: "must be the 64-character digest of the agent's current draft"})
	}
	if len(fields) > 0 {
		return domain.EvaluationGateState{}, &StudioValidationError{Fields: fields}
	}
	if _, err := g.Studio.editable(ctx, actor, domain.StudioOverrideGate, override.AgentID); err != nil {
		return domain.EvaluationGateState{}, err
	}
	definition, current, err := g.subject(ctx, actor.TenantID, override.AgentID)
	if err != nil {
		return domain.EvaluationGateState{}, err
	}
	digest := definition.DefinitionDigest
	if digest != override.DefinitionDigest {
		return domain.EvaluationGateState{}, fmt.Errorf("%w: definition %s is not the agent's current draft %s", domain.ErrConflict, override.DefinitionDigest, digest)
	}
	if current.policy == nil {
		return domain.EvaluationGateState{}, fmt.Errorf("%w: agent %q has no evaluation gate", domain.ErrConflict, override.AgentID)
	}
	existing, err := g.decision(ctx, g.qs, actor.TenantID, override.AgentID, digest)
	if err != nil {
		return domain.EvaluationGateState{}, err
	}
	if existing != nil && existing.Verdict == domain.GateVerdictPass && len(staleness(*existing, current)) == 0 {
		return domain.EvaluationGateState{}, fmt.Errorf("%w: the evaluation gate already admits definition %s", domain.ErrConflict, digest)
	}
	decision := domain.EvaluationGateDecision{
		AgentID: override.AgentID, DefinitionDigest: digest, Verdict: domain.GateVerdictOverride, Reason: reason,
		Policy: *current.policy, PolicyDigest: current.policyDigest, BaseVersion: current.activeVersion,
		GoldenSetID: current.goldenSetID, SetVersion: current.setVersion, DecidedBy: actor.Principal,
	}
	if decision.SetVersion == 0 {
		decision.GoldenSetID = ""
	}
	inputs := inputsDigest(decision)
	payload, err := json.Marshal(map[string]any{"definition_digest": digest, "policy_digest": current.policyDigest,
		"base_version": current.activeVersion, "golden_set_id": decision.GoldenSetID, "set_version": decision.SetVersion})
	if err != nil {
		return domain.EvaluationGateState{}, fmt.Errorf("encode gate override evidence: %w", err)
	}
	evidence := domain.DecisionRecord{
		TenantID: actor.TenantID, Principal: actor.Principal, Category: domain.DecisionCategoryPublication,
		Action: "override_evaluation_gate", Resource: agentScopeValue(override.AgentID), ReleaseVersion: current.activeVersion,
		PolicyID: string(domain.ResourceEvaluationGate), PolicyVersion: current.policyDigest, Outcome: domain.DecisionAllow,
		Reason: reason, Payload: payload, OccurredAt: g.Studio.now(), DecisionKey: inputs,
	}
	recorded, err := g.record(ctx, actor, current, decision, StudioEventGateOverride, "override of definition "+digest+": "+reason, &evidence)
	if err != nil {
		return domain.EvaluationGateState{}, err
	}
	return gateState(override.AgentID, digest, current, &recorded), nil
}

// CheckPublish refuses, before the release transaction, a publish or restore its agent's evaluation
// gate does not admit; CheckRelease decides again inside it. A scheduled activation was admitted
// when it was scheduled and passes.
func (g *StudioEvaluationGate) CheckPublish(ctx context.Context, candidate domain.PublishCandidate) error {
	if candidate.Activation {
		return nil
	}
	if err := g.init(ctx); err != nil {
		return err
	}
	return g.check(ctx, g.qs, qGateActive, candidate, false)
}

// CheckRelease decides the gate inside the publish or restore transaction after the draft lock,
// reading the decision and the locked deployment through tx, and records a restore admitted on
// OVERRIDE_GATE alone.
func (g *StudioEvaluationGate) CheckRelease(ctx context.Context, tx keelport.TxQueryService, candidate domain.PublishCandidate) error {
	if candidate.Activation {
		return nil
	}
	if err := g.init(ctx); err != nil {
		return err
	}
	catalog, ok := tx.(keelport.TxQueryCatalog)
	if !ok {
		return fmt.Errorf("%w: the release transaction cannot bind the evaluation gate's queries", domain.ErrNotReady)
	}
	qs := catalog.QueryService(gateCatalogID, gateQueries)
	if qs == nil {
		return fmt.Errorf("%w: the release transaction returned no evaluation gate query service", domain.ErrNotReady)
	}
	return g.check(ctx, qs, qGateActiveLocked, candidate, true)
}

func (g *StudioEvaluationGate) check(ctx context.Context, qs keelport.QueryService, activeQuery string, candidate domain.PublishCandidate, record bool) error {
	tenantID, agentID, digest := candidate.Actor.TenantID, candidate.Definition.AgentID, candidate.Definition.DefinitionDigest
	current, err := g.current(ctx, qs, activeQuery, tenantID, agentID, candidate.Definition.AgentTypeID)
	if err != nil || current.policy == nil {
		return err
	}
	decision, err := g.decision(ctx, qs, tenantID, agentID, digest)
	if err != nil {
		return err
	}
	if candidate.RestoredFrom == "" {
		return admit(digest, current, decision)
	}
	return g.admitRestore(ctx, qs, candidate, current, decision, record)
}

// admitRestore admits restoring a version whose digest the gate once passed or a person overrode,
// however stale that decision is now; any other restore needs OVERRIDE_GATE, recorded as evidence
// when record is set. A version published before its gate, or outside Studio, was never decided.
func (g *StudioEvaluationGate) admitRestore(ctx context.Context, qs keelport.QueryService, candidate domain.PublishCandidate,
	current gateContext, decision *domain.EvaluationGateDecision, record bool) error {
	if decision != nil && decision.Verdict != domain.GateVerdictFail {
		return nil
	}
	actor, agentID, digest := candidate.Actor, candidate.Definition.AgentID, candidate.Definition.DefinitionDigest
	if err := g.Studio.Authorize(ctx, actor, domain.StudioOverrideGate, agentID); err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			return &domain.EvaluationGateRefusal{Code: domain.GateRefusedMissing,
				Detail: "version " + candidate.RestoredFrom + " has no passing or overriding decision; restoring it needs OVERRIDE_GATE"}
		}
		return err
	}
	if !record {
		return nil
	}
	if g.Audit == nil {
		return fmt.Errorf("%w: restoring an ungated version needs an audit sink", domain.ErrNotReady)
	}
	detail := "restore of version " + candidate.RestoredFrom + " definition " + digest + " on OVERRIDE_GATE"
	if _, err := qs.Query(ctx, qGateAudit, actor.TenantID, agentID, StudioEventGateRestore, detail,
		string(actor.Principal.Kind), actor.Principal.ID); err != nil {
		return fmt.Errorf("record studio audit: %w", err)
	}
	payload, err := json.Marshal(map[string]any{"definition_digest": digest, "restored_version": candidate.RestoredFrom,
		"policy_digest": current.policyDigest, "base_version": current.activeVersion})
	if err != nil {
		return fmt.Errorf("encode gate restore evidence: %w", err)
	}
	// The evidence is written before the restore commits; its key replays it if the restore must be retried.
	if err = g.Audit.Record(ctx, domain.DecisionRecord{
		TenantID: actor.TenantID, Principal: actor.Principal, Category: domain.DecisionCategoryPublication,
		Action: "restore_over_evaluation_gate", Resource: agentScopeValue(agentID), ReleaseVersion: candidate.RestoredFrom,
		PolicyID: string(domain.ResourceEvaluationGate), PolicyVersion: current.policyDigest, Outcome: domain.DecisionAllow,
		Reason: detail, Payload: payload, OccurredAt: g.Studio.now(),
		DecisionKey: sha256Hex(fmt.Sprintf("restore\x1f%d\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s", actor.TenantID, agentID, candidate.RestoredFrom,
			current.activeVersion, current.policyDigest, actor.Principal.ID)),
	}); err != nil {
		return fmt.Errorf("audit restore over evaluation gate: %w", err)
	}
	return nil
}

// subject freezes the agent's current draft as Publish would and reads what its gate compares against.
func (g *StudioEvaluationGate) subject(ctx context.Context, tenantID int64, agentID string) (domain.AgentDefinition, gateContext, error) {
	draft, err := g.Studio.draft(ctx, tenantID, agentID)
	if err != nil {
		return domain.AgentDefinition{}, gateContext{}, err
	}
	definition, _, err := g.Studio.frozenDraft(ctx, tenantID, draft, nil)
	if err != nil {
		return domain.AgentDefinition{}, gateContext{}, err
	}
	current, err := g.current(ctx, g.qs, qGateActive, tenantID, agentID, draft.AgentTypeID)
	return definition, current, err
}

// admit is nil when the decision admits digest under the current gate.
func admit(digest string, current gateContext, decision *domain.EvaluationGateDecision) error {
	switch {
	case current.policy == nil:
		return nil
	case decision == nil:
		return &domain.EvaluationGateRefusal{Code: domain.GateRefusedMissing, Detail: "no decision for definition " + digest + "; evaluate the gate for the current draft"}
	case decision.Verdict == domain.GateVerdictFail:
		return &domain.EvaluationGateRefusal{Code: domain.GateRefusedFailed, Detail: decision.Reason}
	}
	if stale := staleness(*decision, current); len(stale) > 0 {
		return &domain.EvaluationGateRefusal{Code: domain.GateRefusedStale, Detail: "the decision for definition " + digest + " is stale: " + strings.Join(stale, ", ")}
	}
	return nil
}

// staleness lists why a decision no longer holds: another policy or active release, or, for a
// pass, a newer frozen version of the golden set its runs were on.
func staleness(decision domain.EvaluationGateDecision, current gateContext) []string {
	var stale []string
	if decision.PolicyDigest != current.policyDigest {
		stale = append(stale, domain.GateStalePolicy)
	}
	if decision.BaseVersion != current.activeVersion {
		stale = append(stale, domain.GateStaleRelease)
	}
	if decision.Verdict == domain.GateVerdictPass && (decision.GoldenSetID != current.goldenSetID || decision.SetVersion != current.setVersion) {
		stale = append(stale, domain.GateStaleSetVersion)
	}
	return stale
}

func gateState(agentID, digest string, current gateContext, decision *domain.EvaluationGateDecision) domain.EvaluationGateState {
	state := domain.EvaluationGateState{
		AgentID: agentID, CurrentDigest: digest, Policy: current.policy, PolicyDigest: current.policyDigest,
		ActiveVersion: current.activeVersion, GoldenSetID: current.goldenSetID, SetVersion: current.setVersion, Decision: decision,
		Admitted: admit(digest, current, decision) == nil,
	}
	if decision != nil {
		state.Stale = staleness(*decision, current)
	}
	return state
}

// decide scores the digest's runs, and the active release's when regression is bounded, against the gate.
func (g *StudioEvaluationGate) decide(ctx context.Context, tenantID int64, agentID, digest string, current gateContext) (domain.EvaluationGateDecision, error) {
	policy := *current.policy
	decision := domain.EvaluationGateDecision{
		AgentID: agentID, DefinitionDigest: digest, Policy: policy, PolicyDigest: current.policyDigest,
		BaseVersion: current.activeVersion, GoldenSetID: current.goldenSetID, SetVersion: current.setVersion,
	}
	var reasons []string
	if current.setVersion == 0 {
		decision.GoldenSetID = ""
		reasons = append(reasons, "the agent has no frozen golden set version")
	}
	for _, golden := range policy.RequiredScopes {
		if current.setVersion == 0 {
			break
		}
		on := fmt.Sprintf("%s version %d", current.goldenSetID, current.setVersion)
		candidate, found, err := g.score(ctx, tenantID, agentID, digest, current, golden, policy.Metric)
		if err != nil {
			return decision, err
		}
		if !found {
			reasons = append(reasons, fmt.Sprintf("no completed %s evaluation of the draft on %s", golden, on))
			continue
		}
		candidate.Role = domain.GateRunCandidate
		decision.Runs = append(decision.Runs, candidate.EvaluationGateRun)
		if candidate.Criticals > 0 {
			reasons = append(reasons, fmt.Sprintf("%s run %d has %d critical failures", golden, candidate.RunID, candidate.Criticals))
		}
		if candidate.invalid > 0 {
			reasons = append(reasons, fmt.Sprintf("%s run %d has %d %s values outside 0..1", golden, candidate.RunID, candidate.invalid, policy.Metric))
		}
		if minimum := max(policy.MinSamples, 1); candidate.Samples < minimum {
			reasons = append(reasons, fmt.Sprintf("%s run %d has %d samples, fewer than %d", golden, candidate.RunID, candidate.Samples, minimum))
		}
		if candidate.ScoreBP < policy.MinScoreBP {
			reasons = append(reasons, fmt.Sprintf("%s %s score %d bp is below %d bp", golden, policy.Metric, candidate.ScoreBP, policy.MinScoreBP))
		}
		if policy.MaxRegressionBP == nil || current.activeVersion == "" {
			continue
		}
		baseline, found, err := g.score(ctx, tenantID, agentID, current.activeDigest, current, golden, policy.Metric)
		if err != nil {
			return decision, err
		}
		if !found {
			reasons = append(reasons, fmt.Sprintf("no completed %s evaluation of active release %s on %s", golden, current.activeVersion, on))
			continue
		}
		baseline.Role = domain.GateRunBaseline
		decision.Runs = append(decision.Runs, baseline.EvaluationGateRun)
		if regression := baseline.ScoreBP - candidate.ScoreBP; regression > *policy.MaxRegressionBP {
			reasons = append(reasons, fmt.Sprintf("%s %s score regresses %d bp from release %s, more than %d bp", golden, policy.Metric,
				regression, current.activeVersion, *policy.MaxRegressionBP))
		}
	}
	decision.Verdict = domain.GateVerdictPass
	if len(reasons) > 0 {
		decision.Verdict, decision.Reason = domain.GateVerdictFail, truncate(strings.Join(reasons, "; "), maxGateReason)
	}
	return decision, nil
}

// scoredRun is a run's score with the count of metric values that are not 0..1 scores.
type scoredRun struct {
	domain.EvaluationGateRun
	invalid int64
}

// score finds the newest completed run of digest in scope on the current set version and averages
// metric over all of its results; a result the metric did not score counts 0, and several scores of
// the metric count their lowest.
func (g *StudioEvaluationGate) score(ctx context.Context, tenantID int64, agentID, digest string, current gateContext, golden domain.GoldenScope, metric string) (scoredRun, bool, error) {
	runs, err := g.qs.Query(ctx, qGateRun, tenantID, agentID, current.goldenSetID, current.setVersion, string(golden), digest)
	if err != nil {
		return scoredRun{}, false, fmt.Errorf("find %s evaluation run: %w", golden, err)
	}
	if len(runs.Rows) == 0 {
		return scoredRun{}, false, nil
	}
	run := scoredRun{EvaluationGateRun: domain.EvaluationGateRun{RunID: common.AsInt64(runs.Rows[0][0]), Scope: golden}}
	limit := g.maxResults()
	results, err := g.qs.Query(ctx, qGateResults, tenantID, run.RunID, limit+1)
	if err != nil {
		return scoredRun{}, false, fmt.Errorf("load evaluation run %d results: %w", run.RunID, err)
	}
	if len(results.Rows) > limit {
		return scoredRun{}, false, fmt.Errorf("%w: evaluation run %d holds more than the %d results a gate decision reads", domain.ErrValidation, run.RunID, limit)
	}
	var total float64
	for _, row := range results.Rows {
		var scores []domain.EvaluationScore
		if raw := common.AsString(row[0]); raw != "" && raw != "null" {
			if err := json.Unmarshal([]byte(raw), &scores); err != nil {
				return scoredRun{}, false, fmt.Errorf("decode evaluation run %d scores: %w", run.RunID, err)
			}
		}
		value, critical := 0.0, false
		scored := false
		for _, item := range scores {
			critical = critical || item.Critical
			if item.Metric != metric {
				continue
			}
			if math.IsNaN(item.Value) || item.Value < 0 || item.Value > 1 {
				run.invalid++
				item.Value = 0
			}
			if !scored || item.Value < value {
				value, scored = item.Value, true
			}
		}
		if critical {
			run.Criticals++
		}
		run.Samples++
		total += value
	}
	if run.Samples > 0 {
		run.ScoreBP = min(int64(math.Floor(total*domain.MaxEvaluationGateScoreBP/float64(run.Samples)+1e-6)), domain.MaxEvaluationGateScoreBP)
	}
	return run, true, nil
}

// inputsDigest identifies what a decision was made from, so an evaluation of unchanged inputs replays it.
func inputsDigest(decision domain.EvaluationGateDecision) string {
	var payload strings.Builder
	fmt.Fprintf(&payload, "scout.gate.v1\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%d\x1f%s", decision.Verdict, decision.DefinitionDigest,
		decision.PolicyDigest, decision.BaseVersion, decision.GoldenSetID, decision.SetVersion, decision.Reason)
	if decision.Verdict == domain.GateVerdictOverride {
		fmt.Fprintf(&payload, "\x1f%s\x1f%s", decision.DecidedBy.Kind, decision.DecidedBy.ID)
	}
	for _, run := range decision.Runs {
		fmt.Fprintf(&payload, "\x1e%d\x1f%s\x1f%d\x1f%d\x1f%d", run.RunID, run.Role, run.ScoreBP, run.Samples, run.Criticals)
	}
	return sha256Hex(payload.String())
}

// record writes decision in place of any earlier one for its digest, with a Studio event and, for an
// override, its audit decision. A decision whose inputs are already recorded, and a failing one over
// an override that still holds, return what is stored.
func (g *StudioEvaluationGate) record(ctx context.Context, actor domain.StudioActor, current gateContext, decision domain.EvaluationGateDecision, event, detail string, evidence *domain.DecisionRecord) (domain.EvaluationGateDecision, error) {
	policy, err := json.Marshal(decision.Policy)
	if err != nil {
		return domain.EvaluationGateDecision{}, fmt.Errorf("encode evaluation gate: %w", err)
	}
	inputs := inputsDigest(decision)
	tx, err := g.Studio.DB.BeginTx(ctx, gateQueries)
	if err != nil {
		return domain.EvaluationGateDecision{}, fmt.Errorf("begin gate decision transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	key := []any{actor.TenantID, decision.AgentID, decision.DefinitionDigest}
	stored, err := tx.Query(ctx, qGateDecisionLocked, key...)
	if err != nil {
		return domain.EvaluationGateDecision{}, fmt.Errorf("load gate decision: %w", err)
	}
	if len(stored.Rows) > 0 {
		existing, err := g.scanDecision(ctx, tx, actor.TenantID, decision.AgentID, decision.DefinitionDigest, stored.Rows[0])
		if err != nil {
			return domain.EvaluationGateDecision{}, err
		}
		keepOverride := existing.Verdict == domain.GateVerdictOverride && decision.Verdict == domain.GateVerdictFail && len(staleness(existing, current)) == 0
		if keepOverride || strings.TrimSpace(common.AsString(stored.Rows[0][4])) == inputs {
			return existing, nil
		}
	}
	for _, drop := range []string{qGateDropRuns, qGateDrop} {
		if _, err = tx.Query(ctx, drop, key...); err != nil {
			return domain.EvaluationGateDecision{}, fmt.Errorf("replace gate decision: %w", err)
		}
	}
	var setID, setVersion any
	if decision.SetVersion > 0 {
		setID, setVersion = decision.GoldenSetID, decision.SetVersion
	}
	inserted, err := tx.Query(ctx, qGateInsert, actor.TenantID, decision.AgentID, decision.DefinitionDigest, string(decision.Verdict),
		nullableString(decision.Reason), decision.PolicyDigest, string(policy), inputs, nullableString(decision.BaseVersion),
		setID, setVersion, string(actor.Principal.Kind), actor.Principal.ID)
	if err != nil {
		return domain.EvaluationGateDecision{}, fmt.Errorf("insert gate decision: %w", err)
	}
	if len(inserted.Rows) == 0 {
		return domain.EvaluationGateDecision{}, fmt.Errorf("%w: a gate decision for this digest was recorded concurrently; retry", domain.ErrConflict)
	}
	decision.DecidedAt = common.AsTime(inserted.Rows[0][0])
	for _, run := range decision.Runs {
		if _, err = tx.Query(ctx, qGateInsertRun, actor.TenantID, decision.AgentID, decision.DefinitionDigest, run.RunID,
			string(run.Role), run.ScoreBP, run.Samples, run.Criticals); err != nil {
			return domain.EvaluationGateDecision{}, fmt.Errorf("insert gate decision run %d: %w", run.RunID, err)
		}
	}
	if _, err = tx.Query(ctx, qGateAudit, actor.TenantID, decision.AgentID, event, truncate(detail, maxGateReason),
		string(actor.Principal.Kind), actor.Principal.ID); err != nil {
		return domain.EvaluationGateDecision{}, fmt.Errorf("record studio audit: %w", err)
	}
	// The evidence is written before the commit; its key replays it if the commit must be retried.
	if evidence != nil {
		if err = g.Audit.Record(ctx, *evidence); err != nil {
			return domain.EvaluationGateDecision{}, fmt.Errorf("audit evaluation gate override: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.EvaluationGateDecision{}, fmt.Errorf("commit gate decision: %w", err)
	}
	committed = true
	return decision, nil
}

// decision reads the decision recorded for digest; nil when there is none.
func (g *StudioEvaluationGate) decision(ctx context.Context, qs keelport.QueryService, tenantID int64, agentID, digest string) (*domain.EvaluationGateDecision, error) {
	stored, err := qs.Query(ctx, qGateDecision, tenantID, agentID, digest)
	if err != nil {
		return nil, fmt.Errorf("load gate decision: %w", err)
	}
	if len(stored.Rows) == 0 {
		return nil, nil
	}
	decision, err := g.scanDecision(ctx, qs, tenantID, agentID, digest, stored.Rows[0])
	if err != nil {
		return nil, err
	}
	return &decision, nil
}

// scanDecision decodes one qGateDecision row and reads its runs through qs.
func (g *StudioEvaluationGate) scanDecision(ctx context.Context, qs keelport.QueryService, tenantID int64, agentID, digest string, row []any) (domain.EvaluationGateDecision, error) {
	if len(row) < 11 {
		return domain.EvaluationGateDecision{}, fmt.Errorf("gate decision row: expected 11 columns, got %d", len(row))
	}
	decision := domain.EvaluationGateDecision{
		AgentID: agentID, DefinitionDigest: digest, Verdict: domain.EvaluationGateVerdict(common.AsString(row[0])),
		Reason: common.AsString(row[1]), PolicyDigest: strings.TrimSpace(common.AsString(row[2])), BaseVersion: common.AsString(row[5]),
		GoldenSetID: common.AsString(row[6]), SetVersion: common.AsInt64(row[7]),
		DecidedBy: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[8])), ID: common.AsString(row[9])},
		DecidedAt: common.AsTime(row[10]),
	}
	policy, err := scope.DecodeEvaluationGate([]byte(common.AsString(row[3])))
	if err != nil {
		return domain.EvaluationGateDecision{}, fmt.Errorf("decode gate decision policy: %w", err)
	}
	decision.Policy = policy
	runs, err := qs.Query(ctx, qGateDecisionRuns, tenantID, agentID, digest)
	if err != nil {
		return domain.EvaluationGateDecision{}, fmt.Errorf("load gate decision runs: %w", err)
	}
	for _, run := range runs.Rows {
		if len(run) < 6 {
			return domain.EvaluationGateDecision{}, fmt.Errorf("gate decision run row: expected 6 columns, got %d", len(run))
		}
		decision.Runs = append(decision.Runs, domain.EvaluationGateRun{
			RunID: common.AsInt64(run[0]), Role: domain.GateRunRole(common.AsString(run[1])), Scope: domain.GoldenScope(common.AsString(run[2])),
			ScoreBP: common.AsInt64(run[3]), Samples: common.AsInt64(run[4]), Criticals: common.AsInt64(run[5]),
		})
	}
	return decision, nil
}
