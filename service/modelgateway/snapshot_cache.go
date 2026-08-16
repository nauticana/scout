package modelgateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// HMACSnapshotSigner authenticates snapshots with HMAC-SHA256 over the payload.
type HMACSnapshotSigner struct {
	Key []byte
}

var _ contract.SnapshotSigner = (*HMACSnapshotSigner)(nil)

// NewHMACSnapshotSigner requires a key of at least 32 bytes.
func NewHMACSnapshotSigner(key []byte) (*HMACSnapshotSigner, error) {
	if len(key) < sha256.Size {
		return nil, fmt.Errorf("snapshot signer: key must be at least %d bytes", sha256.Size)
	}
	return &HMACSnapshotSigner{Key: append([]byte(nil), key...)}, nil
}

func (signer *HMACSnapshotSigner) Sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, signer.Key)
	mac.Write(payload)
	return mac.Sum(nil)
}

func (signer *HMACSnapshotSigner) Verify(payload, signature []byte) bool {
	return hmac.Equal(signer.Sign(payload), signature)
}

// Snapshot kinds kept by SnapshotCache.
const (
	SnapshotKindCandidates    = "candidates"
	SnapshotKindRoutingPolicy = "routing_policy"
	SnapshotKindBudgetPolicy  = "budget_policy"
)

// SignedSnapshot is one authenticated, expiring copy of a control-plane view; the
// signature covers kind, scope, expiry, and payload so none can be altered alone.
type SignedSnapshot struct {
	Kind      string
	Scope     string
	Payload   []byte
	IssuedAt  time.Time
	ExpiresAt time.Time
	Signature []byte
}

func snapshotSigningInput(snapshot SignedSnapshot) []byte {
	header := snapshot.Kind + "\x00" + snapshot.Scope + "\x00" + strconv.FormatInt(snapshot.ExpiresAt.UnixNano(), 10) + "\x00"
	return append([]byte(header), snapshot.Payload...)
}

// SnapshotCache serves the last good candidate set, routing policy, and budget
// policy through a control-plane outage until each entry's TTL, then fails closed
// with ErrStaleEvidence. Entries are signed so a restored copy is verifiable.
type SnapshotCache struct {
	Catalog  contract.ModelCandidateCatalog
	Policies contract.TenantRoutingPolicyRepository
	Budgets  contract.TenantBudgetPolicy
	Signer   contract.SnapshotSigner
	TTL      time.Duration
	// MaxEntries bounds retained snapshots across kinds and tenants.
	MaxEntries int
	// OnFallback is told each time a cached snapshot covers a source failure.
	OnFallback func(kind string, scope string, cause error)
	Now        func() time.Time

	mu      sync.Mutex
	entries map[string]SignedSnapshot
}

var _ contract.ModelCandidateCatalog = (*SnapshotCache)(nil)
var _ contract.TenantRoutingPolicyRepository = (*SnapshotCache)(nil)
var _ contract.TenantBudgetPolicy = (*SnapshotCache)(nil)

// NewSnapshotCache requires a signer, a positive TTL and bound, and at least one source.
func NewSnapshotCache(catalog contract.ModelCandidateCatalog, policies contract.TenantRoutingPolicyRepository, budgets contract.TenantBudgetPolicy, signer contract.SnapshotSigner, ttl time.Duration, maxEntries int) (*SnapshotCache, error) {
	cache := &SnapshotCache{Catalog: catalog, Policies: policies, Budgets: budgets, Signer: signer, TTL: ttl, MaxEntries: maxEntries}
	if err := cache.validate(); err != nil {
		return nil, err
	}
	return cache, nil
}

func (cache *SnapshotCache) validate() error {
	if cache.Signer == nil || cache.TTL <= 0 || cache.MaxEntries <= 0 {
		return fmt.Errorf("snapshot cache: signer, positive TTL, and positive max entries are required")
	}
	if cache.Catalog == nil && cache.Policies == nil && cache.Budgets == nil {
		return fmt.Errorf("snapshot cache: at least one source is required")
	}
	return nil
}

func (cache *SnapshotCache) now() time.Time {
	if cache.Now == nil {
		return time.Now()
	}
	return cache.Now()
}

func entryKey(kind, scope string) string { return kind + "/" + scope }

// CandidatesFor serves the catalog, falling back to the signed last-good set.
func (cache *SnapshotCache) CandidatesFor(ctx context.Context, tenant domain.TenantContext) (domain.ModelCandidateSet, error) {
	var set domain.ModelCandidateSet
	if cache.Catalog == nil {
		return set, fmt.Errorf("snapshot cache: candidate catalog is not configured")
	}
	err := cache.serve(SnapshotKindCandidates, tenantScope(tenant.TenantID), &set, func() (any, error) {
		return cache.Catalog.CandidatesFor(ctx, tenant)
	})
	return set, err
}

// RoutingPolicyFor serves the routing policy, falling back to the signed last-good copy.
func (cache *SnapshotCache) RoutingPolicyFor(ctx context.Context, tenantID int64) (domain.RoutingPolicy, error) {
	var policy domain.RoutingPolicy
	if cache.Policies == nil {
		return policy, fmt.Errorf("snapshot cache: routing policy repository is not configured")
	}
	err := cache.serve(SnapshotKindRoutingPolicy, tenantScope(tenantID), &policy, func() (any, error) {
		return cache.Policies.RoutingPolicyFor(ctx, tenantID)
	})
	return policy, err
}

// BudgetFor serves the quota policy; an expired copy fails closed like the others.
func (cache *SnapshotCache) BudgetFor(ctx context.Context, tenantID int64) (domain.BudgetLimits, error) {
	var limits domain.BudgetLimits
	if cache.Budgets == nil {
		return limits, fmt.Errorf("snapshot cache: budget policy is not configured")
	}
	err := cache.serve(SnapshotKindBudgetPolicy, tenantScope(tenantID), &limits, func() (any, error) {
		return cache.Budgets.BudgetFor(ctx, tenantID)
	})
	return limits, err
}

func tenantScope(tenantID int64) string { return strconv.FormatInt(tenantID, 10) }

func (cache *SnapshotCache) serve(kind, scope string, target any, load func() (any, error)) error {
	if err := cache.validate(); err != nil {
		return err
	}
	value, sourceErr := load()
	if sourceErr == nil {
		payload, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode %s snapshot: %w", kind, err)
		}
		cache.store(kind, scope, payload)
		return json.Unmarshal(payload, target)
	}
	payload, err := cache.lastGood(kind, scope)
	if err != nil {
		return errors.Join(sourceErr, err)
	}
	if cache.OnFallback != nil {
		cache.OnFallback(kind, scope, sourceErr)
	}
	return json.Unmarshal(payload, target)
}

func (cache *SnapshotCache) store(kind, scope string, payload []byte) {
	now := cache.now()
	snapshot := SignedSnapshot{Kind: kind, Scope: scope, Payload: payload, IssuedAt: now, ExpiresAt: now.Add(cache.TTL)}
	snapshot.Signature = cache.Signer.Sign(snapshotSigningInput(snapshot))
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.putLocked(snapshot, now)
}

func (cache *SnapshotCache) putLocked(snapshot SignedSnapshot, now time.Time) {
	if cache.entries == nil {
		cache.entries = make(map[string]SignedSnapshot)
	}
	key := entryKey(snapshot.Kind, snapshot.Scope)
	if _, known := cache.entries[key]; !known && len(cache.entries) >= cache.MaxEntries {
		cache.evictLocked(now)
	}
	cache.entries[key] = snapshot
}

// evictLocked drops expired entries and, if still full, the earliest-expiring one.
func (cache *SnapshotCache) evictLocked(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for key, entry := range cache.entries {
		if !entry.ExpiresAt.After(now) {
			delete(cache.entries, key)
			continue
		}
		if oldestKey == "" || entry.ExpiresAt.Before(oldest) {
			oldestKey, oldest = key, entry.ExpiresAt
		}
	}
	if len(cache.entries) >= cache.MaxEntries && oldestKey != "" {
		delete(cache.entries, oldestKey)
	}
}

func (cache *SnapshotCache) lastGood(kind, scope string) ([]byte, error) {
	cache.mu.Lock()
	snapshot, ok := cache.entries[entryKey(kind, scope)]
	cache.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: no %s snapshot for scope %s", domain.ErrStaleEvidence, kind, scope)
	}
	if err := cache.verify(snapshot, cache.now()); err != nil {
		return nil, err
	}
	return snapshot.Payload, nil
}

func (cache *SnapshotCache) verify(snapshot SignedSnapshot, now time.Time) error {
	if !cache.Signer.Verify(snapshotSigningInput(snapshot), snapshot.Signature) {
		return fmt.Errorf("%w: %s snapshot signature mismatch for scope %s", domain.ErrValidation, snapshot.Kind, snapshot.Scope)
	}
	if !snapshot.ExpiresAt.After(now) {
		return fmt.Errorf("%w: %s snapshot for scope %s expired at %s", domain.ErrStaleEvidence, snapshot.Kind, snapshot.Scope, snapshot.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// Snapshot exports the retained signed copy so a downstream may persist it.
func (cache *SnapshotCache) Snapshot(kind string, tenantID int64) (SignedSnapshot, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	snapshot, ok := cache.entries[entryKey(kind, tenantScope(tenantID))]
	if ok {
		snapshot.Payload = append([]byte(nil), snapshot.Payload...)
		snapshot.Signature = append([]byte(nil), snapshot.Signature...)
	}
	return snapshot, ok
}

// Restore accepts a previously exported snapshot after verifying its signature and expiry.
func (cache *SnapshotCache) Restore(snapshot SignedSnapshot) error {
	if err := cache.validate(); err != nil {
		return err
	}
	switch snapshot.Kind {
	case SnapshotKindCandidates, SnapshotKindRoutingPolicy, SnapshotKindBudgetPolicy:
	default:
		return fmt.Errorf("%w: unknown snapshot kind %q", domain.ErrValidation, snapshot.Kind)
	}
	now := cache.now()
	if err := cache.verify(snapshot, now); err != nil {
		return err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.putLocked(snapshot, now)
	return nil
}
