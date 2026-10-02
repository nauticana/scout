package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	keelcache "github.com/nauticana/keel/cache"
	"github.com/nauticana/keel/common"
	keelmodel "github.com/nauticana/keel/model"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qPlatformLayerInsert   = "scout_platform_restriction_layer_insert"
	qPlatformCurrentRead   = "scout_platform_restriction_current_read"
	qPlatformCurrentLock   = "scout_platform_restriction_current_lock"
	qPlatformCurrentInsert = "scout_platform_restriction_current_insert"
	qPlatformCurrentSwap   = "scout_platform_restriction_current_swap"
	qTenantLayerInsert     = "scout_tenant_restriction_layer_insert"
	qTenantCurrentRead     = "scout_tenant_restriction_current_read"
	qTenantCurrentLock     = "scout_tenant_restriction_current_lock"
	qTenantCurrentInsert   = "scout_tenant_restriction_current_insert"
	qTenantCurrentSwap     = "scout_tenant_restriction_current_swap"

	platformLayerKey = "platform"

	// DefaultRestrictionTTL is how long a read layer is reused before the store is asked again.
	DefaultRestrictionTTL = 5 * time.Second
	// DefaultRestrictionEntries bounds the cached tenant layers.
	DefaultRestrictionEntries = 4096
)

var restrictionQueries = map[string]string{
	qPlatformLayerInsert: `
INSERT INTO platform_restriction_layer (layer_digest, denials, guardrail_rules)
VALUES (?, ?, ?)
ON CONFLICT (layer_digest) DO NOTHING`,
	qPlatformCurrentRead: `
SELECT l.layer_digest, l.denials, l.guardrail_rules
  FROM platform_current_restriction c
  JOIN platform_restriction_layer l ON l.layer_digest = c.layer_digest
 WHERE c.layer_key = 'platform'`,
	qPlatformCurrentLock: `
SELECT layer_digest
  FROM platform_current_restriction
 WHERE layer_key = 'platform'
   FOR UPDATE`,
	qPlatformCurrentInsert: `
INSERT INTO platform_current_restriction (layer_key, layer_digest)
VALUES ('platform', ?)
ON CONFLICT (layer_key) DO NOTHING
RETURNING layer_digest`,
	qPlatformCurrentSwap: `
UPDATE platform_current_restriction
   SET layer_digest = ?, updated_at = CURRENT_TIMESTAMP
 WHERE layer_key = 'platform' AND layer_digest = ?
RETURNING layer_digest`,
	qTenantLayerInsert: `
INSERT INTO tenant_restriction_layer (tenant_id, layer_digest, denials, guardrail_rules)
VALUES (?, ?, ?, ?)
ON CONFLICT (tenant_id, layer_digest) DO NOTHING`,
	qTenantCurrentRead: `
SELECT l.layer_digest, l.denials, l.guardrail_rules
  FROM tenant_current_restriction c
  JOIN tenant_restriction_layer l ON l.tenant_id = c.tenant_id AND l.layer_digest = c.layer_digest
 WHERE c.tenant_id = ?`,
	qTenantCurrentLock: `
SELECT layer_digest
  FROM tenant_current_restriction
 WHERE tenant_id = ?
   FOR UPDATE`,
	qTenantCurrentInsert: `
INSERT INTO tenant_current_restriction (tenant_id, layer_digest)
VALUES (?, ?)
ON CONFLICT (tenant_id) DO NOTHING
RETURNING layer_digest`,
	qTenantCurrentSwap: `
UPDATE tenant_current_restriction
   SET layer_digest = ?, updated_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND layer_digest = ?
RETURNING layer_digest`,
}

// TableRestrictionLayers stores the platform and tenant restriction layers and
// reads the ones in force. Reads are cached for TTL in this process, so a
// replaced layer reaches running agents within TTL without a redeploy or
// republish. A layer never written is empty.
type TableRestrictionLayers struct {
	DB port.DatabaseRepository
	// Rules validates guardrail rules at write time; required to write any.
	Rules contract.GuardrailRuleCompiler
	// Audit records every change; required to write.
	Audit contract.AuditSink
	// TTL defaults to DefaultRestrictionTTL; Entries to DefaultRestrictionEntries.
	TTL     time.Duration
	Entries int

	once  sync.Once
	qs    port.QueryService
	cache *keelcache.LRU[int64, domain.RestrictionLayer]
}

var (
	_ contract.RestrictionLayerReader = (*TableRestrictionLayers)(nil)
	_ contract.RestrictionLayerWriter = (*TableRestrictionLayers)(nil)
)

func (store *TableRestrictionLayers) init(ctx context.Context) error {
	if store.DB == nil {
		return fmt.Errorf("restriction layers: database is required")
	}
	store.once.Do(func() {
		store.qs = store.DB.GetQueryService(ctx, restrictionQueries)
		entries := store.Entries
		if entries <= 0 {
			entries = DefaultRestrictionEntries
		}
		store.cache = keelcache.NewLRU[int64, domain.RestrictionLayer](entries, nil)
	})
	if store.qs == nil {
		return fmt.Errorf("restriction layers: query service is required")
	}
	return nil
}

func (store *TableRestrictionLayers) ttl() time.Duration {
	if store.TTL > 0 {
		return store.TTL
	}
	return DefaultRestrictionTTL
}

// Layers returns the platform layer and the tenant's; the platform is cached under key 0.
func (store *TableRestrictionLayers) Layers(ctx context.Context, tenantID int64) (domain.RestrictionLayers, error) {
	if tenantID <= 0 {
		return domain.RestrictionLayers{}, fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	if err := store.init(ctx); err != nil {
		return domain.RestrictionLayers{}, err
	}
	platform, err := store.read(ctx, 0, qPlatformCurrentRead)
	if err != nil {
		return domain.RestrictionLayers{}, fmt.Errorf("read platform restriction layer: %w", err)
	}
	tenant, err := store.read(ctx, tenantID, qTenantCurrentRead, tenantID)
	if err != nil {
		return domain.RestrictionLayers{}, fmt.Errorf("read restriction layer of tenant %d: %w", tenantID, err)
	}
	return domain.RestrictionLayers{Platform: platform, Tenant: tenant}, nil
}

func (store *TableRestrictionLayers) read(ctx context.Context, key int64, query string, args ...any) (domain.RestrictionLayer, error) {
	if cached, ok := store.cache.Get(key); ok {
		return cached, nil
	}
	result, err := store.qs.Query(ctx, query, args...)
	if err != nil {
		return domain.RestrictionLayer{}, err
	}
	var layer domain.RestrictionLayer
	if len(result.Rows) > 0 {
		row := result.Rows[0]
		if layer, err = decodeLayer(common.AsString(row[0]), common.AsString(row[1]), common.AsString(row[2])); err != nil {
			return domain.RestrictionLayer{}, err
		}
	}
	store.cache.Set(key, layer, store.ttl())
	return layer, nil
}

// ReplacePlatform swaps the platform layer.
func (store *TableRestrictionLayers) ReplacePlatform(ctx context.Context, actor domain.Principal, layer domain.RestrictionLayer, expectedDigest string) (string, error) {
	return store.replace(ctx, actor, 0, layer, expectedDigest)
}

// ReplaceTenant swaps one tenant's layer.
func (store *TableRestrictionLayers) ReplaceTenant(ctx context.Context, actor domain.Principal, tenantID int64, layer domain.RestrictionLayer, expectedDigest string) (string, error) {
	if tenantID <= 0 {
		return "", fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	return store.replace(ctx, actor, tenantID, layer, expectedDigest)
}

func (store *TableRestrictionLayers) replace(ctx context.Context, actor domain.Principal, tenantID int64, layer domain.RestrictionLayer, expectedDigest string) (string, error) {
	actor.ID = strings.TrimSpace(actor.ID)
	if actor.Kind != domain.PrincipalService || actor.ID == "" {
		return "", fmt.Errorf("%w: only a service principal may replace a restriction layer", domain.ErrForbidden)
	}
	if store.Audit == nil {
		return "", fmt.Errorf("%w: restriction layer changes need an audit sink", domain.ErrNotReady)
	}
	if err := store.init(ctx); err != nil {
		return "", err
	}
	encoded, err := encodeLayer(ctx, store.Rules, layer)
	if err != nil {
		return "", err
	}
	digest := encoded.layer.Digest
	current, changed, err := store.swap(ctx, tenantID, encoded, expectedDigest)
	if err != nil || !changed {
		return current, err
	}
	store.cache.Delete(tenantID)
	payload, err := json.Marshal(map[string]any{"previous_digest": expectedDigest, "layer_digest": digest,
		"denials": len(encoded.layer.Denials), "guardrails": len(encoded.layer.Guardrails)})
	if err != nil {
		return digest, fmt.Errorf("encode restriction audit: %w", err)
	}
	resource := "platform"
	if tenantID > 0 {
		resource = "tenant"
	}
	if err = store.Audit.Record(ctx, domain.DecisionRecord{
		TenantID: tenantID, Principal: domain.PrincipalRef{Kind: actor.Kind, ID: actor.ID},
		Category: domain.DecisionCategoryRestriction, Action: "replace", Resource: resource,
		PolicyVersion: digest, Outcome: domain.DecisionAllow, Payload: payload,
	}); err != nil {
		return digest, fmt.Errorf("audit restriction layer change: %w", err)
	}
	return digest, nil
}

// swap stores the version and moves the pointer when it still holds expectedDigest.
func (store *TableRestrictionLayers) swap(ctx context.Context, tenantID int64, encoded encodedLayer, expectedDigest string) (string, bool, error) {
	digest := encoded.layer.Digest
	insertLayer, lock, insertCurrent, swap, read := qPlatformLayerInsert, qPlatformCurrentLock, qPlatformCurrentInsert, qPlatformCurrentSwap, qPlatformCurrentRead
	layerArgs, keyArgs := []any{digest, string(encoded.denials), string(encoded.guardrails)}, []any{}
	if tenantID > 0 {
		insertLayer, lock, insertCurrent, swap, read = qTenantLayerInsert, qTenantCurrentLock, qTenantCurrentInsert, qTenantCurrentSwap, qTenantCurrentRead
		layerArgs, keyArgs = append([]any{tenantID}, layerArgs...), []any{tenantID}
	}
	tx, err := store.DB.BeginTx(ctx, restrictionQueries)
	if err != nil {
		return "", false, fmt.Errorf("begin restriction layer swap: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	locked, err := tx.Query(ctx, lock, keyArgs...)
	if err != nil {
		return "", false, fmt.Errorf("lock restriction layer: %w", err)
	}
	current := ""
	if len(locked.Rows) > 0 {
		current = common.AsString(locked.Rows[0][0])
	}
	if current == digest {
		return current, false, nil
	}
	if current != expectedDigest {
		return current, false, fmt.Errorf("%w: restriction layer is at %q, not %q", domain.ErrConflict, current, expectedDigest)
	}
	if _, err = tx.Query(ctx, insertLayer, layerArgs...); err != nil {
		return "", false, fmt.Errorf("insert restriction layer: %w", err)
	}
	var moved *keelmodel.QueryResult
	if current == "" {
		moved, err = tx.Query(ctx, insertCurrent, append(keyArgs, digest)...)
	} else {
		moved, err = tx.Query(ctx, swap, append([]any{digest}, append(keyArgs, current)...)...)
	}
	if err != nil {
		return "", false, fmt.Errorf("move restriction layer: %w", err)
	}
	if len(moved.Rows) == 0 {
		// Only a first write races past the row lock; report what won.
		_ = tx.Rollback(ctx)
		committed = true
		now, err := store.qs.Query(ctx, read, keyArgs...)
		if err != nil {
			return "", false, fmt.Errorf("read concurrently written restriction layer: %w", err)
		}
		if len(now.Rows) > 0 {
			current = common.AsString(now.Rows[0][0])
		}
		if current == digest {
			return current, false, nil
		}
		return current, false, fmt.Errorf("%w: restriction layer was written concurrently", domain.ErrConflict)
	}
	if err = tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("commit restriction layer swap: %w", err)
	}
	committed = true
	return digest, true, nil
}
