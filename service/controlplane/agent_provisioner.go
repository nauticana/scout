package controlplane

import (
	"context"
	"fmt"
	"strings"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qProvisionTenant  = "scout_provision_tenant"
	qProvisionProfile = "scout_provision_profile"
	qProvisionDraft   = "scout_provision_draft"
	qProvisionAlias   = "scout_provision_alias"
)

// Every statement is idempotent, so a repeated provision run is a no-op.
var provisionQueries = map[string]string{
	qProvisionTenant: `
INSERT INTO agent_tenant (partner_id, tenant_key, home_region)
VALUES (?, ?, ?)
ON CONFLICT (partner_id) DO NOTHING`,
	qProvisionProfile: `
INSERT INTO agent_profile (tenant_id, agent_id, agent_kind, display_name, is_active)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, agent_id) DO NOTHING`,
	qProvisionDraft: `
INSERT INTO agent_draft (tenant_id, agent_id, enabled, require_approval,
       text_model_provider, text_model_id, image_model_provider, image_model_id,
       video_model_provider, video_model_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, agent_id) DO NOTHING`,
	qProvisionAlias: `
INSERT INTO agent_alias (tenant_id, alias_id, agent_kind, agent_id)
VALUES (?, ?, ?, ?)
ON CONFLICT (tenant_id, alias_id) DO NOTHING`,
}

// AgentProvisioner seeds tenant registration, agent identities, drafts, and
// aliases in one transaction.
type AgentProvisioner struct {
	DB keelport.DatabaseRepository
}

var _ contract.AgentProvisioner = (*AgentProvisioner)(nil)

func (p *AgentProvisioner) Provision(ctx context.Context, tenantID int64, identity domain.TenantIdentity, seeds []domain.AgentSeed) error {
	if tenantID <= 0 {
		return fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	if p.DB == nil {
		return fmt.Errorf("agent provisioner: database is required")
	}
	for _, seed := range seeds {
		if err := validateSeed(seed); err != nil {
			return err
		}
	}
	tx, err := p.DB.BeginTx(ctx, provisionQueries)
	if err != nil {
		return fmt.Errorf("begin provisioning transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Query(ctx, qProvisionTenant, tenantID, identity.TenantKey, identity.HomeRegion); err != nil {
		return fmt.Errorf("register agent tenant: %w", err)
	}
	for _, seed := range seeds {
		if _, err = tx.Query(ctx, qProvisionProfile, tenantID, seed.AgentID, seed.AgentKind, seed.DisplayName, true); err != nil {
			return fmt.Errorf("seed agent profile %q: %w", seed.AgentID, err)
		}
		if _, err = tx.Query(ctx, qProvisionDraft, tenantID, seed.AgentID, seed.Enabled, seed.RequireApproval,
			modelProvider(seed.Models.Text), modelID(seed.Models.Text),
			modelProvider(seed.Models.Image), modelID(seed.Models.Image),
			modelProvider(seed.Models.Video), modelID(seed.Models.Video)); err != nil {
			return fmt.Errorf("seed agent draft %q: %w", seed.AgentID, err)
		}
		if seed.AliasID == "" {
			continue
		}
		if _, err = tx.Query(ctx, qProvisionAlias, tenantID, seed.AliasID, seed.AgentKind, seed.AgentID); err != nil {
			return fmt.Errorf("seed agent alias %q: %w", seed.AliasID, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit provisioning: %w", err)
	}
	committed = true
	return nil
}

func validateSeed(seed domain.AgentSeed) error {
	switch {
	case strings.TrimSpace(seed.AgentID) == "":
		return fmt.Errorf("%w: agent id is required", domain.ErrValidation)
	case strings.TrimSpace(seed.AgentKind) == "":
		return fmt.Errorf("%w: agent kind is required for %q", domain.ErrValidation, seed.AgentID)
	case strings.TrimSpace(seed.DisplayName) == "":
		return fmt.Errorf("%w: display name is required for %q", domain.ErrValidation, seed.AgentID)
	case seed.Models.Text == nil:
		return fmt.Errorf("%w: text model is required for %q", domain.ErrValidation, seed.AgentID)
	}
	return nil
}
