package dataplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nauticana/keel/cache"
	"github.com/nauticana/keel/port"
	"github.com/nauticana/keel/secret"
	"github.com/nauticana/keel/storage"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
	"github.com/nauticana/scout/service/guardrail"
	"github.com/nauticana/scout/service/isolation"
	"github.com/nauticana/scout/service/modelgateway"
	"github.com/nauticana/scout/service/observability"
	agentruntime "github.com/nauticana/scout/service/runtime"
	"github.com/nauticana/scout/service/skill"
	"github.com/nauticana/scout/service/toolgateway"
)

const costBreakerWindow = time.Hour

// BaseDataPlane is the table-backed data plane, complete once Compose succeeds. The first
// block is what a product injects. Every collaborator below it is a default Compose builds
// only while the field is nil, so a product swaps one by assigning its own implementation
// before Compose, and extends the whole by embedding BaseDataPlane.
type BaseDataPlane struct {
	DB    port.DatabaseRepository
	Cache cache.CacheService
	// Storage is bound to Settings.StateBucket; see NewStateStorage.
	Storage     storage.ObjectStorage
	Providers   contract.AgentProviderFactory
	Transport   contract.ToolTransport
	Budget      contract.TenantBudgetManager
	Pricer      contract.ModelPricer
	RateLimiter contract.TenantRateLimiter
	Capacity    contract.CapacityScheduler
	Metrics     contract.RuntimeMetrics

	Settings domain.DataPlaneSettings
	Limits   domain.ToolLoopLimits
	// Currency denominates budget quotes and the cost breaker.
	Currency string
	// UsageCategory labels each settled turn's usage event; TaskKind its history row.
	UsageCategory   string
	TaskKind        string
	MaxOutputTokens int64
	// LanguageCode and Task are ReleaseLoopRequestBuilder's; both are optional.
	LanguageCode string
	Task         func(ctx context.Context, input domain.StepInput) (domain.AgentTask, string, error)
	OnSettled    func(ctx context.Context, turn domain.TurnRequest, turnNo int64, agentVersion string, usage domain.Usage) error
	// Effects is required by a tool that declares VerifyEffect; Approvals by an
	// irreversible_tool_approval rule; Weights is optional tenant fairness.
	Effects   contract.ToolEffectVerifier
	Approvals contract.ToolApprovalGate
	// Knowledge and Entitlements put the documents a release binds whole into its
	// turns; nil means no release binds knowledge whole.
	Knowledge    contract.PinnedKnowledgeResolver
	Entitlements contract.EntitlementResolver
	Weights      contract.TenantWeightPolicy

	Objects          ObjectStateCodec
	Records          contract.TurnRecordStore
	Sessions         contract.SessionCoordinator
	Definitions      contract.DefinitionResolver
	Policies         contract.TenantPolicyRepository
	GuardrailConfigs contract.GuardrailConfigRepository
	Governor         contract.ExecutionGovernor
	Idempotency      contract.StepIdempotencyStore
	Audit            contract.AuditSink
	Guardrails       contract.GuardrailEnforcer
	Estimator        contract.TurnBudgetEstimator
	ReplyHub         *CacheReplyHub
	Cancels          *TableTurnCanceller
	DeadLetters      contract.DeadLetterQueue
	TurnScheduler    *QueueTurnScheduler
	Dispatcher       contract.TurnDispatcher
	Tools            contract.ToolRegistry
	Skills           contract.SkillRegistry
	Credentials      contract.ToolCredentialProvider
	Egress           contract.ToolEgressPolicy
	ToolGateway      contract.GovernedToolGateway
	Catalog          contract.ModelCandidateCatalog
	Router           contract.ModelRouter
	Models           contract.ModelGateway
	Requests         contract.ToolLoopRequestBuilder
	Journal          contract.LoopJournal
	Evidence         contract.EvidenceValidator
	Executors        contract.StepExecutorRegistry
	TurnRuntime      contract.ConversationRuntime
	TurnIngress      contract.ConversationIngress

	closers []io.Closer
}

var _ contract.DataPlane = (*BaseDataPlane)(nil)

func (plane *BaseDataPlane) Runtime() contract.ConversationRuntime       { return plane.TurnRuntime }
func (plane *BaseDataPlane) Scheduler() contract.FairTurnScheduler       { return plane.TurnScheduler }
func (plane *BaseDataPlane) Ingress() contract.ConversationIngress       { return plane.TurnIngress }
func (plane *BaseDataPlane) Replies() contract.ReplayTurnReplySubscriber { return plane.ReplyHub }
func (plane *BaseDataPlane) Canceller() contract.TurnCanceller           { return plane.Cancels }

// LoopDeadline is the longest one step may run; a queue lease must outlast it.
func (plane *BaseDataPlane) LoopDeadline() time.Duration { return plane.Limits.Deadline }

// StoredReply reconstructs the terminal frame after the delivery cache expires.
func (plane *BaseDataPlane) StoredReply(ctx context.Context, tenantID int64, requestID string, sequence int64) (domain.TurnReply, error) {
	if plane.ReplyHub == nil {
		return domain.TurnReply{}, fmt.Errorf("%w: data plane is not composed", domain.ErrNotReady)
	}
	return plane.ReplyHub.StoredReply(ctx, tenantID, requestID, sequence)
}

// Worker is the keel leased queue worker draining the composed scheduler into the composed
// runtime; the caller sets its AbstractWorker fields and runs it. A non-positive lease or
// batch is taken from the settings, so the worker's SQL and the scheduler stay in step.
func (plane *BaseDataPlane) Worker(workerID string, lease time.Duration, batch int) (*RuntimeWorker, error) {
	if plane.TurnScheduler == nil || plane.TurnRuntime == nil {
		return nil, fmt.Errorf("%w: data plane is not composed", domain.ErrNotReady)
	}
	tuning := QueueTuning{Lease: lease, Batch: batch, MaxAttempts: plane.TurnScheduler.MaxAttempts}
	if err := tuning.rejectNegative(); err != nil {
		return nil, err
	}
	tuning = tuning.withDefaults(plane.Settings)
	if tuning.Lease <= plane.LoopDeadline() {
		return nil, fmt.Errorf("%w: runtime worker lease (%s) must outlast the loop deadline (%s)", domain.ErrValidation, tuning.Lease, plane.LoopDeadline())
	}
	return NewRuntimeWorker(plane.TurnScheduler, plane.TurnRuntime, workerID, tuning.Lease, tuning.Batch, tuning.MaxAttempts)
}

// Close releases the caches and cache subscriptions Compose opened.
func (plane *BaseDataPlane) Close() error {
	var failures []error
	for _, closer := range plane.closers {
		failures = append(failures, closer.Close())
	}
	plane.closers = nil
	return errors.Join(failures...)
}

func (plane *BaseDataPlane) validate() error {
	if plane.DB == nil || plane.Cache == nil || plane.Storage == nil || plane.Providers == nil || plane.Transport == nil ||
		plane.Budget == nil || plane.Pricer == nil || plane.RateLimiter == nil || plane.Capacity == nil || plane.Metrics == nil {
		return fmt.Errorf("%w: data plane needs a database, cache, object storage, provider factory, tool transport, budget manager, pricer, rate limiter, capacity scheduler, and metrics", domain.ErrValidation)
	}
	if strings.TrimSpace(plane.Settings.StateBucket) == "" {
		return fmt.Errorf("%w: agent_state_bucket is not configured; turn input and conversation state need a private bucket", domain.ErrNotReady)
	}
	if bound := plane.Storage.Bucket(); bound != plane.Settings.StateBucket {
		return fmt.Errorf("%w: data plane storage is bound to bucket %q, not agent_state_bucket %q", domain.ErrValidation, bound, plane.Settings.StateBucket)
	}
	settings := plane.Settings
	if settings.StateMaxBytes <= 0 || settings.QueueMaxAttempts <= 0 || settings.TurnMaxSteps <= 0 ||
		settings.ToolTimeout <= 0 || settings.ToolMaxAttempts <= 0 ||
		settings.GuardrailMaxInputBytes <= 0 || settings.GuardrailMaxOutputBytes <= 0 {
		return fmt.Errorf("%w: data plane state size, queue attempts, turn steps, tool timeout, tool attempts, and guardrail byte ceilings must be positive", domain.ErrValidation)
	}
	if settings.StepClaimLease <= plane.Limits.Deadline {
		return fmt.Errorf("%w: agent_step_claim_lease (%s) must outlast agent_loop_deadline (%s), or a second worker replays a loop that is still running", domain.ErrValidation, settings.StepClaimLease, plane.Limits.Deadline)
	}
	if settings.QueueLease <= plane.Limits.Deadline || settings.QueueBatch <= 0 {
		return fmt.Errorf("%w: agent_queue_batch must be positive and agent_queue_lease (%s) must outlast agent_loop_deadline (%s), or a turn is re-delivered while it runs", domain.ErrValidation, settings.QueueLease, plane.Limits.Deadline)
	}
	if len(plane.Currency) != 3 || strings.TrimSpace(plane.UsageCategory) == "" || plane.MaxOutputTokens <= 0 {
		return fmt.Errorf("%w: data plane needs a three-letter currency, a usage category, and positive max output tokens", domain.ErrValidation)
	}
	return nil
}

// Compose validates the injected collaborators and builds every default still unset. A
// failure closes what it had opened and leaves the plane as it was.
func (plane *BaseDataPlane) Compose() error {
	if err := plane.validate(); err != nil {
		return err
	}
	draft := *plane
	for _, stage := range []func() error{
		draft.composeState, draft.composeGovernance, draft.composeDelivery, draft.composeTools, draft.composeLoop, draft.composeRuntime,
	} {
		if err := stage(); err != nil {
			draft.closers = draft.closers[len(plane.closers):]
			return errors.Join(err, draft.Close())
		}
	}
	*plane = draft
	return nil
}

func (plane *BaseDataPlane) composeState() error {
	settings := plane.Settings
	if plane.Objects == nil {
		plane.Objects = &ObjectStateStore{Storage: plane.Storage, MaxBytes: settings.StateMaxBytes}
	}
	if plane.Records == nil {
		plane.Records = &TableTurnRecordStore{DB: plane.DB, Objects: plane.Objects, UsageCategory: plane.UsageCategory, TaskKind: plane.TaskKind}
	}
	if plane.Sessions == nil {
		sessions, err := NewMemorySessionCache(MemoryCacheConfig{Capacity: settings.SessionCacheSize, TTL: settings.SessionCacheTTL})
		if err != nil {
			return err
		}
		plane.closers = append(plane.closers, sessions)
		plane.Sessions = &SessionCoordinator{
			Store: &DurableSessionStore{DB: plane.DB, Objects: plane.Objects}, Cache: sessions, Metrics: plane.Metrics,
		}
	}
	if plane.Definitions == nil {
		graphs, err := NewMemoryGraphCache(MemoryCacheConfig{Capacity: settings.GraphCacheSize, TTL: settings.GraphCacheTTL})
		if err != nil {
			return err
		}
		plane.closers = append(plane.closers, graphs)
		plane.Definitions, err = NewDefinitionResolver(&controlplane.TableExecutionGraphRepository{DB: plane.DB}, graphs, plane.Metrics)
		if err != nil {
			return err
		}
	}
	if plane.Idempotency == nil {
		plane.Idempotency = &StepIdempotencyStore{DB: plane.DB, Objects: plane.Objects, ClaimLease: settings.StepClaimLease}
	}
	return nil
}

func (plane *BaseDataPlane) composeGovernance() error {
	settings := plane.Settings
	if plane.Policies == nil {
		plane.Policies = &controlplane.TableTenantPolicyRepository{DB: plane.DB}
	}
	if plane.GuardrailConfigs == nil {
		plane.GuardrailConfigs = &controlplane.TableGuardrailConfigRepository{DB: plane.DB}
	}
	if plane.Governor == nil {
		// The breaker limits are zero, so it records spend without tripping; replace Governor to enforce one.
		plane.Governor = &isolation.ExecutionGovernor{
			Loops: &isolation.MemoryLoopDetector{Threshold: plane.Limits.MaxRepeatedCalls, MaxConversations: settings.SessionCacheSize},
			Costs: &isolation.WindowedCostBreaker{Currency: plane.Currency, Window: costBreakerWindow, MaxEntries: settings.SessionCacheSize},
		}
	}
	if plane.Audit == nil {
		plane.Audit = &observability.TableAuditSink{DB: plane.DB, Evidence: plane.Objects}
	}
	if plane.Guardrails == nil {
		compiler, err := guardrail.NewRuleSetCompiler(guardrail.CompilerConfig{})
		if err != nil {
			return err
		}
		plane.Guardrails, err = guardrail.NewLayeredEnforcer(guardrail.EnforcerConfig{
			Baseline:  guardrail.DefaultBaseline(settings.GuardrailMaxInputBytes, settings.GuardrailMaxOutputBytes),
			Compiler:  compiler,
			Approvals: plane.Approvals,
			Events:    &observability.TableSafetyEventSink{DB: plane.DB},
			Audit:     plane.Audit,
		})
		if err != nil {
			return err
		}
	}
	if plane.Estimator == nil {
		plane.Estimator = LoopBudgetEstimator{Limits: plane.Limits, Currency: plane.Currency}
	}
	return nil
}

func (plane *BaseDataPlane) composeDelivery() error {
	settings := plane.Settings
	if plane.ReplyHub == nil {
		plane.ReplyHub = &CacheReplyHub{Cache: plane.Cache, Records: plane.Records}
		plane.closers = append(plane.closers, plane.ReplyHub)
	}
	if plane.Cancels == nil {
		plane.Cancels = &TableTurnCanceller{DB: plane.DB, Cache: plane.Cache}
		plane.closers = append(plane.closers, plane.Cancels)
	}
	if plane.DeadLetters == nil {
		plane.DeadLetters = &TableDeadLetterQueue{DB: plane.DB}
	}
	if plane.TurnScheduler == nil {
		plane.TurnScheduler = &QueueTurnScheduler{
			DB: plane.DB, Objects: plane.Objects, Weights: plane.Weights,
			DeadLetters: plane.DeadLetters, MaxAttempts: settings.QueueMaxAttempts,
		}
	}
	if err := plane.TurnScheduler.validate(); err != nil {
		return err
	}
	if plane.Dispatcher == nil {
		dispatcher := &QueueTurnDispatcher{DB: plane.DB, Partitions: settings.QueuePartitions, ShardsPerTenant: settings.QueueShards}
		if err := dispatcher.validate(); err != nil {
			return err
		}
		plane.Dispatcher = dispatcher
	}
	return nil
}

func (plane *BaseDataPlane) composeTools() error {
	settings := plane.Settings
	if plane.Tools == nil {
		plane.Tools = &toolgateway.TableToolRegistry{DB: plane.DB, DefaultTimeout: settings.ToolTimeout, DefaultMaxAttempts: settings.ToolMaxAttempts}
	}
	if plane.Skills == nil {
		plane.Skills = &skill.TableSkillRegistry{DB: plane.DB}
	}
	// A transport that is not in-process serves use_skill itself, through skill.UseSkill.
	if inProcess, ok := plane.Transport.(*toolgateway.InProcessTransport); ok && !inProcess.Serves(domain.UseSkillToolID) {
		if err := (&skill.UseSkill{Skills: plane.Skills}).Register(inProcess); err != nil {
			return err
		}
	}
	if plane.Credentials == nil {
		inProcess, ok := plane.Transport.(*toolgateway.InProcessTransport)
		if !ok {
			return fmt.Errorf("%w: a tool transport that is not in-process needs a credential provider", domain.ErrValidation)
		}
		plane.Credentials = &toolgateway.InProcessCredentials{Transport: inProcess}
	}
	if plane.Egress == nil {
		plane.Egress = &toolgateway.TableEgressPolicy{DB: plane.DB}
	}
	if plane.ToolGateway == nil {
		gateway, err := toolgateway.NewGovernedGateway(toolgateway.GovernedGatewayConfig{
			Registry: plane.Tools, RateLimiter: plane.RateLimiter, Authorizer: &toolgateway.BindingAuthorizer{Registry: plane.Tools},
			Credentials: plane.Credentials, Egress: plane.Egress, Transport: plane.Transport,
			Validator: toolgateway.SchemaResultValidator{}, Effects: plane.Effects,
			RetryAttempts: settings.ToolMaxAttempts, RetryBaseDelay: 200 * time.Millisecond, RetryMaxDelay: 5 * time.Second,
			Guardrails: plane.Guardrails, GuardrailConfigs: &toolgateway.ReleaseGuardrailConfigs{Configs: plane.GuardrailConfigs},
			Timeout: settings.ToolTimeout,
		})
		if err != nil {
			return err
		}
		gateway.Audit = plane.Audit
		plane.ToolGateway = gateway
	}
	return nil
}

func (plane *BaseDataPlane) composeLoop() error {
	// One catalog answers both: routing and the gateway's capability confirmation must
	// agree, and a request carrying tools is refused outright without it.
	if plane.Catalog == nil {
		plane.Catalog = &modelgateway.TableCandidateCatalog{DB: plane.DB, Region: plane.Settings.ModelRegion}
	}
	if plane.Router == nil {
		plane.Router = &modelgateway.PinnedModelRouter{Catalog: plane.Catalog}
	}
	if plane.Models == nil {
		gateway, err := modelgateway.NewGateway(plane.RateLimiter, &modelgateway.FactoryProviderRegistry{Factory: plane.Providers}, plane.Capacity)
		if err != nil {
			return err
		}
		gateway.Catalog = plane.Catalog
		plane.Models = gateway
	}
	if plane.Requests == nil {
		plane.Requests = &ReleaseLoopRequestBuilder{
			Definitions: &controlplane.TableAgentDefinitionReader{DB: plane.DB}, Renderer: &agentruntime.PromptRenderer{}, Skills: plane.Skills,
			Knowledge: plane.Knowledge, Entitlements: plane.Entitlements, KnowledgeMaxBytes: plane.Settings.GuardrailMaxInputBytes,
			LanguageCode: plane.LanguageCode, MaxOutputTokens: plane.MaxOutputTokens, Task: plane.Task,
		}
	}
	if plane.Journal == nil {
		plane.Journal = &TableLoopJournal{DB: plane.DB, Objects: plane.Objects}
	}
	if plane.Evidence == nil {
		verifier, ok := plane.Objects.(contract.EvidenceObjectVerifier)
		if !ok {
			return fmt.Errorf("%w: an object codec that cannot verify evidence needs an evidence validator", domain.ErrValidation)
		}
		plane.Evidence = &guardrail.EvidenceValidator{Objects: verifier}
	}
	if plane.Executors == nil {
		loop, err := NewToolLoopExecutor(ToolLoopExecutor{
			Requests: plane.Requests, Router: plane.Router, Models: plane.Models, Registry: plane.Tools, Tools: plane.ToolGateway,
			Journal: plane.Journal, Pricer: plane.Pricer, Evidence: plane.Evidence, Limits: plane.Limits,
		})
		if err != nil {
			return err
		}
		executors := NewStepExecutorRegistry()
		if err = executors.Register(domain.StepKindToolLoop, loop); err != nil {
			return err
		}
		plane.Executors = executors
	}
	return nil
}

func (plane *BaseDataPlane) composeRuntime() error {
	if plane.TurnRuntime == nil {
		runtime := &TurnRuntime{
			Records: plane.Records, Sessions: plane.Sessions, Definitions: plane.Definitions, Policies: plane.Policies,
			Governor: plane.Governor, Executors: plane.Executors, Idempotency: plane.Idempotency, Guardrails: plane.Guardrails,
			Publisher: plane.ReplyHub, Estimator: plane.Estimator, Budget: plane.Budget, Cancels: plane.Cancels,
			GuardrailConfigs: plane.GuardrailConfigs, Audit: plane.Audit, OnSettled: plane.OnSettled,
			MaxSteps: plane.Settings.TurnMaxSteps,
		}
		if recorder, ok := plane.Metrics.(contract.ObservationRecorder); ok {
			runtime.Observations = recorder
		}
		if err := runtime.validate(); err != nil {
			return err
		}
		plane.TurnRuntime = runtime
	}
	if plane.TurnIngress == nil {
		ingress := &TurnIngress{
			Limiter: plane.RateLimiter, Records: plane.Records, Objects: plane.Objects, Estimator: plane.Estimator,
			Budget: plane.Budget, Replies: plane.ReplyHub, Dispatcher: plane.Dispatcher, Audit: plane.Audit,
		}
		if err := ingress.validate(); err != nil {
			return err
		}
		plane.TurnIngress = ingress
	}
	return nil
}

// NewStateStorage binds object storage to the private agent_state_bucket.
func NewStateStorage(ctx context.Context, secrets secret.SecretProvider, settings domain.DataPlaneSettings) (storage.ObjectStorage, error) {
	if strings.TrimSpace(settings.StateBucket) == "" {
		return nil, fmt.Errorf("%w: agent_state_bucket is not configured; turn input and conversation state need a private bucket", domain.ErrNotReady)
	}
	objects, err := storage.NewFromConfig(ctx, secrets, settings.StateBucket)
	if err != nil {
		return nil, fmt.Errorf("bind agent_state_bucket %q: %w", settings.StateBucket, err)
	}
	return objects, nil
}
