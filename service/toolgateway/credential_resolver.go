package toolgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qCredentialBinding = "scout_credential_binding"
	qCredentialRevoked = "scout_credential_revoked"
)

var credentialQueries = map[string]string{
	qCredentialBinding: `
SELECT b.credential_ref, b.delegated_from_user_id, b.grant_id, b.scopes, b.max_ttl_seconds,
       b.begda, b.endda, b.revoked_at
  FROM tool_credential_binding b
  LEFT JOIN delegation_grant g ON g.tenant_id = b.tenant_id AND g.grant_id = b.grant_id
 WHERE b.tenant_id = ? AND b.principal_kind = ? AND b.principal_id = ? AND b.tool_id = ? AND b.purpose = ?
   AND b.begda <= CURRENT_TIMESTAMP AND (b.endda IS NULL OR b.endda > CURRENT_TIMESTAMP)
   AND b.revoked_at IS NULL
   AND (b.grant_id IS NULL OR (g.revoked_at IS NULL AND g.begda <= CURRENT_TIMESTAMP AND (g.endda IS NULL OR g.endda > CURRENT_TIMESTAMP)))
 ORDER BY b.begda DESC
 LIMIT 1`,
	qCredentialRevoked: `
SELECT b.tenant_id, b.principal_kind, b.principal_id, b.tool_id, b.purpose, b.credential_ref,
       COALESCE(b.revoked_at, g.revoked_at),
       CASE WHEN b.endda IS NULL THEN g.endda WHEN g.endda IS NULL THEN b.endda
            WHEN b.endda < g.endda THEN b.endda ELSE g.endda END,
       b.grant_id
  FROM tool_credential_binding b
  LEFT JOIN delegation_grant g ON g.tenant_id = b.tenant_id AND g.grant_id = b.grant_id
 WHERE b.tenant_id = ?
   AND (COALESCE(b.revoked_at, g.revoked_at) >= ?
        OR (b.endda IS NOT NULL AND b.endda >= ? AND b.endda <= CURRENT_TIMESTAMP)
        OR (g.endda IS NOT NULL AND g.endda >= ? AND g.endda <= CURRENT_TIMESTAMP))`,
}

// TableCredentialBindings resolves the reference bound to one principal for one
// tool. It returns a pointer into the keel secret store, never secret material,
// so a binding is safe to log, cache, and audit.
type TableCredentialBindings struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

func (r *TableCredentialBindings) init(ctx context.Context) error {
	if r.DB == nil {
		return fmt.Errorf("credential bindings: database is required")
	}
	r.once.Do(func() { r.qs = r.DB.GetQueryService(ctx, credentialQueries) })
	if r.qs == nil {
		return fmt.Errorf("credential bindings: query service is required")
	}
	return nil
}

// Binding returns the in-force binding, or domain.ErrForbidden when none exists.
func (r *TableCredentialBindings) Binding(ctx context.Context, principal domain.Principal, toolID, purpose string) (domain.CredentialBinding, error) {
	if err := r.init(ctx); err != nil {
		return domain.CredentialBinding{}, err
	}
	if principal.Kind == "" || strings.TrimSpace(principal.ID) == "" || strings.TrimSpace(toolID) == "" {
		return domain.CredentialBinding{}, fmt.Errorf("%w: principal and tool are required", domain.ErrValidation)
	}
	result, err := r.qs.Query(ctx, qCredentialBinding, principal.TenantID, string(principal.Kind), principal.ID, toolID, purpose)
	if err != nil {
		return domain.CredentialBinding{}, fmt.Errorf("load credential binding: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.CredentialBinding{}, fmt.Errorf("%w: principal %q has no %s credential for tool %q",
			domain.ErrForbidden, principal.ID, purpose, toolID)
	}
	row := result.Rows[0]
	validTo, _ := common.AsTimeOK(row[6])
	binding := domain.CredentialBinding{
		TenantID: principal.TenantID, Principal: domain.PrincipalRef{Kind: principal.Kind, ID: principal.ID},
		ToolID: toolID, CredentialRef: common.AsString(row[0]), GrantID: common.AsString(row[2]), Purpose: purpose,
		MaxTTL: time.Duration(common.AsInt64(row[4])) * time.Second, ValidFrom: common.AsTime(row[5]), ValidTo: validTo,
	}
	if delegated := common.AsInt64(row[1]); delegated > 0 {
		binding.DelegatedFrom = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: fmt.Sprintf("%d", delegated)}
	}
	if scopes := common.AsString(row[3]); scopes != "" {
		if err := json.Unmarshal([]byte(scopes), &binding.Scopes); err != nil {
			return domain.CredentialBinding{}, fmt.Errorf("%w: credential scopes must be a JSON array: %v", domain.ErrValidation, err)
		}
	}
	return binding, nil
}

// Revoked lists bindings whose delegation ended, so in-flight work can be stopped.
func (r *TableCredentialBindings) Revoked(ctx context.Context, tenantID int64, since time.Time) ([]domain.CredentialBinding, error) {
	if err := r.init(ctx); err != nil {
		return nil, err
	}
	result, err := r.qs.Query(ctx, qCredentialRevoked, tenantID, since, since, since)
	if err != nil {
		return nil, fmt.Errorf("load revoked credential bindings: %w", err)
	}
	bindings := make([]domain.CredentialBinding, 0, len(result.Rows))
	for _, row := range result.Rows {
		revoked, _ := common.AsTimeOK(row[6])
		validTo, _ := common.AsTimeOK(row[7])
		bindings = append(bindings, domain.CredentialBinding{
			TenantID: common.AsInt64(row[0]),
			Principal: domain.PrincipalRef{
				Kind: domain.PrincipalKind(common.AsString(row[1])), ID: common.AsString(row[2]),
			},
			ToolID: common.AsString(row[3]), Purpose: common.AsString(row[4]),
			CredentialRef: common.AsString(row[5]), RevokedAt: revoked, ValidTo: validTo, GrantID: common.AsString(row[8]),
		})
	}
	return bindings, nil
}

// SecretResolver exchanges a binding reference for a short-lived credential. It
// is the keel secret provider or an external workload-identity issuer.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string, scopes []string, ttl time.Duration) ([]byte, error)
}

// BoundCredentialProvider resolves a credential just in time for one authorized
// call. Two agents on the same tool version resolve different bindings, so no
// two principals ever share an indistinguishable identity.
type BoundCredentialProvider struct {
	Bindings contract.CredentialBindingRepository
	Secrets  SecretResolver
	// DefaultTTL bounds an issued credential when the binding sets no MaxTTL.
	DefaultTTL time.Duration
}

// Credential returns the secret and the authority the call exercises.
func (p *BoundCredentialProvider) Credential(ctx context.Context, principal domain.Principal, toolID, action, purpose string) ([]byte, domain.AuthorityRef, error) {
	if p.Bindings == nil || p.Secrets == nil {
		return nil, domain.AuthorityRef{}, fmt.Errorf("credential provider: bindings and a secret resolver are required")
	}
	binding, err := p.Bindings.Binding(ctx, principal, toolID, purpose)
	if err != nil {
		return nil, domain.AuthorityRef{}, err
	}
	ttl := binding.MaxTTL
	if ttl <= 0 {
		ttl = p.DefaultTTL
	}
	secret, err := p.Secrets.Resolve(ctx, binding.CredentialRef, binding.Scopes, ttl)
	if err != nil {
		return nil, domain.AuthorityRef{}, fmt.Errorf("resolve credential for %q: %w", toolID, err)
	}
	authority := domain.AuthorityRef{Subject: domain.PrincipalRef{Kind: principal.Kind, ID: principal.ID}}
	switch {
	case binding.DelegatedFrom.ID != "":
		authority.Grantor = binding.DelegatedFrom
		authority.GrantID = binding.GrantID
	case len(principal.Authority) > 0:
		authority.Grantor = principal.Authority[0].Grantor
		authority.GrantID = principal.Authority[0].GrantID
	}
	return secret, authority, nil
}

var (
	_ contract.CredentialBindingRepository = (*TableCredentialBindings)(nil)
	_ contract.CredentialRevoker           = (*TableCredentialBindings)(nil)
	_ contract.ToolCredentialProvider      = (*BoundCredentialProvider)(nil)
)
