package fake

import (
	"context"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// TurnDispatcher contains a configurable durable dispatch callback.
type TurnDispatcher struct {
	EnqueueFunc func(context.Context, domain.TurnDispatch) error
}

// Enqueue invokes EnqueueFunc when configured.
func (dispatcher *TurnDispatcher) Enqueue(ctx context.Context, dispatch domain.TurnDispatch) error {
	if dispatcher.EnqueueFunc == nil {
		return nil
	}
	return dispatcher.EnqueueFunc(ctx, dispatch)
}

// FairTurnScheduler contains configurable lease lifecycle callbacks.
type FairTurnScheduler struct {
	ClaimFunc  func(context.Context, string, time.Duration) (domain.QueueLease, error)
	ExtendFunc func(context.Context, string, string, time.Duration) error
	AckFunc    func(context.Context, string, string) error
	NackFunc   func(context.Context, string, string, string) error
}

// Claim invokes ClaimFunc.
func (scheduler *FairTurnScheduler) Claim(ctx context.Context, workerID string, leaseDuration time.Duration) (domain.QueueLease, error) {
	return scheduler.ClaimFunc(ctx, workerID, leaseDuration)
}

// Extend invokes ExtendFunc when configured.
func (scheduler *FairTurnScheduler) Extend(ctx context.Context, messageID, workerID string, leaseDuration time.Duration) error {
	if scheduler.ExtendFunc == nil {
		return nil
	}
	return scheduler.ExtendFunc(ctx, messageID, workerID, leaseDuration)
}

// Ack invokes AckFunc when configured.
func (scheduler *FairTurnScheduler) Ack(ctx context.Context, messageID, workerID string) error {
	if scheduler.AckFunc == nil {
		return nil
	}
	return scheduler.AckFunc(ctx, messageID, workerID)
}

// Nack invokes NackFunc when configured.
func (scheduler *FairTurnScheduler) Nack(ctx context.Context, messageID, workerID, reason string) error {
	if scheduler.NackFunc == nil {
		return nil
	}
	return scheduler.NackFunc(ctx, messageID, workerID, reason)
}

// DeadLetterQueue contains a configurable parking callback.
type DeadLetterQueue struct {
	PublishFunc func(context.Context, domain.QueueMessage, string) error
}

// Publish invokes PublishFunc when configured.
func (queue *DeadLetterQueue) Publish(ctx context.Context, message domain.QueueMessage, reason string) error {
	if queue.PublishFunc == nil {
		return nil
	}
	return queue.PublishFunc(ctx, message, reason)
}

// TenantWeightPolicy contains a configurable fair-scheduling weight callback.
type TenantWeightPolicy struct {
	SchedulingWeightFunc func(context.Context, int64) (int, int, error)
}

// SchedulingWeight invokes SchedulingWeightFunc when configured; the default is weight 1, unlimited.
func (policy *TenantWeightPolicy) SchedulingWeight(ctx context.Context, tenantID int64) (int, int, error) {
	if policy.SchedulingWeightFunc == nil {
		return 1, 0, nil
	}
	return policy.SchedulingWeightFunc(ctx, tenantID)
}

// TurnBudgetEstimator contains a configurable turn quote callback.
type TurnBudgetEstimator struct {
	EstimateFunc func(context.Context, domain.TurnRequest) (domain.Usage, error)
}

// Estimate invokes EstimateFunc when configured; the default quotes one token in USD.
func (estimator *TurnBudgetEstimator) Estimate(ctx context.Context, request domain.TurnRequest) (domain.Usage, error) {
	if estimator.EstimateFunc == nil {
		return domain.Usage{InputTokens: 1, CostMinorUnits: 1, Currency: "USD"}, nil
	}
	return estimator.EstimateFunc(ctx, request)
}

// TurnRecordStore contains configurable durable turn identity callbacks.
type TurnRecordStore struct {
	OpenFunc        func(context.Context, domain.TurnRequest, domain.ObjectRef) (int64, error)
	FindFunc        func(context.Context, int64, string) (int64, string, []byte, error)
	StartFunc       func(context.Context, int64, string) error
	FailFunc        func(context.Context, int64, string, string, string) error
	SuspendFunc     func(context.Context, int64, string, string) error
	ResumeFunc      func(context.Context, int64, string) error
	RecordUsageFunc func(context.Context, int64, string, int64, string, domain.UsageAttribution, domain.Usage) error
}

// Open invokes OpenFunc when configured; the default assigns turn 1.
func (store *TurnRecordStore) Open(ctx context.Context, request domain.TurnRequest, input domain.ObjectRef) (int64, error) {
	if store.OpenFunc == nil {
		return 1, nil
	}
	return store.OpenFunc(ctx, request, input)
}

// Find invokes FindFunc when configured; the default reports a queued turn 1.
func (store *TurnRecordStore) Find(ctx context.Context, tenantID int64, requestID string) (int64, string, []byte, error) {
	if store.FindFunc == nil {
		return 1, "queued", nil, nil
	}
	return store.FindFunc(ctx, tenantID, requestID)
}

// Start invokes StartFunc when configured.
func (store *TurnRecordStore) Start(ctx context.Context, tenantID int64, requestID string) error {
	if store.StartFunc == nil {
		return nil
	}
	return store.StartFunc(ctx, tenantID, requestID)
}

// Fail invokes FailFunc when configured.
func (store *TurnRecordStore) Fail(ctx context.Context, tenantID int64, requestID, status, errorCode string) error {
	if store.FailFunc == nil {
		return nil
	}
	return store.FailFunc(ctx, tenantID, requestID, status, errorCode)
}

// RecordUsage invokes RecordUsageFunc when configured.
// Suspend parks the turn awaiting a decision.
func (store *TurnRecordStore) Suspend(ctx context.Context, tenantID int64, requestID, reason string) error {
	if store.SuspendFunc != nil {
		return store.SuspendFunc(ctx, tenantID, requestID, reason)
	}
	return nil
}

// Resume returns a suspended turn to the queue.
func (store *TurnRecordStore) Resume(ctx context.Context, tenantID int64, requestID string) error {
	if store.ResumeFunc != nil {
		return store.ResumeFunc(ctx, tenantID, requestID)
	}
	return nil
}

func (store *TurnRecordStore) RecordUsage(ctx context.Context, tenantID int64, conversationID string, turnNo int64, subjectRef string, attribution domain.UsageAttribution, usage domain.Usage) error {
	if store.RecordUsageFunc == nil {
		return nil
	}
	return store.RecordUsageFunc(ctx, tenantID, conversationID, turnNo, subjectRef, attribution, usage)
}

// StepIdempotencyStore contains configurable replay-safety callbacks.
type StepIdempotencyStore struct {
	BeginFunc   func(context.Context, int64, string, domain.ExecutionStep) (domain.StepResult, bool, error)
	CommitFunc  func(context.Context, int64, string, domain.ExecutionStep, domain.StepResult) error
	AbandonFunc func(context.Context, int64, string, domain.ExecutionStep) error
}

// Begin invokes BeginFunc when configured; the default claims the step.
func (store *StepIdempotencyStore) Begin(ctx context.Context, tenantID int64, requestID string, step domain.ExecutionStep) (domain.StepResult, bool, error) {
	if store.BeginFunc == nil {
		return domain.StepResult{}, false, nil
	}
	return store.BeginFunc(ctx, tenantID, requestID, step)
}

// Commit invokes CommitFunc when configured.
func (store *StepIdempotencyStore) Commit(ctx context.Context, tenantID int64, requestID string, step domain.ExecutionStep, result domain.StepResult) error {
	if store.CommitFunc == nil {
		return nil
	}
	return store.CommitFunc(ctx, tenantID, requestID, step, result)
}

// Abandon invokes AbandonFunc when configured.
func (store *StepIdempotencyStore) Abandon(ctx context.Context, tenantID int64, requestID string, step domain.ExecutionStep) error {
	if store.AbandonFunc == nil {
		return nil
	}
	return store.AbandonFunc(ctx, tenantID, requestID, step)
}

// SessionCoordinator contains configurable tiered session callbacks.
type SessionCoordinator struct {
	LoadFunc       func(context.Context, int64, string) (domain.SessionSnapshot, error)
	CheckpointFunc func(context.Context, int64, int64, domain.StepCheckpoint) error
	CompleteFunc   func(context.Context, int64, string, int64, domain.TurnResult) error
}

// Load invokes LoadFunc.
func (coordinator *SessionCoordinator) Load(ctx context.Context, tenantID int64, conversationID string) (domain.SessionSnapshot, error) {
	return coordinator.LoadFunc(ctx, tenantID, conversationID)
}

// Checkpoint invokes CheckpointFunc when configured.
func (coordinator *SessionCoordinator) Checkpoint(ctx context.Context, tenantID, expectedRevision int64, checkpoint domain.StepCheckpoint) error {
	if coordinator.CheckpointFunc == nil {
		return nil
	}
	return coordinator.CheckpointFunc(ctx, tenantID, expectedRevision, checkpoint)
}

// Complete invokes CompleteFunc when configured.
func (coordinator *SessionCoordinator) Complete(ctx context.Context, tenantID int64, conversationID string, expectedRevision int64, result domain.TurnResult) error {
	if coordinator.CompleteFunc == nil {
		return nil
	}
	return coordinator.CompleteFunc(ctx, tenantID, conversationID, expectedRevision, result)
}

// DefinitionResolverFunc adapts a function to contract.DefinitionResolver.
type DefinitionResolverFunc func(context.Context, int64, string, string) (domain.ExecutionGraph, error)

// Resolve invokes the configured function.
func (function DefinitionResolverFunc) Resolve(ctx context.Context, tenantID int64, agentID, version string) (domain.ExecutionGraph, error) {
	return function(ctx, tenantID, agentID, version)
}

// StepExecutorRegistryFunc adapts a function to contract.StepExecutorRegistry.
type StepExecutorRegistryFunc func(context.Context, string) (contract.StepExecutor, error)

// ExecutorFor invokes the configured function.
func (function StepExecutorRegistryFunc) ExecutorFor(ctx context.Context, stepKind string) (contract.StepExecutor, error) {
	return function(ctx, stepKind)
}

// TenantPolicyRepositoryFunc adapts a function to contract.TenantPolicyRepository.
type TenantPolicyRepositoryFunc func(context.Context, int64) (domain.TenantRuntimePolicy, error)

// GetRuntimePolicy invokes the configured function.
func (function TenantPolicyRepositoryFunc) GetRuntimePolicy(ctx context.Context, tenantID int64) (domain.TenantRuntimePolicy, error) {
	return function(ctx, tenantID)
}

// GuardrailConfigRepository contains configurable guardrail policy callbacks.
type GuardrailConfigRepository struct {
	PublishFunc func(context.Context, int64, string, domain.GuardrailConfig) error
	GetFunc     func(context.Context, int64, string, string) (domain.GuardrailConfig, error)
}

// Publish invokes PublishFunc when configured.
func (repository *GuardrailConfigRepository) Publish(ctx context.Context, tenantID int64, agentID string, config domain.GuardrailConfig) error {
	if repository.PublishFunc == nil {
		return nil
	}
	return repository.PublishFunc(ctx, tenantID, agentID, config)
}

// Get invokes GetFunc when configured.
func (repository *GuardrailConfigRepository) Get(ctx context.Context, tenantID int64, agentID, agentVersion string) (domain.GuardrailConfig, error) {
	if repository.GetFunc == nil {
		return domain.GuardrailConfig{}, nil
	}
	return repository.GetFunc(ctx, tenantID, agentID, agentVersion)
}

// ExecutionGovernor contains a configurable permit factory.
type ExecutionGovernor struct {
	StartFunc func(context.Context, domain.TurnRequest, domain.TenantRuntimePolicy) (contract.ExecutionPermit, error)
}

// Start invokes StartFunc when configured; the default returns a permissive permit.
func (governor *ExecutionGovernor) Start(ctx context.Context, request domain.TurnRequest, policy domain.TenantRuntimePolicy) (contract.ExecutionPermit, error) {
	if governor.StartFunc == nil {
		return &ExecutionPermit{}, nil
	}
	return governor.StartFunc(ctx, request, policy)
}

// ExecutionPermit contains configurable per-step limit callbacks.
type ExecutionPermit struct {
	BeforeStepFunc func(context.Context, domain.ExecutionStep) error
	AfterStepFunc  func(context.Context, domain.StepResult) error
	CloseFunc      func(context.Context, domain.Usage) error
}

// BeforeStep invokes BeforeStepFunc when configured.
func (permit *ExecutionPermit) BeforeStep(ctx context.Context, step domain.ExecutionStep) error {
	if permit.BeforeStepFunc == nil {
		return nil
	}
	return permit.BeforeStepFunc(ctx, step)
}

// AfterStep invokes AfterStepFunc when configured.
func (permit *ExecutionPermit) AfterStep(ctx context.Context, result domain.StepResult) error {
	if permit.AfterStepFunc == nil {
		return nil
	}
	return permit.AfterStepFunc(ctx, result)
}

// Close invokes CloseFunc when configured.
func (permit *ExecutionPermit) Close(ctx context.Context, usage domain.Usage) error {
	if permit.CloseFunc == nil {
		return nil
	}
	return permit.CloseFunc(ctx, usage)
}

// TurnReplyPublisherFunc adapts a function to contract.TurnReplyPublisher.
type TurnReplyPublisherFunc func(context.Context, domain.TurnReply) error

// Publish invokes the configured function.
func (function TurnReplyPublisherFunc) Publish(ctx context.Context, reply domain.TurnReply) error {
	return function(ctx, reply)
}

// TurnReplySubscriberFunc adapts a function to contract.TurnReplySubscriber.
type TurnReplySubscriberFunc func(context.Context, int64, string) (contract.TurnReplySubscription, error)

// Subscribe invokes the configured function.
func (function TurnReplySubscriberFunc) Subscribe(ctx context.Context, tenantID int64, requestID string) (contract.TurnReplySubscription, error) {
	return function(ctx, tenantID, requestID)
}

// TurnReplySubscription is a closable subscription over a fixed reply route.
type TurnReplySubscription struct {
	RouteValue  string
	ReceiveFunc func(context.Context) (domain.TurnReply, error)
	CloseFunc   func() error
	Closed      bool
}

// Route returns the configured route.
func (subscription *TurnReplySubscription) Route() string { return subscription.RouteValue }

// Receive invokes ReceiveFunc when configured.
func (subscription *TurnReplySubscription) Receive(ctx context.Context) (domain.TurnReply, error) {
	if subscription.ReceiveFunc == nil {
		return domain.TurnReply{}, context.Canceled
	}
	return subscription.ReceiveFunc(ctx)
}

// Close marks the subscription closed and invokes CloseFunc when configured.
func (subscription *TurnReplySubscription) Close() error {
	subscription.Closed = true
	if subscription.CloseFunc == nil {
		return nil
	}
	return subscription.CloseFunc()
}

var (
	_ contract.TurnDispatcher            = (*TurnDispatcher)(nil)
	_ contract.FairTurnScheduler         = (*FairTurnScheduler)(nil)
	_ contract.DeadLetterQueue           = (*DeadLetterQueue)(nil)
	_ contract.TenantWeightPolicy        = (*TenantWeightPolicy)(nil)
	_ contract.TurnBudgetEstimator       = (*TurnBudgetEstimator)(nil)
	_ contract.TurnRecordStore           = (*TurnRecordStore)(nil)
	_ contract.StepIdempotencyStore      = (*StepIdempotencyStore)(nil)
	_ contract.SessionCoordinator        = (*SessionCoordinator)(nil)
	_ contract.DefinitionResolver        = DefinitionResolverFunc(nil)
	_ contract.StepExecutorRegistry      = StepExecutorRegistryFunc(nil)
	_ contract.TenantPolicyRepository    = TenantPolicyRepositoryFunc(nil)
	_ contract.GuardrailConfigRepository = (*GuardrailConfigRepository)(nil)
	_ contract.TenantBudgetManager       = (*TenantBudgetManager)(nil)
	_ contract.ExecutionGovernor         = (*ExecutionGovernor)(nil)
	_ contract.ExecutionPermit           = (*ExecutionPermit)(nil)
	_ contract.TurnReplyPublisher        = TurnReplyPublisherFunc(nil)
	_ contract.TurnReplySubscriber       = TurnReplySubscriberFunc(nil)
	_ contract.TurnReplySubscription     = (*TurnReplySubscription)(nil)
)
