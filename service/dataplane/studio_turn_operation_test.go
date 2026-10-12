package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/observability"
)

// turnOperationFake is the operator half of turnControlFake: a held turn taken over, entries
// appended under the lease, and the gates of handback and finish.
type turnOperationFake struct {
	holder    map[string]domain.PrincipalRef
	entries   []domain.OperatorEntry
	intents   []domain.OperatorCallIntent
	acts      []domain.OperatorAct
	finishes  []domain.OperatorFinish
	handbacks []string
	// open is an open call outcome of the turn, lapsed a lease the store clock ended.
	open, lapsed bool
	finishErr    error
}

func (control *turnControlFake) lease(lease domain.OperatorLease) error {
	op := &control.operation
	if control.status[lease.RequestID] != domain.TurnOperated || control.fence[lease.RequestID] != lease.Fence ||
		op.holder[lease.RequestID] != lease.Holder || op.lapsed {
		return fmt.Errorf("%w: no lease", domain.ErrFenceLost)
	}
	return nil
}

func (control *turnControlFake) TakeOver(_ context.Context, takeover domain.TurnTakeover) (domain.OperatorHold, error) {
	op := &control.operation
	if control.status[takeover.RequestID] != domain.TurnHeld || control.fence[takeover.RequestID] != takeover.Fence || op.open {
		return domain.OperatorHold{}, fmt.Errorf("%w: not held there, or an outcome is open", domain.ErrConflict)
	}
	if op.holder == nil {
		op.holder = map[string]domain.PrincipalRef{}
	}
	control.status[takeover.RequestID], control.fence[takeover.RequestID] = domain.TurnOperated, takeover.Fence+1
	op.holder[takeover.RequestID] = takeover.Holder
	return domain.OperatorHold{Lease: domain.OperatorLease{TenantID: takeover.TenantID, RequestID: takeover.RequestID, Fence: takeover.Fence + 1, Holder: takeover.Holder}}, nil
}

func (control *turnControlFake) IntendCall(_ context.Context, lease domain.OperatorLease, tool domain.ToolReference, window time.Duration) (domain.OperatorCallIntent, error) {
	if err := control.lease(lease); err != nil {
		return domain.OperatorCallIntent{}, err
	}
	entryNo := len(control.operation.entries) + 1
	intent := domain.OperatorCallIntent{
		Key:            domain.CallOutcomeKey{TenantID: lease.TenantID, RequestID: lease.RequestID, ExecutionStepID: 11, CallID: fmt.Sprintf("op:%d:%d", lease.Fence, entryNo)},
		IdempotencyKey: operatorIdempotencyKey(lease, entryNo), EntryNo: entryNo, Tool: tool, Window: window,
	}
	control.operation.intents = append(control.operation.intents, intent)
	return intent, nil
}

func (control *turnControlFake) appendEntry(lease domain.OperatorLease, kind domain.OperatorEntryKind, body domain.OperatorEntryBody, usage domain.Usage) domain.OperatorEntry {
	op := &control.operation
	replies := 0
	for _, entry := range op.entries {
		if entry.Kind == domain.OperatorReply {
			replies++
		}
	}
	if body.Frame != nil {
		frame := *body.Frame
		frame.Sequence, frame.Fence = int64(2+replies), lease.Fence
		body.Frame = &frame
	}
	entry := domain.OperatorEntry{TenantID: lease.TenantID, RequestID: lease.RequestID, Fence: lease.Fence, EntryNo: len(op.entries) + 1,
		Kind: kind, Actor: lease.Holder, Position: domain.OperatorPosition{StepNo: 2, ExecutionStepID: 11, AfterEntryNo: 3}, Body: body, Usage: usage}
	op.entries = append(op.entries, entry)
	return entry
}

func (control *turnControlFake) Act(_ context.Context, act domain.OperatorAct) (domain.OperatorActed, error) {
	if err := control.lease(act.Lease); err != nil {
		return domain.OperatorActed{}, err
	}
	control.operation.acts = append(control.operation.acts, act)
	acted := domain.OperatorActed{Entry: control.appendEntry(act.Lease, act.Kind, act.Body, act.Usage)}
	if act.Call != nil && act.Settlement == nil {
		control.status[act.Lease.RequestID], control.fence[act.Lease.RequestID] = domain.TurnHeld, act.Lease.Fence+1
		acted.HeldFence = act.Lease.Fence + 1
	}
	return acted, nil
}

func (control *turnControlFake) HandBack(_ context.Context, lease domain.OperatorLease, instruction string) (domain.OperatorHandedBack, error) {
	if err := control.lease(lease); err != nil {
		return domain.OperatorHandedBack{}, err
	}
	op := &control.operation
	op.handbacks = append(op.handbacks, lease.RequestID)
	if op.open {
		return domain.OperatorHandedBack{}, fmt.Errorf("%w: an outcome is open", domain.ErrConflict)
	}
	handed := domain.OperatorHandedBack{Fence: lease.Fence + 1}
	if instruction != "" {
		entry := control.appendEntry(lease, domain.OperatorHandback, domain.OperatorEntryBody{Text: instruction}, domain.Usage{})
		handed.Instruction = &entry
	}
	control.status[lease.RequestID], control.fence[lease.RequestID] = domain.TurnQueued, lease.Fence+1
	return handed, nil
}

func (control *turnControlFake) Finish(_ context.Context, finish domain.OperatorFinish) (domain.OperatorEntry, error) {
	if err := control.lease(finish.Lease); err != nil {
		return domain.OperatorEntry{}, err
	}
	op := &control.operation
	op.finishes = append(op.finishes, finish)
	if op.open {
		return domain.OperatorEntry{}, fmt.Errorf("%w: an outcome is open", domain.ErrConflict)
	}
	if op.finishErr != nil {
		return domain.OperatorEntry{}, op.finishErr
	}
	frame := finish.Frame
	entry := control.appendEntry(finish.Lease, domain.OperatorReply, domain.OperatorEntryBody{Frame: &frame}, domain.Usage{})
	control.status[finish.Lease.RequestID] = domain.TurnCompleted
	return entry, nil
}

func (control *turnControlFake) OperatorEntries(context.Context, int64, string) ([]domain.OperatorEntry, error) {
	return append([]domain.OperatorEntry(nil), control.operation.entries...), nil
}

// operatedQuery answers the turn read from the fake control's state, so every read sees the
// transition the control made; everything else goes to the scripted fake.
type operatedQuery struct {
	*queueQueryFake
	control *turnControlFake
}

func (query operatedQuery) Query(ctx context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if name != qOperateTurn {
		return query.queueQueryFake.Query(ctx, name, args...)
	}
	query.calls = append(query.calls, name)
	requestID := args[2].(string)
	status := query.control.status[requestID]
	holder := query.control.operation.holder[requestID]
	row := liveTurnRow(requestID, status, query.control.fence[requestID], string(domain.HoldCheckpoint))
	if status == domain.TurnOperated || status == domain.TurnCompleted {
		row[12], row[13] = string(holder.Kind), holder.ID
	}
	return &keelmodel.QueryResult{Rows: [][]any{row}}, nil
}

type operatedDB struct {
	keelport.DatabaseRepository
	query operatedQuery
}

func (db operatedDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.query
}

type operationFixture struct {
	*turnControlFixture
	agentReserved []domain.BudgetRequest
	operatorHolds []domain.BudgetRequest
	calls         []domain.ToolCall
	scopes        []string
	published     []domain.TurnReply
	gateway       func(domain.ToolCall) (domain.ToolResult, error)
}

// newOperationFixture is a turn of writer-a held at fence 3 that the actor may operate, with a
// release binding shop.write@2 and a gateway that records every call it is given.
func newOperationFixture(t *testing.T) *operationFixture {
	t.Helper()
	fixture := &operationFixture{turnControlFixture: newTurnControlFixture(domain.OperationView, domain.OperationOperate)}
	fixture.query.rows[qOperateRelease] = [][]any{{"v3", "end-user-5"}}
	fixture.service.DB = operatedDB{query: operatedQuery{queueQueryFake: fixture.query, control: fixture.control}}
	fixture.budget.ReserveFunc = func(_ context.Context, request domain.BudgetRequest) (domain.BudgetReservation, error) {
		fixture.agentReserved = append(fixture.agentReserved, request)
		reservation := domain.BudgetReservation{TenantID: request.TenantID, ReservationID: "reservation-2", RequestID: request.RequestID, Currency: "USD"}
		fixture.reserved = append(fixture.reserved, reservation)
		return reservation, nil
	}
	fixture.service.OperatorBudget = &operatorBudgetFake{
		reserve: func(_ context.Context, request domain.BudgetRequest) (domain.BudgetReservation, error) {
			fixture.operatorHolds = append(fixture.operatorHolds, request)
			return domain.BudgetReservation{TenantID: request.TenantID, ReservationID: "operator-1", RequestID: request.RequestID, Currency: request.Currency}, nil
		},
		release: fixture.budget.Release,
	}
	fixture.service.Tools = &fake.ToolRegistry{ListFunc: func(_ context.Context, tenantID int64, agentID, version string) ([]domain.ToolDefinition, error) {
		if tenantID != 7 || agentID != "writer-a" || version != "v3" {
			return nil, fmt.Errorf("unexpected release %d/%s@%s", tenantID, agentID, version)
		}
		return []domain.ToolDefinition{{ToolID: "shop.write", Version: "2", Timeout: time.Second, MaxAttempts: 1}}, nil
	}}
	fixture.gateway = func(domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Output: []byte(`{"refund":"r-1"}`), Usage: domain.Usage{CostMinorUnits: 40, Currency: "USD"}}, nil
	}
	fixture.service.Gateway = fake.GovernedToolGatewayFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		fixture.calls = append(fixture.calls, call)
		fixture.scopes = append(fixture.scopes, domain.DecisionKeyFor(ctx, domain.DecisionRecord{TenantID: 7, Category: "probe"}))
		return fixture.gateway(call)
	})
	fixture.service.Dispatch = domain.ToolDispatchWindow{Timeout: time.Second, Attempts: 1, Grace: time.Minute}
	fixture.service.Guardrails = &fake.GuardrailEnforcer{}
	fixture.service.Definitions = fake.DefinitionResolverFunc(func(context.Context, int64, string, string) (domain.ExecutionGraph, error) {
		return domain.ExecutionGraph{AgentID: "writer-a", Version: "v3", Steps: []domain.ExecutionStep{{ExecutionStepID: 11, StepID: "answer", Kind: domain.StepKindToolLoop}}}, nil
	})
	fixture.service.Publisher = fake.TurnReplyPublisherFunc(func(_ context.Context, reply domain.TurnReply) error {
		fixture.published = append(fixture.published, reply)
		return nil
	})
	return fixture
}

type operatorBudgetFake struct {
	reserve func(context.Context, domain.BudgetRequest) (domain.BudgetReservation, error)
	release func(context.Context, domain.BudgetReservation) error
}

func (budget *operatorBudgetFake) ReserveOperatorAction(ctx context.Context, request domain.BudgetRequest) (domain.BudgetReservation, error) {
	return budget.reserve(ctx, request)
}

func (budget *operatorBudgetFake) Release(ctx context.Context, reservation domain.BudgetReservation) error {
	return budget.release(ctx, reservation)
}

// takeOver takes the fixture's turn at its held fence 3 and returns the operated fence 4.
func (fixture *operationFixture) takeOver(t *testing.T) int64 {
	t.Helper()
	turn, err := fixture.service.TakeOver(context.Background(), usageActor, domain.TurnTakeoverRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 3})
	if err != nil || turn.Status != domain.TurnOperated || turn.Fence != 4 || turn.Holder != usageActor.Principal {
		t.Fatalf("takeover = %+v, %v", turn, err)
	}
	return turn.Fence
}

// §2: takeover needs OPERATE and the held fence the operator saw, is refused while an outcome is
// open, is a person's act only, and is audited at the fence it left the turn at.
func TestStudioTakeoverIsRefusedWithAStaleFenceOrAnOpenOutcome(t *testing.T) {
	ctx := context.Background()
	request := domain.TurnTakeoverRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 3}

	viewer := newOperationFixture(t)
	viewer.service.Roles = operateRoles{actions: []domain.OperationAction{domain.OperationView}}
	if _, err := viewer.service.TakeOver(ctx, usageActor, request); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("takeover without OPERATE = %v", err)
	}
	stale := newOperationFixture(t)
	if _, err := stale.service.TakeOver(ctx, usageActor, domain.TurnTakeoverRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 2}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a stale fence = %v, want conflict", err)
	}
	open := newOperationFixture(t)
	open.control.operation.open = true
	if _, err := open.service.TakeOver(ctx, usageActor, request); !errors.Is(err, domain.ErrConflict) || open.control.status["request-1"] != domain.TurnHeld {
		t.Fatalf("an open outcome = %v, status %s", err, open.control.status["request-1"])
	}
	service := newOperationFixture(t)
	if _, err := service.service.TakeOver(ctx, domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalService, ID: "bot"}}, request); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a service principal took over a turn: %v", err)
	}
	fixture := newOperationFixture(t)
	fixture.takeOver(t)
	records := fixture.audit.Request(7, "request-1")
	if got := categories(records); !slices.Equal(got, []string{observability.AuditCategoryTurnTakenOver}) || !strings.Contains(string(records[0].Payload), `"fence":4`) {
		t.Fatalf("audit = %v %s", got, records[0].Payload)
	}
	// Taking over again at the old fence is stale.
	if _, err := fixture.service.TakeOver(ctx, usageActor, request); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a second takeover at the held fence = %v", err)
	}
}

// §8.7: an act after the lease lapsed, at another fence, or by another person is a 409 refusal.
func TestStudioOperatorActIsRefusedAfterTheLeaseLapsed(t *testing.T) {
	ctx := context.Background()
	fixture := newOperationFixture(t)
	fence := fixture.takeOver(t)
	note := domain.TurnActRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence, Kind: domain.OperatorNote, Text: "check the order"}
	fixture.control.operation.lapsed = true
	if _, err := fixture.service.Act(ctx, usageActor, note); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("a lapsed lease = %v, want fence lost", err)
	}
	fixture.control.operation.lapsed = false
	if _, err := fixture.service.Act(ctx, usageActor, domain.TurnActRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence - 1, Kind: domain.OperatorNote, Text: "x"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a stale fence = %v", err)
	}
	other := domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "43"}}
	if _, err := fixture.service.Act(ctx, other, note); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("another person's act = %v", err)
	}
	// After the sweeper held the turn again, the read refuses before any write.
	fixture.control.status["request-1"], fixture.control.fence["request-1"] = domain.TurnHeld, fence+1
	if _, err := fixture.service.Act(ctx, usageActor, note); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("an act on a lapsed and swept turn = %v", err)
	}
	if len(fixture.control.operation.entries) != 0 {
		t.Fatalf("a refused act appended %+v", fixture.control.operation.entries)
	}
}

// §5: operator records use turn/<req>/fence/<f>/op/<n>, so each entry records once and keys are
// distinct per epoch.
func TestStudioOperatorNotesAreAuditedPerEntry(t *testing.T) {
	ctx := context.Background()
	fixture := newOperationFixture(t)
	fence := fixture.takeOver(t)
	for _, text := range []string{"first", "second"} {
		result, err := fixture.service.Act(ctx, usageActor, domain.TurnActRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence, Kind: domain.OperatorNote, Text: text})
		if err != nil || result.Entry.Body.Text != text || result.Turn.Status != domain.TurnOperated {
			t.Fatalf("note = %+v, %v", result, err)
		}
	}
	records := fixture.audit.Request(7, "request-1")
	if got := categories(records); !slices.Equal(got, []string{observability.AuditCategoryTurnTakenOver, observability.AuditCategoryTurnOperatorAction, observability.AuditCategoryTurnOperatorAction}) {
		t.Fatalf("audit = %v", got)
	}
	if records[1].DecisionKey == records[2].DecisionKey || records[1].Principal != usageActor.Principal {
		t.Fatalf("operator records = %+v", records[1:])
	}
	want := domain.DecisionKeyFor(domain.WithDecisionScope(ctx, "turn/", "request-1", "/fence/", fence, "/op/", 2),
		domain.DecisionRecord{TenantID: 7, Category: observability.AuditCategoryTurnOperatorAction, Action: "turn", Resource: "writer-a"})
	if records[2].DecisionKey != want {
		t.Fatalf("the second entry is not keyed under its op scope")
	}
}

// Decision 5 and §5: a reply passes the release's output guardrails and evidence rule and is
// published at its sequence; a blocked reply appends nothing.
func TestStudioOperatorReplyPassesTheGuardrailsAndTheEvidenceRule(t *testing.T) {
	ctx := context.Background()
	fixture := newOperationFixture(t)
	fence := fixture.takeOver(t)
	fixture.service.Guardrails = &fake.GuardrailEnforcer{AfterModelChunkFunc: func(_ context.Context, _ domain.GuardrailConfig, subject domain.GuardrailSubject, chunk domain.ModelChunk) (domain.ModelChunk, error) {
		if subject.Principal != usageActor.Principal || subject.ReleaseVersion != "v3" {
			t.Errorf("guardrail subject = %+v", subject)
		}
		if strings.Contains(string(chunk.Payload), "secret") {
			return domain.ModelChunk{}, fmt.Errorf("%w: blocked", domain.ErrForbidden)
		}
		chunk.Payload = []byte(strings.ReplaceAll(string(chunk.Payload), "4111", "[redacted]"))
		return chunk, nil
	}}
	reply := domain.TurnActRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence, Kind: domain.OperatorReply, Text: "card 4111 refunded"}
	result, err := fixture.service.Act(ctx, usageActor, reply)
	if err != nil || !result.Published || len(fixture.published) != 1 {
		t.Fatalf("reply = %+v, %v", result, err)
	}
	frame := fixture.published[0]
	if string(frame.Payload) != "card [redacted] refunded" || frame.Fence != fence || frame.Sequence != 2 || frame.Final ||
		frame.ReplyRoute != "route:request-1" || frame.AgentVersion != "v3" || frame.Events[0].Kind != domain.TurnEventOperatorReply {
		t.Fatalf("frame = %+v", frame)
	}
	reply.Text = "the secret"
	if _, err = fixture.service.Act(ctx, usageActor, reply); !errors.Is(err, domain.ErrForbidden) || len(fixture.control.operation.entries) != 1 {
		t.Fatalf("a blocked reply = %v, entries %d", err, len(fixture.control.operation.entries))
	}

	evidenced := newOperationFixture(t)
	fence = evidenced.takeOver(t)
	evidenced.service.Definitions = fake.DefinitionResolverFunc(func(context.Context, int64, string, string) (domain.ExecutionGraph, error) {
		return domain.ExecutionGraph{Steps: []domain.ExecutionStep{{ExecutionStepID: 11, StepID: "answer", Kind: domain.StepKindToolLoop, Configuration: []byte(`{"require_evidence":true}`)}}}, nil
	})
	plain := domain.TurnActRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence, Kind: domain.OperatorReply, Text: "trust me"}
	if _, err = evidenced.service.Act(ctx, usageActor, plain); !errors.Is(err, domain.ErrDegraded) {
		t.Fatalf("an evidence rule without a validator = %v, want degraded", err)
	}
	var scope domain.EvidenceScope
	evidenced.service.Claims = evidenceValidatorFunc(func(_ context.Context, got domain.EvidenceScope, answer domain.EvidencedAnswer) (domain.EvidenceReport, error) {
		scope = got
		if len(answer.Claims) > 0 && answer.Claims[0].Text == "unsupported" {
			return domain.EvidenceReport{Unsupported: []domain.ClaimFinding{{ClaimIndex: 0, Reason: "no evidence"}}}, nil
		}
		return domain.EvidenceReport{}, nil
	})
	if _, err = evidenced.service.Act(ctx, usageActor, plain); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a plain reply under require_evidence = %v", err)
	}
	unsupported := plain
	unsupported.Text = `{"answer":"a","claims":[{"text":"unsupported"}],"evidence":[]}`
	if _, err = evidenced.service.Act(ctx, usageActor, unsupported); !errors.Is(err, domain.ErrUnsupportedClaim) {
		t.Fatalf("an unsupported claim = %v", err)
	}
	supported := plain
	supported.Text = `{"answer":"a","claims":[],"evidence":[]}`
	if _, err = evidenced.service.Act(ctx, usageActor, supported); err != nil || scope.TenantID != 7 {
		t.Fatalf("an evidenced reply = %v, scope %+v", err, scope)
	}
}

type evidenceValidatorFunc func(context.Context, domain.EvidenceScope, domain.EvidencedAnswer) (domain.EvidenceReport, error)

func (function evidenceValidatorFunc) Validate(ctx context.Context, scope domain.EvidenceScope, answer domain.EvidencedAnswer) (domain.EvidenceReport, error) {
	return function(ctx, scope, answer)
}

// Decision 5: a tool the pinned release does not bind is refused before any spend or record, and
// a call the gateway refuses (policy, the person's grants, credentials) never leaves: its intent
// is retracted, its hold released, and no entry records it.
func TestStudioOperatorToolAuthorityIsTheIntersection(t *testing.T) {
	ctx := context.Background()
	fixture := newOperationFixture(t)
	fence := fixture.takeOver(t)
	call := domain.TurnActRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence, Kind: domain.OperatorTool,
		Tool: domain.ToolReference{ToolID: "shop.delete", Version: "2"}, Arguments: []byte(`{}`)}
	if _, err := fixture.service.Act(ctx, usageActor, call); !errors.Is(err, domain.ErrForbidden) ||
		len(fixture.operatorHolds) != 0 || len(fixture.control.operation.intents) != 0 || len(fixture.calls) != 0 {
		t.Fatalf("an unbound tool = %v, holds %d, intents %d, calls %d", err, len(fixture.operatorHolds), len(fixture.control.operation.intents), len(fixture.calls))
	}
	call.Tool = domain.ToolReference{ToolID: "shop.write", Version: "2"}
	fixture.gateway = func(domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, fmt.Errorf("%w: %w: policy denies the person", domain.ErrNotSent, domain.ErrForbidden)
	}
	_, err := fixture.service.Act(ctx, usageActor, call)
	if !errors.Is(err, domain.ErrForbidden) || len(fixture.control.operation.entries) != 0 || !slices.Equal(fixture.refunded, []string{"operator-1"}) {
		t.Fatalf("a refused call = %v, entries %d, released %v", err, len(fixture.control.operation.entries), fixture.refunded)
	}
	if len(fixture.calls) != 1 {
		t.Fatalf("calls = %d", len(fixture.calls))
	}
	sent := fixture.calls[0]
	if sent.Principal.Kind != domain.PrincipalAgent || sent.Principal.ID != "writer-a" || sent.Principal.Release != "v3" ||
		sent.Operator.Kind != domain.PrincipalHuman || sent.Operator.ID != "42" || sent.OnBehalfOf != (domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "end-user-5"}) ||
		sent.IdempotencyKey != operatorIdempotencyKey(domain.OperatorLease{TenantID: 7, RequestID: "request-1", Fence: fence}, 1) || !strings.HasPrefix(sent.IdempotencyKey, "op:") {
		t.Fatalf("the person's call = %+v", sent)
	}
}

// Decision 6 and §8.17: several operator calls are each admitted and settled against the tenant as
// the person's, never the agent's release budget, and their usage is stored on each entry.
func TestStudioOperatorToolCallsSettleTenantSpendPerAction(t *testing.T) {
	ctx := context.Background()
	fixture := newOperationFixture(t)
	fence := fixture.takeOver(t)
	call := domain.TurnActRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence, Kind: domain.OperatorTool,
		Tool: domain.ToolReference{ToolID: "shop.write", Version: "2"}, Arguments: []byte(`{"order":"o-1"}`)}
	for range 2 {
		result, err := fixture.service.Act(ctx, usageActor, call)
		if err != nil || result.Entry.Kind != domain.OperatorTool || result.Entry.Body.Tool.Status != domain.CallCompleted ||
			result.Entry.Usage.ToolCalls != 1 || result.Entry.Usage.CostMinorUnits != 40 || result.Entry.Usage.Currency != "USD" {
			t.Fatalf("tool act = %+v, %v", result, err)
		}
	}
	if len(fixture.agentReserved) != 0 {
		t.Fatalf("an operator call reserved the agent's budget: %+v", fixture.agentReserved)
	}
	if len(fixture.operatorHolds) != 2 || fixture.operatorHolds[0].Principal != usageActor.Principal || fixture.operatorHolds[0].Tokens != operatorAdmissionTokens {
		t.Fatalf("operator holds = %+v", fixture.operatorHolds)
	}
	acts := fixture.control.operation.acts
	if len(acts) != 2 || acts[0].Settlement == nil || acts[0].Settlement.Status != domain.CallCompleted || acts[0].Settlement.Resolution != domain.ResolvedByGateway ||
		acts[0].Reservation.ReservationID != "operator-1" || acts[1].Call.Key.CallID != "op:4:2" || fixture.scopes[0] == fixture.scopes[1] {
		t.Fatalf("acts = %+v, scopes %v", acts, fixture.scopes)
	}
}

// Decision 2: a person's call nobody can establish holds the turn again, audited turn_held.
func TestStudioOperatorUnknownCallHoldsTheTurn(t *testing.T) {
	ctx := context.Background()
	fixture := newOperationFixture(t)
	fence := fixture.takeOver(t)
	fixture.gateway = func(domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, fmt.Errorf("%w: timeout after send", domain.ErrEffectUnknown)
	}
	result, err := fixture.service.Act(ctx, usageActor, domain.TurnActRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence, Kind: domain.OperatorTool,
		Tool: domain.ToolReference{ToolID: "shop.write", Version: "2"}, Arguments: []byte(`{}`)})
	if err != nil || result.Entry.Body.Tool.Status != domain.CallUnknown || result.Turn.Status != domain.TurnHeld || fixture.control.operation.acts[0].Settlement != nil {
		t.Fatalf("an unknown operator call = %+v, %v", result, err)
	}
	if got := categories(fixture.audit.Request(7, "request-1")); !slices.Equal(got, []string{observability.AuditCategoryTurnTakenOver,
		observability.AuditCategoryTurnOperatorAction, observability.AuditCategoryTurnHeld}) {
		t.Fatalf("audit = %v", got)
	}
}

// Decision 10: a handback re-admits first; a refused re-admission leaves the turn operated, and a
// refused handback releases only the hold it newly took.
func TestStudioHandbackReadmitsFirst(t *testing.T) {
	ctx := context.Background()
	refused := newOperationFixture(t)
	fence := refused.takeOver(t)
	refused.budget.ReserveFunc = func(context.Context, domain.BudgetRequest) (domain.BudgetReservation, error) {
		return domain.BudgetReservation{}, fmt.Errorf("%w: window spent", domain.ErrBudgetExceeded)
	}
	request := domain.TurnHandbackRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence, Instruction: "use the refund tool"}
	if _, err := refused.service.HandBack(ctx, usageActor, request); !errors.Is(err, domain.ErrBudgetExceeded) ||
		len(refused.control.operation.handbacks) != 0 || refused.control.status["request-1"] != domain.TurnOperated {
		t.Fatalf("refused re-admission = %v, status %s", err, refused.control.status["request-1"])
	}
	open := newOperationFixture(t)
	fence = open.takeOver(t)
	open.control.operation.open = true
	if _, err := open.service.HandBack(ctx, usageActor, request); !errors.Is(err, domain.ErrConflict) || !slices.Equal(open.refunded, []string{"reservation-2"}) ||
		open.control.status["request-1"] != domain.TurnOperated {
		t.Fatalf("an open outcome = %v, released %v", err, open.refunded)
	}
	handed := newOperationFixture(t)
	fence = handed.takeOver(t)
	turn, err := handed.service.HandBack(ctx, usageActor, domain.TurnHandbackRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence, Instruction: "use the refund tool"})
	if err != nil || turn.Status != domain.TurnQueued || turn.Fence != fence+1 || len(handed.refunded) != 0 {
		t.Fatalf("handback = %+v, %v", turn, err)
	}
	if got := categories(handed.audit.Request(7, "request-1")); !slices.Equal(got, []string{observability.AuditCategoryTurnTakenOver,
		observability.AuditCategoryTurnOperatorAction, observability.AuditCategoryTurnHandedBack}) {
		t.Fatalf("audit = %v", got)
	}
}

// Decision 4: a finish completes the turn with the guarded reply, is refused while an outcome is
// open, settles the agent's held reservation, publishes the reply and the final frame, and is
// audited as the person's completion under the key the runtime's terminal record takes.
func TestStudioFinishCompletesAsTheOperatorsTurn(t *testing.T) {
	ctx := context.Background()
	open := newOperationFixture(t)
	fence := open.takeOver(t)
	open.control.operation.open = true
	request := domain.TurnFinishRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence, Reply: []byte("refund issued")}
	if _, err := open.service.Finish(ctx, usageActor, request); !errors.Is(err, domain.ErrConflict) || !slices.Equal(open.refunded, []string{"reservation-2"}) {
		t.Fatalf("finish with an open outcome = %v, released %v", err, open.refunded)
	}

	fixture := newOperationFixture(t)
	fence = fixture.takeOver(t)
	turn, err := fixture.service.Finish(ctx, usageActor, domain.TurnFinishRequest{AgentID: "writer-a", RequestID: "request-1", Fence: fence, Reply: []byte("refund issued")})
	if err != nil || turn.Status != domain.TurnCompleted || turn.Holder != usageActor.Principal {
		t.Fatalf("finish = %+v, %v", turn, err)
	}
	finish := fixture.control.operation.finishes[0]
	if finish.Reservation.ReservationID != "reservation-2" || string(finish.Frame.Payload) != "refund issued" || finish.Currency != "USD" ||
		finish.SubjectRef != "writer-a@v3" || finish.Attribution.Principal != (domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer-a"}) {
		t.Fatalf("finish command = %+v", finish)
	}
	if len(fixture.published) != 2 || fixture.published[0].Final || !fixture.published[1].Final || fixture.published[1].Sequence != fixture.published[0].Sequence+1 {
		t.Fatalf("frames = %+v", fixture.published)
	}
	records := fixture.audit.Request(7, "request-1")
	last := records[len(records)-1]
	terminal := domain.DecisionKeyFor(domain.WithDecisionScope(ctx, "turn/", "request-1"),
		domain.DecisionRecord{TenantID: 7, Category: observability.AuditCategoryTurnCompleted, Action: "turn", Resource: "writer-a"})
	if last.Category != observability.AuditCategoryTurnCompleted || last.Principal != usageActor.Principal || last.DecisionKey != terminal {
		t.Fatalf("completion record = %+v", last)
	}
	var payload map[string]any
	if err := json.Unmarshal(last.Payload, &payload); err != nil || payload["operator"] != true {
		t.Fatalf("the completion does not say a person authored it: %s", last.Payload)
	}
	if err := observability.VerifyTurnChain(append([]domain.DecisionRecord{{TenantID: 7, RequestID: "request-1", Category: observability.AuditCategoryTurnAdmitted, DecisionKey: "a"}}, records...)); err != nil {
		t.Fatalf("chain = %v", err)
	}
}
