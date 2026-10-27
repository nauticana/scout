package controlplane

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

// AgentPublisher validates, compiles, and atomically stores immutable agent versions.
type AgentPublisher struct {
	Compiler contract.AgentCompiler
	Store    contract.AgentPublicationStore
	// Effective and Releases freeze the scope chain into the release every turn and
	// tool call reads; a binding that broadens what it inherits fails here.
	Effective contract.EffectiveConfigCompiler
	Releases  contract.EffectiveReleaseRepository
	// ScopeID is the scope a published agent compiles against.
	ScopeID string
	// Currencies checks a cost budget's unit; defaults to keel's catalog over DB, and a release
	// with a cost budget is refused without either.
	Currencies scope.CurrencyCatalog
	DB         keelport.DatabaseRepository

	once sync.Once
}

// Publish compiles and persists one immutable definition without activating traffic.
func (publisher *AgentPublisher) Publish(ctx context.Context, tenantID int64, definition domain.AgentDefinition) error {
	if tenantID <= 0 || strings.TrimSpace(definition.AgentID) == "" || strings.TrimSpace(definition.Version) == "" {
		return fmt.Errorf("%w: tenant, agent, and version are required", domain.ErrValidation)
	}
	if !validSHA256(definition.DefinitionDigest) {
		return fmt.Errorf("%w: definition digest must contain 64 hexadecimal characters", domain.ErrValidation)
	}
	if publisher.Compiler == nil || publisher.Store == nil || publisher.Effective == nil || publisher.Releases == nil {
		return fmt.Errorf("agent publisher: compiler, publication store, effective compiler, and release repository are required")
	}
	graph, err := publisher.Compiler.Compile(ctx, definition)
	if err != nil {
		return fmt.Errorf("compile agent %q version %q: %w", definition.AgentID, definition.Version, err)
	}
	if graph.AgentID != definition.AgentID || graph.Version != definition.Version || graph.EntryStepID == "" || !validSHA256(graph.Digest) {
		return fmt.Errorf("%w: compiler returned an inconsistent execution graph", domain.ErrValidation)
	}
	// The settings are compiled and checked before the immutable version is stored, so a
	// refused budget leaves no version that could never serve a turn.
	release, err := publisher.compile(ctx, tenantID, definition)
	if err != nil {
		return err
	}
	if err := publisher.Store.Publish(ctx, tenantID, definition, graph); err != nil {
		return fmt.Errorf("publish agent %q version %q: %w", definition.AgentID, definition.Version, err)
	}
	if err := publisher.Releases.Put(ctx, release); err != nil {
		return fmt.Errorf("store effective release for %q version %q: %w", definition.AgentID, definition.Version, err)
	}
	return nil
}

func (publisher *AgentPublisher) compile(ctx context.Context, tenantID int64, definition domain.AgentDefinition) (domain.EffectiveRelease, error) {
	release, err := publisher.Effective.Compile(ctx, domain.CompileRequest{
		TenantID: tenantID, AgentID: definition.AgentID, AgentVersion: definition.Version,
		ScopeID: publisher.ScopeID, CompiledBy: definition.PublishedBy,
	})
	if err != nil {
		return domain.EffectiveRelease{}, fmt.Errorf("compile effective release for %q version %q: %w", definition.AgentID, definition.Version, err)
	}
	if err := publisher.checkBudget(ctx, release.Resources); err != nil {
		return domain.EffectiveRelease{}, fmt.Errorf("budget of %q version %q: %w", definition.AgentID, definition.Version, err)
	}
	return scope.WithoutOperationalSettings(release), nil
}

// checkBudget refuses a cost ceiling whose currency or frozen exponent the catalog does not match.
func (publisher *AgentPublisher) checkBudget(ctx context.Context, resources []domain.EffectiveResource) error {
	for _, resource := range resources {
		if resource.ResourceKind != domain.ResourceBudget {
			continue
		}
		budget, err := scope.DecodeBudget(resource.Value)
		if err != nil || budget.CostMinorUnits == 0 {
			return err
		}
		if publisher.Currencies == nil && publisher.DB == nil {
			return fmt.Errorf("agent publisher: a currency catalog or database is required to check a cost budget's currency")
		}
		publisher.once.Do(func() { publisher.Currencies = scope.Currencies(publisher.Currencies, publisher.DB) })
		_, err = scope.CheckBudgetUnit(ctx, publisher.Currencies, budget)
		if unenforceable, ok := errors.AsType[*domain.BudgetUnenforceableError](err); ok {
			return fmt.Errorf("%w: %s", domain.ErrValidation, unenforceable.Detail)
		}
		return err
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

var _ contract.AgentPublisher = (*AgentPublisher)(nil)
