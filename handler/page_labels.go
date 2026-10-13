package handler

import (
	"context"
	"fmt"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// pageLabels collects one response's refs and writes back the captions a labeler gives them.
type pageLabels struct {
	labels domain.Labels
	fill   []func()
}

func newPageLabels() *pageLabels {
	return &pageLabels{labels: domain.Labels{Tenants: map[int64]string{}, Principals: map[domain.ScopedPrincipalRef]string{}, Clients: map[domain.ScopedClientRef]string{}}}
}

func (p *pageLabels) principal(tenantID int64, ref *api.PrincipalRef) {
	if ref == nil {
		return
	}
	key := domain.ScopedPrincipalRef{TenantID: tenantID, PrincipalRef: domain.PrincipalRef{Kind: domain.PrincipalKind(ref.Kind), ID: ref.ID}}
	p.labels.Principals[key] = ""
	p.fill = append(p.fill, func() { ref.Label = p.labels.Principals[key] })
}

func (p *pageLabels) client(tenantID int64, ref string, label *string) {
	if ref == "" {
		return
	}
	key := domain.ScopedClientRef{TenantID: tenantID, ClientRef: ref}
	p.labels.Clients[key] = ""
	p.fill = append(p.fill, func() { *label = p.labels.Clients[key] })
}

func (p *pageLabels) confirmation(item *api.Confirmation) {
	p.labels.Tenants[item.TenantID] = ""
	p.fill = append(p.fill, func() { item.TenantLabel = p.labels.Tenants[item.TenantID] })
	p.client(item.TenantID, item.ClientRef, &item.ClientLabel)
	p.principal(item.TenantID, &item.Maker)
	p.principal(item.TenantID, item.Decider)
	p.principal(item.TenantID, item.Reconciler)
}

func (p *pageLabels) decision(tenantID int64, item *api.DecisionRecord) {
	p.client(tenantID, item.ClientRef, &item.ClientLabel)
	p.principal(tenantID, &item.Principal)
	if item.Authority != nil {
		p.principal(tenantID, &item.Authority.Subject)
		p.principal(tenantID, item.Authority.Grantor)
	}
}

func (p *pageLabels) apply(ctx context.Context, labeler contract.Labeler) error {
	if labeler == nil || len(p.fill) == 0 {
		return nil
	}
	if err := labeler.Label(ctx, p.labels); err != nil {
		return fmt.Errorf("label page: %v", err) // never a typed status: the page, not the person, failed
	}
	for _, fill := range p.fill {
		fill()
	}
	return nil
}
