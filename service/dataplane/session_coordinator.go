package dataplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// SessionCoordinator keeps durable session state authoritative over its cache.
type SessionCoordinator struct {
	Store   contract.DurableSessionStore
	Cache   contract.HotSessionCache
	Metrics contract.RuntimeMetrics
}

// Load returns a valid cached snapshot or refreshes it from durable storage.
func (coordinator *SessionCoordinator) Load(ctx context.Context, tenantID int64, conversationID string) (domain.SessionSnapshot, error) {
	if tenantID <= 0 || strings.TrimSpace(conversationID) == "" {
		return domain.SessionSnapshot{}, fmt.Errorf("%w: tenant and conversation are required", domain.ErrValidation)
	}
	if err := coordinator.validate(); err != nil {
		return domain.SessionSnapshot{}, err
	}
	snapshot, found, err := coordinator.Cache.Get(ctx, tenantID, conversationID)
	if err != nil {
		coordinator.recordCacheError(ctx, tenantID, "get", err)
	} else if found && snapshot.ConversationID == conversationID {
		return snapshot, nil
	} else if found {
		cacheErr := fmt.Errorf("%w: cached session identity mismatch", domain.ErrConflict)
		coordinator.recordCacheError(ctx, tenantID, "get", cacheErr)
		coordinator.invalidate(ctx, tenantID, conversationID)
	}
	snapshot, err = coordinator.Store.Load(ctx, tenantID, conversationID)
	if err != nil {
		return domain.SessionSnapshot{}, fmt.Errorf("load session %q: %w", conversationID, err)
	}
	if snapshot.ConversationID != conversationID {
		return domain.SessionSnapshot{}, fmt.Errorf("%w: durable session identity mismatch", domain.ErrConflict)
	}
	if err := coordinator.Cache.Put(ctx, tenantID, snapshot); err != nil {
		coordinator.recordCacheError(ctx, tenantID, "put", err)
	}
	return snapshot, nil
}

// Checkpoint writes durable state before invalidating the cached snapshot.
func (coordinator *SessionCoordinator) Checkpoint(ctx context.Context, tenantID, expectedRevision int64, checkpoint domain.StepCheckpoint) error {
	if tenantID <= 0 || expectedRevision <= 0 || strings.TrimSpace(checkpoint.ConversationID) == "" {
		return fmt.Errorf("%w: tenant, revision, and conversation are required", domain.ErrValidation)
	}
	if err := coordinator.validate(); err != nil {
		return err
	}
	if err := coordinator.Store.Checkpoint(ctx, tenantID, expectedRevision, checkpoint); err != nil {
		return fmt.Errorf("checkpoint session %q: %w", checkpoint.ConversationID, err)
	}
	coordinator.invalidate(ctx, tenantID, checkpoint.ConversationID)
	return nil
}

// Complete writes the terminal result before invalidating the cached snapshot.
func (coordinator *SessionCoordinator) Complete(ctx context.Context, tenantID int64, conversationID string, expectedRevision int64, result domain.TurnResult) error {
	if tenantID <= 0 || expectedRevision <= 0 || strings.TrimSpace(conversationID) == "" {
		return fmt.Errorf("%w: tenant, revision, and conversation are required", domain.ErrValidation)
	}
	if err := coordinator.validate(); err != nil {
		return err
	}
	if err := coordinator.Store.Complete(ctx, tenantID, conversationID, expectedRevision, result); err != nil {
		return fmt.Errorf("complete session %q: %w", conversationID, err)
	}
	coordinator.invalidate(ctx, tenantID, conversationID)
	return nil
}

func (coordinator *SessionCoordinator) validate() error {
	if coordinator.Store == nil || coordinator.Cache == nil || coordinator.Metrics == nil {
		return fmt.Errorf("session coordinator: store, cache, and metrics are required")
	}
	return nil
}

func (coordinator *SessionCoordinator) invalidate(ctx context.Context, tenantID int64, conversationID string) {
	if err := coordinator.Cache.Invalidate(ctx, tenantID, conversationID); err != nil {
		coordinator.recordCacheError(ctx, tenantID, "invalidate", err)
	}
}

func (coordinator *SessionCoordinator) recordCacheError(ctx context.Context, tenantID int64, operation string, err error) {
	coordinator.Metrics.RecordDependency(ctx, tenantID, "session_cache", operation, domain.Usage{}, err)
}

var _ contract.SessionCoordinator = (*SessionCoordinator)(nil)
