package contract

import (
	"context"
	"time"

	"github.com/nauticana/scout/domain"
)

// ScopeRepository reads the configuration hierarchy and the bindings attached to it.
type ScopeRepository interface {
	// Chain returns the ancestry of a scope ordered widest first.
	Chain(ctx context.Context, tenantID int64, scopeID string) (domain.ScopeChain, error)
	// Bindings returns every binding in force at asOf for the given scopes.
	Bindings(ctx context.Context, tenantID int64, scopeIDs []string, asOf time.Time) ([]domain.ScopedBinding, error)
}

// ResourceMerger combines a child binding with the value it inherits. One merger
// is registered per resource kind; an unregistered kind fails compilation.
type ResourceMerger interface {
	Kind() domain.ResourceKind
	Merge(ctx context.Context, inherited []byte, override domain.ScopedBinding) ([]byte, error)
}

// ResourceMergerRegistry resolves mergers by resource kind.
type ResourceMergerRegistry interface {
	MergerFor(ctx context.Context, kind domain.ResourceKind) (ResourceMerger, error)
}

// NarrowingChecker enforces monotonic restriction: a narrower scope may reduce
// what it inherits and never broaden it. Broadening is domain.ErrAuthorityExceeded
// at publication, not a runtime rejection.
type NarrowingChecker interface {
	CheckNarrowing(ctx context.Context, kind domain.ResourceKind, inherited, candidate []byte) error
}

// EffectiveConfigCompiler compiles a scope chain into the immutable release the
// runtime pins. Compilation happens at publication; the runtime never walks the
// chain on the request path.
type EffectiveConfigCompiler interface {
	Compile(ctx context.Context, request domain.CompileRequest) (domain.EffectiveRelease, error)
}

// PrincipalLimits resolves the limits frozen into a principal's effective
// release. The tenant envelope always binds on top of whatever these return.
type PrincipalLimits interface {
	// Budget returns the principal's token and cost ceiling.
	Budget(ctx context.Context, principal domain.Principal) (domain.BudgetLimits, error)
	// AutonomyMode returns the mode the principal may act under at a point in
	// time; outside its operating window a bounded mode degrades rather than fails.
	AutonomyMode(ctx context.Context, principal domain.Principal, at time.Time) (domain.AutonomyMode, error)
}

// EffectiveConfigExplainer answers "why is this the value?" from the provenance
// frozen into a release, without recompiling the scope chain.
type EffectiveConfigExplainer interface {
	Explain(ctx context.Context, tenantID int64, agentID, agentVersion string) ([]domain.ResourceExplanation, error)
	// Diff compares two compiled releases of one agent, by resource.
	Diff(ctx context.Context, tenantID int64, agentID, fromVersion, toVersion string) ([]domain.ResourceDiff, error)
}

// EffectiveReleaseRepository stores compiled releases by agent version.
type EffectiveReleaseRepository interface {
	Put(ctx context.Context, release domain.EffectiveRelease) error
	Get(ctx context.Context, tenantID int64, agentID, agentVersion string) (domain.EffectiveRelease, error)
}
