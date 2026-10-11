package api

import "time"

const (
	// StudioGatePath shows the evaluation gate of an agent's current draft: GET ?agent_name=.
	StudioGatePath         = StudioBasePath + "gate"
	StudioGateEvaluatePath = StudioBasePath + "gate/evaluate"
	StudioGateOverridePath = StudioBasePath + "gate/override"
)

// EvaluationGatePolicy is the effective evaluation_gate setting. Scores and regressions are in
// basis points of a 0..1 metric; max_regression_bp is absent when regression is not bounded.
type EvaluationGatePolicy struct {
	Metric          string   `json:"metric"`
	MinScoreBP      int64    `json:"min_score_bp"`
	MaxRegressionBP *int64   `json:"max_regression_bp,omitempty"`
	MinSamples      int64    `json:"min_samples"`
	RequiredScopes  []string `json:"required_scopes"`
}

// EvaluationGateRun is one completed run a decision read: role candidate for the decided
// draft, baseline for the active release.
type EvaluationGateRun struct {
	RunID            int64  `json:"run_id"`
	Role             string `json:"role"`
	Scope            string `json:"scope"`
	ScoreBP          int64  `json:"score_bp"`
	Samples          int64  `json:"samples"`
	CriticalFailures int64  `json:"critical_failures"`
}

// EvaluationGateDecision is the gate's verdict (pass, fail, or override) on one definition digest.
type EvaluationGateDecision struct {
	DefinitionDigest string               `json:"definition_digest"`
	Verdict          string               `json:"verdict"`
	Reason           string               `json:"reason,omitempty"`
	Policy           EvaluationGatePolicy `json:"policy"`
	PolicyDigest     string               `json:"policy_digest"`
	BaseVersion      string               `json:"base_version,omitempty"`
	GoldenSetID      string               `json:"golden_set_id,omitempty"`
	SetVersion       int64                `json:"set_version,omitempty"`
	Runs             []EvaluationGateRun  `json:"runs"`
	DecidedByKind    string               `json:"decided_by_kind"`
	DecidedByID      string               `json:"decided_by_id"`
	DecidedAt        time.Time            `json:"decided_at"`
}

// EvaluationGateState is the gate of the agent's current draft. policy is absent when no gate
// applies; decision is the one recorded for current_digest; stale lists why it no longer holds;
// admitted reports whether the gate lets the draft publish; problem says why the draft does not freeze.
type EvaluationGateState struct {
	AgentName     string                  `json:"agent_name"`
	CurrentDigest string                  `json:"current_digest,omitempty"`
	Policy        *EvaluationGatePolicy   `json:"policy,omitempty"`
	PolicyDigest  string                  `json:"policy_digest,omitempty"`
	ActiveVersion string                  `json:"active_version,omitempty"`
	GoldenSetID   string                  `json:"golden_set_id,omitempty"`
	SetVersion    int64                   `json:"set_version,omitempty"`
	Decision      *EvaluationGateDecision `json:"decision,omitempty"`
	Stale         []string                `json:"stale"`
	Admitted      bool                    `json:"admitted"`
	Problem       string                  `json:"problem,omitempty"`
}

// EvaluationGateRequest decides the gate of agent_name's current draft.
type EvaluationGateRequest struct {
	AgentName string `json:"agent_name"`
}

// EvaluationGateOverrideRequest admits definition_digest, agent_name's current draft, despite its gate.
type EvaluationGateOverrideRequest struct {
	AgentName        string `json:"agent_name"`
	DefinitionDigest string `json:"definition_digest"`
	Reason           string `json:"reason"`
}
