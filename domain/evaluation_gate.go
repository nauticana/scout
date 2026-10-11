package domain

import (
	"strings"
	"time"
)

// StudioOverrideGate lets a person publish one exact draft that its evaluation gate does not admit.
const StudioOverrideGate StudioAction = "OVERRIDE_GATE"

// MaxEvaluationGateScoreBP is a whole score in basis points; gate scores and regressions run from 0 to it.
const MaxEvaluationGateScoreBP = 10000

// EvaluationGatePolicy is the evaluation_gate setting: what a draft's completed evaluations must
// show before it may be published. Every result of a run is a sample, and one the metric did
// not score counts as 0. MinScoreBP is the lowest mean of Metric, a 0..1 score, in basis points;
// MaxRegressionBP, when set, the most the mean may fall below the active release's on the same
// set version; RequiredScopes the golden scopes each needing a completed run.
type EvaluationGatePolicy struct {
	Metric          string        `json:"metric"`
	MinScoreBP      int64         `json:"min_score_bp"`
	MaxRegressionBP *int64        `json:"max_regression_bp,omitempty"`
	MinSamples      int64         `json:"min_samples"`
	RequiredScopes  []GoldenScope `json:"required_scopes"`
}

// EvaluationGateVerdict is an agent gate decision's outcome.
type EvaluationGateVerdict string

const (
	GateVerdictPass EvaluationGateVerdict = "pass"
	GateVerdictFail EvaluationGateVerdict = "fail"
	// GateVerdictOverride admits a digest the gate did not, on a person's recorded reason.
	GateVerdictOverride EvaluationGateVerdict = "override"
)

// GateRunRole says what a run contributed to a decision: the candidate's own score or the
// active release's baseline.
type GateRunRole string

const (
	GateRunCandidate GateRunRole = "candidate"
	GateRunBaseline  GateRunRole = "baseline"
)

// EvaluationGateRun is one completed evaluation run a decision rests on, with its score.
type EvaluationGateRun struct {
	RunID     int64
	Role      GateRunRole
	Scope     GoldenScope
	ScoreBP   int64
	Samples   int64
	Criticals int64
}

// EvaluationGateDecision is the gate's verdict on one definition digest of an agent: the
// policy it applied, the golden set version and runs it read, and the active release it
// compared against. A later decision for the same digest replaces it.
type EvaluationGateDecision struct {
	AgentID          string
	DefinitionDigest string
	Verdict          EvaluationGateVerdict
	Reason           string
	Policy           EvaluationGatePolicy
	PolicyDigest     string
	BaseVersion      string
	GoldenSetID      string
	SetVersion       int64
	Runs             []EvaluationGateRun
	DecidedBy        PrincipalRef
	DecidedAt        time.Time
}

// Reasons a decision no longer admits the digest it was made for.
const (
	GateStalePolicy     = "policy_changed"
	GateStaleRelease    = "active_release_changed"
	GateStaleSetVersion = "set_version_superseded"
)

// EvaluationGateState is an agent's gate as it stands for its current draft: the effective
// policy (nil when no gate applies), the active release and golden set version a decision
// is compared with now, and the decision recorded for the draft's digest, if any. Problem
// names why the draft does not freeze; it then has no digest.
type EvaluationGateState struct {
	AgentID       string
	CurrentDigest string
	Policy        *EvaluationGatePolicy
	PolicyDigest  string
	ActiveVersion string
	GoldenSetID   string
	SetVersion    int64
	Decision      *EvaluationGateDecision
	Stale         []string
	Admitted      bool
	Problem       string
}

// EvaluationGateOverride admits DefinitionDigest, which must be the agent's current draft, despite its gate.
type EvaluationGateOverride struct {
	AgentID          string
	DefinitionDigest string
	Reason           string
}

// Codes of an EvaluationGateRefusal.
const (
	GateRefusedMissing = "missing"
	GateRefusedFailed  = "failed"
	GateRefusedStale   = "stale"
)

// EvaluationGateRefusal is a publish the agent's evaluation gate does not admit: no decision
// for the digest, a failing one, or one made against another policy, active release, or set version.
type EvaluationGateRefusal struct {
	Code   string
	Detail string
}

func (e *EvaluationGateRefusal) Error() string {
	return strings.TrimSpace(ErrConflict.Error() + ": evaluation gate " + e.Code + ": " + e.Detail)
}

func (e *EvaluationGateRefusal) Unwrap() error { return ErrConflict }
