package fake

import (
	"context"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ApprovalAuthorizerFunc adapts an approval authorization function.
type ApprovalAuthorizerFunc func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) error

// AuthorizeApproval invokes the configured function.
func (function ApprovalAuthorizerFunc) AuthorizeApproval(ctx context.Context, request domain.ApprovalRequest, principal domain.PrincipalRef) error {
	return function(ctx, request, principal)
}

// PrincipalResolverFunc adapts a function to contract.PrincipalResolver.
type PrincipalResolverFunc func(context.Context, int64, domain.PrincipalRef) (domain.Principal, error)

// Resolve invokes the configured function.
func (function PrincipalResolverFunc) Resolve(ctx context.Context, tenantID int64, ref domain.PrincipalRef) (domain.Principal, error) {
	return function(ctx, tenantID, ref)
}

// PrincipalAuthorizerFunc adapts a function to contract.PrincipalAuthorizer.
type PrincipalAuthorizerFunc func(context.Context, domain.Principal, string, string, string) (domain.AuthorizationGrant, error)

// Authorize invokes the configured function.
func (function PrincipalAuthorizerFunc) Authorize(ctx context.Context, principal domain.Principal, object, action, value string) (domain.AuthorizationGrant, error) {
	return function(ctx, principal, object, action, value)
}

// DelegationVerifierFunc adapts a function to contract.DelegationVerifier.
type DelegationVerifierFunc func(context.Context, domain.Principal) (domain.AuthorityRef, error)

// Verify invokes the configured function.
func (function DelegationVerifierFunc) Verify(ctx context.Context, principal domain.Principal) (domain.AuthorityRef, error) {
	return function(ctx, principal)
}

// ScopeRepository contains configurable hierarchy and binding reads.
type ScopeRepository struct {
	ChainFunc    func(context.Context, int64, string) (domain.ScopeChain, error)
	BindingsFunc func(context.Context, int64, []string, time.Time) ([]domain.ScopedBinding, error)
}

// Chain invokes ChainFunc.
func (r *ScopeRepository) Chain(ctx context.Context, tenantID int64, scopeID string) (domain.ScopeChain, error) {
	return r.ChainFunc(ctx, tenantID, scopeID)
}

// Bindings invokes BindingsFunc.
func (r *ScopeRepository) Bindings(ctx context.Context, tenantID int64, scopeIDs []string, asOf time.Time) ([]domain.ScopedBinding, error) {
	return r.BindingsFunc(ctx, tenantID, scopeIDs, asOf)
}

// EffectiveConfigCompilerFunc adapts a function to contract.EffectiveConfigCompiler.
type EffectiveConfigCompilerFunc func(context.Context, domain.CompileRequest) (domain.EffectiveRelease, error)

// Compile invokes the configured function.
func (function EffectiveConfigCompilerFunc) Compile(ctx context.Context, request domain.CompileRequest) (domain.EffectiveRelease, error) {
	return function(ctx, request)
}

// NarrowingCheckerFunc adapts a function to contract.NarrowingChecker.
type NarrowingCheckerFunc func(context.Context, domain.ResourceKind, []byte, []byte) error

// CheckNarrowing invokes the configured function.
func (function NarrowingCheckerFunc) CheckNarrowing(ctx context.Context, kind domain.ResourceKind, inherited, candidate []byte) error {
	return function(ctx, kind, inherited, candidate)
}

var (
	_ contract.ApprovalAuthorizer      = ApprovalAuthorizerFunc(nil)
	_ contract.PrincipalResolver       = PrincipalResolverFunc(nil)
	_ contract.PrincipalAuthorizer     = PrincipalAuthorizerFunc(nil)
	_ contract.DelegationVerifier      = DelegationVerifierFunc(nil)
	_ contract.ScopeRepository         = (*ScopeRepository)(nil)
	_ contract.EffectiveConfigCompiler = EffectiveConfigCompilerFunc(nil)
	_ contract.NarrowingChecker        = NarrowingCheckerFunc(nil)
)

// PolicyDecisionPointFunc adapts a function to contract.PolicyDecisionPoint.
type PolicyDecisionPointFunc func(context.Context, domain.DecisionSubject) (domain.Decision, error)

// Decide invokes the configured function.
func (function PolicyDecisionPointFunc) Decide(ctx context.Context, subject domain.DecisionSubject) (domain.Decision, error) {
	return function(ctx, subject)
}

// PolicyResolverFunc adapts a function to contract.PolicyResolver.
type PolicyResolverFunc func(context.Context, domain.Principal) (domain.PolicySet, error)

// Policies invokes the configured function.
func (function PolicyResolverFunc) Policies(ctx context.Context, principal domain.Principal) (domain.PolicySet, error) {
	return function(ctx, principal)
}

// ObligationEnforcer contains a configurable obligation callback for one kind.
type ObligationEnforcer struct {
	ObligationKind domain.ObligationKind
	EnforceFunc    func(context.Context, domain.DecisionSubject, domain.Obligation) error
}

// Kind returns the configured obligation kind.
func (e *ObligationEnforcer) Kind() domain.ObligationKind { return e.ObligationKind }

// Enforce invokes EnforceFunc when configured.
func (e *ObligationEnforcer) Enforce(ctx context.Context, subject domain.DecisionSubject, obligation domain.Obligation) error {
	if e.EnforceFunc != nil {
		return e.EnforceFunc(ctx, subject, obligation)
	}
	return nil
}

// NotifierFunc adapts a function to contract.Notifier.
type NotifierFunc func(context.Context, domain.Notification) error

// Notify invokes the configured function.
func (function NotifierFunc) Notify(ctx context.Context, notification domain.Notification) error {
	return function(ctx, notification)
}

// AuditQueryFunc adapts a function to contract.AuditQuery.
type AuditQueryFunc func(context.Context, domain.DecisionQuery) (domain.DecisionPage, error)

// Decisions invokes the configured function.
func (function AuditQueryFunc) Decisions(ctx context.Context, query domain.DecisionQuery) (domain.DecisionPage, error) {
	return function(ctx, query)
}

var (
	_ contract.PolicyDecisionPoint = PolicyDecisionPointFunc(nil)
	_ contract.PolicyResolver      = PolicyResolverFunc(nil)
	_ contract.ObligationEnforcer  = (*ObligationEnforcer)(nil)
	_ contract.Notifier            = NotifierFunc(nil)
	_ contract.AuditQuery          = AuditQueryFunc(nil)
)

// ExternalPrincipalSourceFunc adapts a function to contract.ExternalPrincipalSource.
type ExternalPrincipalSourceFunc func(context.Context, string, string) (int64, domain.PrincipalRef, error)

// Lookup invokes the configured function.
func (function ExternalPrincipalSourceFunc) Lookup(ctx context.Context, issuer, subject string) (int64, domain.PrincipalRef, error) {
	return function(ctx, issuer, subject)
}

// EffectiveReleaseRepository contains configurable compiled-release storage.
type EffectiveReleaseRepository struct {
	PutFunc func(context.Context, domain.EffectiveRelease) error
	GetFunc func(context.Context, int64, string, string) (domain.EffectiveRelease, error)
}

// Put invokes PutFunc when configured.
func (r *EffectiveReleaseRepository) Put(ctx context.Context, release domain.EffectiveRelease) error {
	if r.PutFunc != nil {
		return r.PutFunc(ctx, release)
	}
	return nil
}

// Get invokes GetFunc when configured.
func (r *EffectiveReleaseRepository) Get(ctx context.Context, tenantID int64, agentID, agentVersion string) (domain.EffectiveRelease, error) {
	if r.GetFunc != nil {
		return r.GetFunc(ctx, tenantID, agentID, agentVersion)
	}
	return domain.EffectiveRelease{}, domain.ErrNotFound
}

// EffectiveConfigExplainer contains configurable explain and diff reads.
type EffectiveConfigExplainer struct {
	ExplainFunc func(context.Context, int64, string, string) ([]domain.ResourceExplanation, error)
	DiffFunc    func(context.Context, int64, string, string, string) ([]domain.ResourceDiff, error)
}

// Explain invokes ExplainFunc when configured.
func (e *EffectiveConfigExplainer) Explain(ctx context.Context, tenantID int64, agentID, agentVersion string) ([]domain.ResourceExplanation, error) {
	if e.ExplainFunc != nil {
		return e.ExplainFunc(ctx, tenantID, agentID, agentVersion)
	}
	return nil, nil
}

// Diff invokes DiffFunc when configured.
func (e *EffectiveConfigExplainer) Diff(ctx context.Context, tenantID int64, agentID, fromVersion, toVersion string) ([]domain.ResourceDiff, error) {
	if e.DiffFunc != nil {
		return e.DiffFunc(ctx, tenantID, agentID, fromVersion, toVersion)
	}
	return nil, nil
}

// PrincipalLimits contains configurable budget and autonomy reads.
type PrincipalLimits struct {
	BudgetFunc       func(context.Context, domain.Principal) (domain.BudgetLimits, error)
	AutonomyModeFunc func(context.Context, domain.Principal, time.Time) (domain.AutonomyMode, error)
}

// Budget invokes BudgetFunc when configured.
func (l *PrincipalLimits) Budget(ctx context.Context, principal domain.Principal) (domain.BudgetLimits, error) {
	if l.BudgetFunc != nil {
		return l.BudgetFunc(ctx, principal)
	}
	return domain.BudgetLimits{}, nil
}

// AutonomyMode invokes AutonomyModeFunc when configured; the default is the closed mode.
func (l *PrincipalLimits) AutonomyMode(ctx context.Context, principal domain.Principal, at time.Time) (domain.AutonomyMode, error) {
	if l.AutonomyModeFunc != nil {
		return l.AutonomyModeFunc(ctx, principal, at)
	}
	return domain.AutonomyHumanOnly, nil
}

// ResourceMergerFunc adapts a function and a kind to contract.ResourceMerger.
type ResourceMergerFunc struct {
	ResourceKind domain.ResourceKind
	MergeFunc    func(context.Context, []byte, domain.ScopedBinding) ([]byte, error)
}

// Kind returns the configured resource kind.
func (m ResourceMergerFunc) Kind() domain.ResourceKind { return m.ResourceKind }

// Merge invokes MergeFunc when configured; the default replaces.
func (m ResourceMergerFunc) Merge(ctx context.Context, inherited []byte, override domain.ScopedBinding) ([]byte, error) {
	if m.MergeFunc != nil {
		return m.MergeFunc(ctx, inherited, override)
	}
	return override.Value, nil
}

// ResourceMergerRegistryFunc adapts a function to contract.ResourceMergerRegistry.
type ResourceMergerRegistryFunc func(context.Context, domain.ResourceKind) (contract.ResourceMerger, error)

// MergerFor invokes the configured function.
func (function ResourceMergerRegistryFunc) MergerFor(ctx context.Context, kind domain.ResourceKind) (contract.ResourceMerger, error) {
	return function(ctx, kind)
}

var (
	_ contract.ExternalPrincipalSource    = ExternalPrincipalSourceFunc(nil)
	_ contract.EffectiveReleaseRepository = (*EffectiveReleaseRepository)(nil)
	_ contract.EffectiveConfigExplainer   = (*EffectiveConfigExplainer)(nil)
	_ contract.PrincipalLimits            = (*PrincipalLimits)(nil)
	_ contract.ResourceMerger             = ResourceMergerFunc{}
	_ contract.ResourceMergerRegistry     = ResourceMergerRegistryFunc(nil)
)
