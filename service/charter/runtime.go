// Package charter composes Charter's governed pipeline over Scout: MCP tools are the bindings, keel supplies
// identity, RBAC, trust guards, evidence storage and ids, and Scout owns nothing Charter defines.
package charter

import (
	"context"
	"fmt"

	charterkeel "github.com/nauticana/charter/sdk/adapter/keel"
	"github.com/nauticana/charter/sdk/agent"
	"github.com/nauticana/charter/sdk/authority"
	"github.com/nauticana/charter/sdk/binding"
	"github.com/nauticana/charter/sdk/capability"
	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/evidence"
	"github.com/nauticana/charter/sdk/identity"
	"github.com/nauticana/charter/sdk/information"
	"github.com/nauticana/charter/sdk/organization"
	"github.com/nauticana/charter/sdk/validate"
	"github.com/nauticana/keel/guard"
	"github.com/nauticana/keel/idempotency"
	keelport "github.com/nauticana/keel/port"
)

// Runtime is the composition root. Every field is optional except Documents and Transport: nil leaves a keel
// concern out (no RBAC layer, no guards, no publishing) and falls back to Charter's in-memory bases.
type Runtime struct {
	Documents corpus.Source
	Transport binding.Executor
	// Observer reads effects back; without one a contract with required postconditions is denied before the transport.
	Observer binding.Observer
	// Store keeps evidence; nil is in-memory. Publisher additionally publishes each redacted document to Topic.
	Store     evidence.Store
	Publisher keelport.MessagePublisher
	Topic     string
	// Ledger records capability idempotency; nil uses Database when set, else an in-memory ledger.
	Ledger capability.Ledger
	// Database makes the capability ledger durable across restarts.
	Database keelport.DatabaseRepository
	IDs      capability.IDGenerator
	// Keel, Identities and Permissions layer keel RBAC behind Charter authority; all three or none.
	Keel        charterkeel.PermissionChecker
	Identities  charterkeel.IdentityMap
	Permissions map[corpus.DocumentKey]charterkeel.Permission
	Guards      guard.TrustGuard
	Querier     guard.GuardQuerier
	Metrics     keelport.MetricsRecorder
	Deliveries  binding.DeliveryLedger
	Dispatcher  TriggerDispatcher
	// ReadWithoutGrant lets read-class contracts proceed on their assignment when no grant exists.
	ReadWithoutGrant bool
}

var _ validate.Subject = Runtime{}

// Compose builds the governed runtime over documents. Non-nil members of external replace Runtime.Transport and
// Runtime.Observer, which is how the conformance harness scripts the external system.
func (r Runtime) Compose(_ context.Context, documents corpus.Source, external validate.External) (validate.Runtime, error) {
	if documents == nil {
		documents = r.Documents
	}
	transport, observer := external.Transport, external.Observer
	if transport == nil {
		transport = r.Transport
	}
	if observer == nil {
		observer = r.Observer
	}
	if documents == nil || transport == nil {
		return validate.Runtime{}, fmt.Errorf("charter runtime: documents and transport are required")
	}
	if (r.Keel == nil) != (r.Identities == nil) || (r.Keel == nil) != (r.Permissions == nil) {
		return validate.Runtime{}, fmt.Errorf("charter runtime: keel RBAC needs checker, identities and permissions together")
	}
	store := r.Store
	if store == nil {
		store = evidence.NewBaseMemoryStore()
	}
	ids := r.IDs
	if ids == nil {
		ids = &capability.BaseCounterIDs{Prefix: "SCOUT-"}
	}
	ledger := r.ledger()
	redactor := evidence.BaseRedactor{}
	var sink evidence.Sink = &evidence.AbstractSink{Store: store, Digester: evidence.BaseSHA256Digester{}, Redactor: redactor}
	if r.Publisher != nil {
		sink = &charterkeel.PublishingSink{Next: sink, Publisher: r.Publisher, Topic: r.Topic, Redactor: redactor}
	}
	agents, org := agent.NewBaseProvider(documents), organization.NewBaseProvider(documents)
	records := evidence.NewBaseProvider(store)
	var grants authority.Evaluator = &authority.AbstractEvaluator{Source: authority.NewDocumentGrantSource(documents)}
	if r.Keel != nil {
		grants = &charterkeel.PermissionGate{Charter: grants, Keel: r.Keel, Identities: r.Identities, Permissions: r.Permissions}
	}
	base := &capability.BaseInvoker{
		Catalog:          capability.NewBaseCatalog(documents),
		Identities:       identity.NewBaseResolver(documents),
		Authority:        grants,
		Approvals:        &authority.AbstractApprovalGate{Source: authority.NewDocumentApprovalSource(documents)},
		Sod:              capability.NewBaseSodChecker(documents, records),
		Information:      &information.BaseEvaluator{Provider: information.NewBaseProvider(documents)},
		Bindings:         binding.NewBaseProvider(documents),
		Transport:        transport,
		Observer:         observer,
		Ledger:           ledger,
		Evidence:         sink,
		Escalation:       &capability.BaseEscalator{Recipients: &capability.BaseAccountableRecipient{Agents: agents, Organization: org}, IDs: ids},
		IDs:              ids,
		ReadWithoutGrant: r.ReadWithoutGrant,
	}
	var invoker capability.Invoker = base
	if r.Guards != nil {
		invoker = &charterkeel.GuardedInvoker{Next: invoker, Guards: r.Guards, Querier: r.Querier}
	}
	if r.Metrics != nil {
		invoker = &charterkeel.MetricsInvoker{Next: invoker, Metrics: r.Metrics}
	}
	return validate.Runtime{Admission: &agent.BaseAdmission{Agents: agents, Assignments: org}, Invoker: invoker, Reconciler: base, Evidence: records}, nil
}

// ledger prefers an explicit ledger, then a durable keel-backed one, then memory. The invoker runs its transport
// synchronously, so the claim is never renewed and the lease stays zero.
func (r Runtime) ledger() capability.Ledger {
	switch {
	case r.Ledger != nil:
		return r.Ledger
	case r.Database != nil:
		return &charterkeel.Ledger{Keel: idempotency.NewPgsqlLedger(r.Database, 0)}
	default:
		return capability.NewBaseMemoryLedger()
	}
}

// Events composes the delivery path: an event binding's declared semantics, a claim ledger, and dispatch to every
// definition declaring the trigger.
func (r Runtime) Events() (*binding.AbstractEventConsumer, error) {
	if r.Documents == nil || r.Dispatcher == nil {
		return nil, fmt.Errorf("charter runtime: events need documents and a dispatcher")
	}
	ledger := r.Deliveries
	if ledger == nil {
		ledger = binding.NewBaseMemoryDeliveryLedger()
	}
	return &binding.AbstractEventConsumer{
		Bindings: binding.NewBaseProvider(r.Documents),
		Ledger:   ledger,
		Handler:  &TriggerHandler{Agents: agent.NewBaseProvider(r.Documents), Dispatch: r.Dispatcher},
	}, nil
}
