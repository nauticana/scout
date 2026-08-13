package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StepExecutorFunc adapts a function to contract.StepExecutor.
type StepExecutorFunc func(context.Context, domain.StepInput) (domain.StepResult, error)

// Execute invokes the configured function.
func (function StepExecutorFunc) Execute(ctx context.Context, input domain.StepInput) (domain.StepResult, error) {
	return function(ctx, input)
}

// ExecutionGraphRepository contains configurable graph repository operations.
type ExecutionGraphRepository struct {
	PutFunc func(context.Context, int64, domain.ExecutionGraph) error
	GetFunc func(context.Context, int64, string, string) (domain.ExecutionGraph, error)
}

// Put invokes PutFunc.
func (repository *ExecutionGraphRepository) Put(ctx context.Context, tenantID int64, graph domain.ExecutionGraph) error {
	return repository.PutFunc(ctx, tenantID, graph)
}

// Get invokes GetFunc.
func (repository *ExecutionGraphRepository) Get(ctx context.Context, tenantID int64, agentID, version string) (domain.ExecutionGraph, error) {
	return repository.GetFunc(ctx, tenantID, agentID, version)
}

// ExecutionGraphCache contains configurable graph cache operations.
type ExecutionGraphCache struct {
	GetFunc        func(context.Context, int64, string, string) (domain.ExecutionGraph, bool, error)
	PutFunc        func(context.Context, int64, domain.ExecutionGraph) error
	InvalidateFunc func(context.Context, int64, string, string) error
}

// Get invokes GetFunc.
func (cache *ExecutionGraphCache) Get(ctx context.Context, tenantID int64, agentID, version string) (domain.ExecutionGraph, bool, error) {
	return cache.GetFunc(ctx, tenantID, agentID, version)
}

// Put invokes PutFunc.
func (cache *ExecutionGraphCache) Put(ctx context.Context, tenantID int64, graph domain.ExecutionGraph) error {
	return cache.PutFunc(ctx, tenantID, graph)
}

// Invalidate invokes InvalidateFunc.
func (cache *ExecutionGraphCache) Invalidate(ctx context.Context, tenantID int64, agentID, version string) error {
	return cache.InvalidateFunc(ctx, tenantID, agentID, version)
}

// DurableSessionStore contains configurable durable session operations.
type DurableSessionStore struct {
	LoadFunc       func(context.Context, int64, string) (domain.SessionSnapshot, error)
	CheckpointFunc func(context.Context, int64, int64, domain.StepCheckpoint) error
	CompleteFunc   func(context.Context, int64, string, int64, domain.TurnResult) error
}

// Load invokes LoadFunc.
func (store *DurableSessionStore) Load(ctx context.Context, tenantID int64, conversationID string) (domain.SessionSnapshot, error) {
	return store.LoadFunc(ctx, tenantID, conversationID)
}

// Checkpoint invokes CheckpointFunc.
func (store *DurableSessionStore) Checkpoint(ctx context.Context, tenantID, expectedRevision int64, checkpoint domain.StepCheckpoint) error {
	return store.CheckpointFunc(ctx, tenantID, expectedRevision, checkpoint)
}

// Complete invokes CompleteFunc.
func (store *DurableSessionStore) Complete(ctx context.Context, tenantID int64, conversationID string, expectedRevision int64, result domain.TurnResult) error {
	return store.CompleteFunc(ctx, tenantID, conversationID, expectedRevision, result)
}

// HotSessionCache contains configurable session cache operations.
type HotSessionCache struct {
	GetFunc        func(context.Context, int64, string) (domain.SessionSnapshot, bool, error)
	PutFunc        func(context.Context, int64, domain.SessionSnapshot) error
	InvalidateFunc func(context.Context, int64, string) error
}

// Get invokes GetFunc.
func (cache *HotSessionCache) Get(ctx context.Context, tenantID int64, conversationID string) (domain.SessionSnapshot, bool, error) {
	return cache.GetFunc(ctx, tenantID, conversationID)
}

// Put invokes PutFunc.
func (cache *HotSessionCache) Put(ctx context.Context, tenantID int64, snapshot domain.SessionSnapshot) error {
	return cache.PutFunc(ctx, tenantID, snapshot)
}

// Invalidate invokes InvalidateFunc.
func (cache *HotSessionCache) Invalidate(ctx context.Context, tenantID int64, conversationID string) error {
	return cache.InvalidateFunc(ctx, tenantID, conversationID)
}

// RuntimeMetrics contains configurable runtime metric callbacks.
type RuntimeMetrics struct {
	RecordTurnFunc       func(context.Context, domain.TurnRequest, domain.TurnResult, error)
	RecordStepFunc       func(context.Context, int64, string, domain.ExecutionStep, domain.StepResult, error)
	RecordDependencyFunc func(context.Context, int64, string, string, domain.Usage, error)
}

// PublishedAgentResolver contains a configurable immutable-definition lookup.
type PublishedAgentResolver struct {
	ResolveFunc func(context.Context, int64, string, string, string) (domain.AgentDefinition, error)
}

// Resolve invokes ResolveFunc.
func (resolver *PublishedAgentResolver) Resolve(ctx context.Context, tenantID int64, aliasID, languageCode, conversationID string) (domain.AgentDefinition, error) {
	return resolver.ResolveFunc(ctx, tenantID, aliasID, languageCode, conversationID)
}

// RecordTurn invokes RecordTurnFunc when configured.
func (metrics *RuntimeMetrics) RecordTurn(ctx context.Context, request domain.TurnRequest, result domain.TurnResult, err error) {
	if metrics.RecordTurnFunc != nil {
		metrics.RecordTurnFunc(ctx, request, result, err)
	}
}

// RecordStep invokes RecordStepFunc when configured.
func (metrics *RuntimeMetrics) RecordStep(ctx context.Context, tenantID int64, agentID string, step domain.ExecutionStep, result domain.StepResult, err error) {
	if metrics.RecordStepFunc != nil {
		metrics.RecordStepFunc(ctx, tenantID, agentID, step, result, err)
	}
}

// RecordDependency invokes RecordDependencyFunc when configured.
func (metrics *RuntimeMetrics) RecordDependency(ctx context.Context, tenantID int64, dependency, operation string, usage domain.Usage, err error) {
	if metrics.RecordDependencyFunc != nil {
		metrics.RecordDependencyFunc(ctx, tenantID, dependency, operation, usage, err)
	}
}

var _ contract.StepExecutor = StepExecutorFunc(nil)
var _ contract.ExecutionGraphRepository = (*ExecutionGraphRepository)(nil)
var _ contract.ExecutionGraphCache = (*ExecutionGraphCache)(nil)
var _ contract.DurableSessionStore = (*DurableSessionStore)(nil)
var _ contract.HotSessionCache = (*HotSessionCache)(nil)
var _ contract.RuntimeMetrics = (*RuntimeMetrics)(nil)
var _ contract.PublishedAgentResolver = (*PublishedAgentResolver)(nil)
