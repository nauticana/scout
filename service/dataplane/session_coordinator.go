package dataplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/lru"
	"github.com/nauticana/scout/internal/singleflight"
)

// SessionCoordinator keeps durable session state authoritative over its cache:
// the durable write completes first, an overlapping in-flight read never
// repopulates a superseded revision, and a cached revision older than one this
// process has written is discarded rather than served.
type SessionCoordinator struct {
	Store   contract.DurableSessionStore
	Cache   contract.HotSessionCache
	Metrics contract.RuntimeMetrics
	// CacheTimeout bounds invalidation after a durable write; default 1s.
	CacheTimeout time.Duration
	// RevisionMemory bounds how many conversations' last locally written
	// revisions are remembered for stale-hit detection; default 4096.
	RevisionMemory int

	flights singleflight.Group[domain.SessionKey, domain.SessionSnapshot]
	mu      sync.Mutex
	floors  *lru.Cache[domain.SessionKey, int64]
	reads   map[domain.SessionKey]*inflightRead
}

type inflightRead struct{ superseded bool }

const defaultRevisionMemory = 4096

// Load returns a valid cached snapshot or refreshes it from durable storage.
func (coordinator *SessionCoordinator) Load(ctx context.Context, tenantID int64, conversationID string) (domain.SessionSnapshot, error) {
	if tenantID <= 0 || strings.TrimSpace(conversationID) == "" {
		return domain.SessionSnapshot{}, fmt.Errorf("%w: tenant and conversation are required", domain.ErrValidation)
	}
	if err := coordinator.validate(); err != nil {
		return domain.SessionSnapshot{}, err
	}
	key := domain.SessionKey{TenantID: tenantID, ConversationID: conversationID}
	snapshot, found, err := coordinator.Cache.Get(ctx, tenantID, conversationID)
	switch {
	case err != nil:
		coordinator.recordCacheError(ctx, tenantID, "get", err)
	case found && snapshot.ConversationID != conversationID:
		coordinator.recordCacheError(ctx, tenantID, "get", fmt.Errorf("%w: cached session identity mismatch", domain.ErrConflict))
		_ = coordinator.invalidate(ctx, tenantID, conversationID)
	case found && snapshot.Revision < coordinator.floor(key):
		coordinator.recordCacheError(ctx, tenantID, "get", fmt.Errorf("%w: cached revision %d behind written %d", domain.ErrStaleEvidence, snapshot.Revision, coordinator.floor(key)))
		_ = coordinator.invalidate(ctx, tenantID, conversationID)
	case found:
		return snapshot, nil
	}
	// Concurrent misses for one conversation coalesce into a single durable load.
	return coordinator.flights.Do(ctx, key, func(loadCtx context.Context) (domain.SessionSnapshot, error) {
		read := coordinator.beginRead(key)
		defer coordinator.endRead(key)
		snapshot, err := coordinator.Store.Load(loadCtx, tenantID, conversationID)
		if err != nil {
			return domain.SessionSnapshot{}, fmt.Errorf("load session %q: %w", conversationID, err)
		}
		if snapshot.ConversationID != conversationID {
			return domain.SessionSnapshot{}, fmt.Errorf("%w: durable session identity mismatch", domain.ErrConflict)
		}
		if floor := coordinator.floor(key); snapshot.Revision < floor {
			return domain.SessionSnapshot{}, fmt.Errorf("%w: durable revision %d behind locally written %d", domain.ErrStaleEvidence, snapshot.Revision, floor)
		}
		if err := coordinator.Cache.Put(loadCtx, tenantID, snapshot); err != nil {
			coordinator.recordCacheError(loadCtx, tenantID, "put", err)
		}
		// A write that landed during this read has already invalidated; the put
		// above may have raced it, so invalidate again instead of keeping it.
		if coordinator.superseded(read) {
			_ = coordinator.invalidate(loadCtx, tenantID, conversationID)
		}
		return snapshot, nil
	})
}

// Checkpoint writes durable state before invalidating the cached snapshot.
func (coordinator *SessionCoordinator) Checkpoint(ctx context.Context, tenantID, expectedRevision int64, checkpoint domain.StepCheckpoint) error {
	if tenantID <= 0 || expectedRevision < 0 || strings.TrimSpace(checkpoint.ConversationID) == "" {
		return fmt.Errorf("%w: tenant, non-negative revision, and conversation are required", domain.ErrValidation)
	}
	if err := coordinator.validate(); err != nil {
		return err
	}
	return coordinator.write(ctx, tenantID, checkpoint.ConversationID, expectedRevision+1, "checkpoint", func() error {
		return coordinator.Store.Checkpoint(ctx, tenantID, expectedRevision, checkpoint)
	})
}

// Complete writes the terminal result before invalidating the cached snapshot.
func (coordinator *SessionCoordinator) Complete(ctx context.Context, tenantID int64, conversationID string, expectedRevision int64, result domain.TurnResult) error {
	if tenantID <= 0 || expectedRevision < 0 || strings.TrimSpace(conversationID) == "" {
		return fmt.Errorf("%w: tenant, non-negative revision, and conversation are required", domain.ErrValidation)
	}
	if err := coordinator.validate(); err != nil {
		return err
	}
	return coordinator.write(ctx, tenantID, conversationID, expectedRevision, "complete", func() error {
		return coordinator.Store.Complete(ctx, tenantID, conversationID, expectedRevision, result)
	})
}

// write runs the durable write, then supersedes overlapping reads, raises the
// revision floor, and invalidates; a failed write still invalidates because the
// cached snapshot is stale by definition or of unknown standing.
func (coordinator *SessionCoordinator) write(ctx context.Context, tenantID int64, conversationID string, resultingRevision int64, operation string, durable func() error) error {
	key := domain.SessionKey{TenantID: tenantID, ConversationID: conversationID}
	if err := durable(); err != nil {
		if cacheErr := coordinator.invalidateAfterWrite(ctx, tenantID, conversationID); cacheErr != nil {
			return errors.Join(fmt.Errorf("%s session %q: %w", operation, conversationID, err), cacheErr)
		}
		return fmt.Errorf("%s session %q: %w", operation, conversationID, err)
	}
	coordinator.mu.Lock()
	if read := coordinator.reads[key]; read != nil {
		read.superseded = true
	}
	if floor, _ := coordinator.floorCache().Get(key); resultingRevision > floor {
		coordinator.floorCache().Set(key, resultingRevision, 0)
	}
	coordinator.mu.Unlock()
	if err := coordinator.invalidateAfterWrite(ctx, tenantID, conversationID); err != nil {
		return fmt.Errorf("%s session %q committed but cache invalidation failed: %w", operation, conversationID, err)
	}
	return nil
}

func (coordinator *SessionCoordinator) validate() error {
	if coordinator.Store == nil || coordinator.Cache == nil || coordinator.Metrics == nil {
		return fmt.Errorf("session coordinator: store, cache, and metrics are required")
	}
	if coordinator.RevisionMemory < 0 {
		return fmt.Errorf("session coordinator: revision memory must not be negative")
	}
	return nil
}

func (coordinator *SessionCoordinator) floorCache() *lru.Cache[domain.SessionKey, int64] {
	if coordinator.floors == nil {
		capacity := coordinator.RevisionMemory
		if capacity == 0 {
			capacity = defaultRevisionMemory
		}
		coordinator.floors = lru.New[domain.SessionKey, int64](capacity, nil)
	}
	return coordinator.floors
}

func (coordinator *SessionCoordinator) floor(key domain.SessionKey) int64 {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	floor, _ := coordinator.floorCache().Get(key)
	return floor
}

func (coordinator *SessionCoordinator) beginRead(key domain.SessionKey) *inflightRead {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.reads == nil {
		coordinator.reads = make(map[domain.SessionKey]*inflightRead)
	}
	read := &inflightRead{}
	coordinator.reads[key] = read
	return read
}

func (coordinator *SessionCoordinator) endRead(key domain.SessionKey) {
	coordinator.mu.Lock()
	delete(coordinator.reads, key)
	coordinator.mu.Unlock()
}

func (coordinator *SessionCoordinator) superseded(read *inflightRead) bool {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return read.superseded
}

func (coordinator *SessionCoordinator) invalidate(ctx context.Context, tenantID int64, conversationID string) error {
	if err := coordinator.Cache.Invalidate(ctx, tenantID, conversationID); err != nil {
		coordinator.recordCacheError(ctx, tenantID, "invalidate", err)
		return err
	}
	return nil
}

func (coordinator *SessionCoordinator) invalidateAfterWrite(ctx context.Context, tenantID int64, conversationID string) error {
	timeout := coordinator.CacheTimeout
	if timeout <= 0 {
		timeout = time.Second
	}
	cacheCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return coordinator.invalidate(cacheCtx, tenantID, conversationID)
}

func (coordinator *SessionCoordinator) recordCacheError(ctx context.Context, tenantID int64, operation string, err error) {
	coordinator.Metrics.RecordDependency(ctx, tenantID, "session_cache", operation, domain.Usage{}, err)
}

var _ contract.SessionCoordinator = (*SessionCoordinator)(nil)
