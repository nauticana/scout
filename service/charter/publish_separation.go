package charter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/charter/sdk/authority"
	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// PublishSeparation reads two-person publish from a tenant's Charter documents: publishing an
// agent is separated from authoring it while a SodConstraint in force in the tenant's namespace
// constrains both the Author and the Publish capability at the agent's scope (CHR-AUTH-007).
// Excuse admits only a SodException in that namespace that relieves the person of that
// constraint now and leaves no other constraint separating them (CHR-AUTH-012).
type PublishSeparation struct {
	Documents corpus.Source
	// Namespace maps a tenant to the Charter namespace that owns its constraints and exceptions.
	Namespace func(tenantID int64) string
	// Author and Publish are the capabilities a constraint names, relative to the tenant's namespace.
	Author  model.Ref
	Publish model.Ref
	// Actor maps a Studio person to the Charter actor an exception names as its beneficiary.
	Actor func(ctx context.Context, tenantID int64, person domain.PrincipalRef) (model.ObjectRef, error)
}

var _ contract.PublishSeparation = (*PublishSeparation)(nil)

// Required reports whether a constraint in force separates authoring from publishing the agent.
func (p *PublishSeparation) Required(ctx context.Context, tenantID int64, agentID string, at time.Time) (bool, error) {
	namespace, constraints, err := p.constraints(ctx, tenantID, at)
	if err != nil {
		return false, err
	}
	return p.separated(namespace, constraints, agentID)
}

// Excuse fails closed unless the named exception relieves the person of every constraint
// separating authoring from publishing the agent at the instant.
func (p *PublishSeparation) Excuse(ctx context.Context, tenantID int64, agentID, exceptionID string, person domain.PrincipalRef, at time.Time) error {
	if p.Actor == nil {
		return fmt.Errorf("%w: charter publish separation needs an actor mapping", domain.ErrNotReady)
	}
	if strings.TrimSpace(exceptionID) == "" {
		return fmt.Errorf("%w: no separation exception is named", domain.ErrForbidden)
	}
	namespace, constraints, err := p.constraints(ctx, tenantID, at)
	if err != nil {
		return err
	}
	exception, err := corpus.ResolveAs[model.SodException](ctx, &corpus.AbstractDocumentProvider{Source: p.Documents},
		namespace, model.Ref{Namespace: namespace, ID: exceptionID}, model.KindSodException)
	if errors.Is(err, corpus.ErrNotFound) || errors.Is(err, corpus.ErrKindMismatch) {
		return fmt.Errorf("%w: separation exception %s is not in %s: %v", domain.ErrForbidden, exceptionID, namespace, err)
	}
	if err != nil {
		return fmt.Errorf("resolve separation exception %s: %w", exceptionID, err)
	}
	actor, err := p.Actor(ctx, tenantID, person)
	if err != nil {
		return fmt.Errorf("map %s:%s to a charter actor: %w", person.Kind, person.ID, err)
	}
	remaining, err := authority.Excuse(constraints, exception, namespace, actor, at)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrForbidden, err)
	}
	separated, err := p.separated(namespace, remaining, agentID)
	if err != nil {
		return err
	}
	if separated {
		return fmt.Errorf("%w: another constraint still separates authoring from publishing agent %s", domain.ErrForbidden, agentID)
	}
	return nil
}

func (p *PublishSeparation) constraints(ctx context.Context, tenantID int64, at time.Time) (string, []model.SodConstraint, error) {
	if p.Documents == nil || p.Namespace == nil || p.Author.ID == "" || p.Publish.ID == "" {
		return "", nil, fmt.Errorf("%w: charter publish separation needs documents, a namespace, and the author and publish capabilities", domain.ErrNotReady)
	}
	namespace := p.Namespace(tenantID)
	if namespace == "" {
		return "", nil, fmt.Errorf("%w: tenant %d has no charter namespace", domain.ErrNotReady, tenantID)
	}
	all, err := corpus.EffectiveAs[model.SodConstraint](ctx, &corpus.AbstractDocumentProvider{Source: p.Documents}, model.KindSodConstraint, at)
	if err != nil {
		return "", nil, fmt.Errorf("list separation constraints: %w", err)
	}
	var owned []model.SodConstraint
	for _, constraint := range all {
		if constraint.Namespace == namespace {
			owned = append(owned, constraint)
		}
	}
	return namespace, owned, nil
}

func (p *PublishSeparation) separated(namespace string, constraints []model.SodConstraint, agentID string) (bool, error) {
	scope := "agent:" + agentID
	evaluator := authority.SodEvaluator{Scopes: authority.ExactSodScopeMatcher{}}
	_, conflict, err := evaluator.Conflict(constraints,
		[]authority.SodAction{{Namespace: namespace, Capability: p.Author, Scope: scope}},
		authority.SodAction{Namespace: namespace, Capability: p.Publish, Scope: scope})
	if err != nil {
		return false, fmt.Errorf("evaluate publish separation: %w", err)
	}
	return conflict, nil
}
