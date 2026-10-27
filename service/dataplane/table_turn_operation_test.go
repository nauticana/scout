package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// opRow is one turn_operator_entry row.
type opRow []any

// opCall is one turn_call_outcome row as the operator statements see it.
type opCall struct {
	status domain.CallOutcomeStatus
	origin domain.CallOrigin
	fence  int64
	step   int64
}

// operatorState is the one conversation_turn row and its children the operator statements touch.
type operatorState struct {
	status               domain.TurnStatus
	fence                int64
	holderKind, holderID string
	holdUntil            time.Time
	holdCode             string
	openAgent            int
	earlierLive          bool
	committed            int
	openStep, latestStep int64
	journal              int
	published            int64
	entries              []opRow
	calls                map[string]*opCall
	requeued             int
	usageLines           [][]any
	response             string
	steps                [][]any
	journalRows          [][]any
}

func (state operatorState) clone() operatorState {
	copied := state
	copied.entries = slices.Clone(state.entries)
	copied.usageLines = slices.Clone(state.usageLines)
	copied.calls = map[string]*opCall{}
	for id, call := range state.calls {
		c := *call
		copied.calls[id] = &c
	}
	return copied
}

// operatorTable answers the operator statements with their SQL's semantics against one turn at a
// store clock the test moves; a rolled-back transaction leaves the state as it was.
type operatorTable struct {
	keelport.DatabaseRepository
	state     operatorState
	saved     *operatorState
	now       time.Time
	calls     []string
	commits   int
	rollbacks int
}

func newOperatorTable() *operatorTable {
	return &operatorTable{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), state: operatorState{
		status: domain.TurnHeld, fence: 3, holdCode: "C", committed: 1, openStep: 12, latestStep: 12, journal: 3, published: 1,
		calls: map[string]*opCall{},
	}}
}

func (table *operatorTable) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return table
}
func (table *operatorTable) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	saved := table.state.clone()
	table.saved = &saved
	return table, nil
}
func (*operatorTable) GenID() int64 { return 0 }
func (table *operatorTable) Commit(context.Context) error {
	table.commits++
	table.saved = nil
	return nil
}
func (table *operatorTable) Rollback(context.Context) error {
	if table.saved != nil {
		table.rollbacks++
		table.state, table.saved = *table.saved, nil
	}
	return nil
}

func (table *operatorTable) holds(args []any) bool {
	s := table.state
	return s.status == domain.TurnOperated && s.fence == args[2] && s.holderKind == args[3] && s.holderID == args[4] && s.holdUntil.After(table.now)
}

func (table *operatorTable) open(origin domain.CallOrigin) int {
	count := 0
	for _, call := range table.state.calls {
		if call.status.Open() && (origin == "" || call.origin == origin) {
			count++
		}
	}
	return count
}

func (table *operatorTable) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	table.calls = append(table.calls, name)
	s := &table.state
	result := &keelmodel.QueryResult{}
	seconds := func(value any) time.Duration { return time.Duration(value.(int64)) * time.Second }
	switch name {
	case qOperatorTakeover:
		if s.status == domain.TurnHeld && s.fence == args[5] && s.openAgent+table.open("") == 0 && !s.earlierLive {
			s.status, s.fence, s.holderKind, s.holderID, s.holdUntil = domain.TurnOperated, s.fence+1, args[0].(string), args[1].(string), table.now.Add(seconds(args[2]))
			result.Rows = [][]any{{s.fence, s.holdUntil}}
		}
	case qOperatorGuard:
		if table.holds(args[1:]) {
			s.holdUntil = table.now.Add(seconds(args[0]))
			result.Rows = [][]any{{int64(4), "conversation-1", s.holdUntil}}
		}
	case qOperatorState:
		result.Rows = [][]any{{string(s.status), s.fence, s.holderKind, s.holderID, s.status == domain.TurnOperated && !s.holdUntil.After(table.now), int64(4), "conversation-1"}}
	case qOperatorPosition:
		replies, last := int64(0), int64(0)
		for _, row := range s.entries {
			if row[4] == string(domain.OperatorReply) {
				replies++
			}
			last = max(last, int64(row[3].(int)))
		}
		result.Rows = [][]any{{int64(s.committed), s.openStep, int64(s.journal), s.latestStep, s.published, replies, last, int64(table.open(domain.CallFromOperator))}}
	case qOperatorIntend:
		if _, exists := s.calls[args[3].(string)]; !exists {
			s.calls[args[3].(string)] = &opCall{status: domain.CallInFlight, origin: domain.CallFromOperator, fence: args[7].(int64), step: args[2].(int64)}
			result.Rows = [][]any{{args[3]}}
		}
	case qOperatorEntryInsert:
		for _, row := range s.entries {
			if row[3] == args[3] {
				return result, nil
			}
		}
		s.entries = append(s.entries, opRow(args))
		result.Rows = [][]any{{table.now, int64(2)}}
	case qOperatorSettleCall:
		if call := s.calls[args[6].(string)]; call != nil && call.status == domain.CallInFlight && call.fence == args[7] {
			call.status = domain.CallOutcomeStatus(args[0].(string))
			result.Rows = [][]any{{args[6]}}
		}
	case qOperatorUnknownCall:
		if call := s.calls[args[3].(string)]; call != nil && call.status == domain.CallInFlight && call.fence == args[4] {
			call.status = domain.CallUnknown
			result.Rows = [][]any{{args[3]}}
		}
	case qOperatorHoldUnknown:
		if s.status == domain.TurnOperated && s.fence == args[2] {
			s.status, s.fence, s.holdCode, s.holderKind, s.holderID, s.holdUntil = domain.TurnHeld, s.fence+1, "U", "", "", time.Time{}
			result.Rows = [][]any{{s.fence}}
		}
	case qOperatorHandback:
		if table.holds(args) && s.openAgent+table.open("") == 0 {
			s.status, s.fence, s.holdCode, s.holderKind, s.holderID, s.holdUntil = domain.TurnQueued, s.fence+1, "", "", "", time.Time{}
			result.Rows = [][]any{{s.fence}}
		}
	case qQueueRequeue:
		s.requeued++
	case qOperatorFinish:
		if table.holds(args[2:]) && s.openAgent+table.open("") == 0 && !s.earlierLive {
			s.status, s.response, s.holdCode, s.holdUntil = domain.TurnCompleted, args[0].(string), "", time.Time{}
			result.Rows = [][]any{{int64(4)}}
		}
	case qOperatorEntries:
		for _, row := range s.entries {
			var step any = int64(0)
			if row[8] != nil {
				step = row[8]
			}
			currency, exponent := "", int64(0)
			if row[17] != nil {
				currency, exponent = row[17].(string), 2
			}
			result.Rows = append(result.Rows, []any{row[2], int64(row[3].(int)), row[4], row[5], row[6], int64(row[7].(int)), step, int64(row[9].(int)),
				row[10], row[11], row[12], row[13], int64(row[14].(int)), row[15], row[16], currency, exponent, table.now})
		}
	case qOperatorSteps:
		result.Rows = s.steps
	case qOperatorLapse:
		if s.status == domain.TurnOperated && !s.holdUntil.After(table.now) {
			kind, id := s.holderKind, s.holderID
			s.status, s.fence, s.holderKind, s.holderID, s.holdUntil = domain.TurnHeld, s.fence+1, "", "", time.Time{}
			result.Rows = [][]any{{int64(7), "request-1", s.fence, kind, id, "writer-a"}}
		}
	case qOperatorCallKnown:
		if _, found := s.calls[args[3].(string)]; found {
			result.Rows = [][]any{{int64(1)}}
		}
	case qLoopEntryList:
		result.Rows = s.journalRows
	case qSettleOperatorUsage:
		count, in, out, tools, cost, currencies := int64(0), int64(0), int64(0), int64(0), int64(0), map[string]bool{}
		currency := ""
		for _, row := range s.entries {
			if row[4] == string(domain.OperatorTool) {
				count, in, out, tools, cost = count+1, in+row[12].(int64), out+row[13].(int64), tools+int64(row[14].(int)), cost+row[15].(int64)
				currency = row[17].(string)
				currencies[currency] = true
			}
		}
		result.Rows = [][]any{{count, in, out, tools, int64(0), cost, int64(len(currencies)), currency}}
	case qSettleOperatorActor:
		for _, row := range slices.Backward(s.entries) {
			if row[4] == string(domain.OperatorTool) {
				result.Rows = [][]any{{row[5], row[6]}}
				break
			}
		}
	case qLedgerInsertUsageEvent:
		s.usageLines = append(s.usageLines, args)
		result.Rows = [][]any{{int64(1)}}
	case qLedgerUsageSettled:
	default:
		return nil, fmt.Errorf("operator table: unexpected statement %s", name)
	}
	return result, nil
}

var operatorLease = domain.OperatorLease{TenantID: 7, RequestID: "request-1", Fence: 4, Holder: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}
var operatorReservation = domain.BudgetReservation{TenantID: 7, ReservationID: "operator-1", RequestID: "request-1", Currency: "USD"}

func operatorControl(table *operatorTable, settler *settlerFake) *TableTurnControl {
	return &TableTurnControl{
		DB: table, Objects: newObjectStateStore(&fake.ObjectStorage{}),
		Settlement: TurnSettlementWriter{Budget: settler, UsageCategory: "model_output", OperatorUsageCategory: "operator"},
	}
}

func takenOver(t *testing.T, table *operatorTable, control *TableTurnControl) domain.OperatorHold {
	t.Helper()
	hold, err := control.TakeOver(context.Background(), domain.TurnTakeover{TenantID: 7, RequestID: "request-1", Fence: 3, Holder: operatorLease.Holder})
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	return hold
}

// §2 and §4: a takeover moves a turn held at the stated fence to operated, fence +1, under a
// store-clock lease of the configured length, and is refused for a stale fence, an open outcome,
// or an earlier live turn of the conversation, all in the one conditional UPDATE.
func TestTableTakeoverGatesOnTheHeldFenceAndOpenOutcomes(t *testing.T) {
	ctx := context.Background()
	takeover := domain.TurnTakeover{TenantID: 7, RequestID: "request-1", Fence: 3, Holder: operatorLease.Holder}
	for name, prepare := range map[string]func(*operatorTable){
		"stale fence":  func(table *operatorTable) { table.state.fence = 4 },
		"open outcome": func(table *operatorTable) { table.state.openAgent = 1 },
		"earlier turn": func(table *operatorTable) { table.state.earlierLive = true },
		"not held":     func(table *operatorTable) { table.state.status = domain.TurnQueued },
	} {
		table := newOperatorTable()
		prepare(table)
		if _, err := operatorControl(table, &settlerFake{}).TakeOver(ctx, takeover); !errors.Is(err, domain.ErrConflict) || table.state.status == domain.TurnOperated {
			t.Fatalf("%s: takeover = %v, status %s", name, err, table.state.status)
		}
	}
	table := newOperatorTable()
	control := operatorControl(table, &settlerFake{})
	control.Operation.Lease = 2 * time.Minute
	hold := takenOver(t, table, control)
	if hold.Lease != operatorLease || !hold.HoldUntil.Equal(table.now.Add(2*time.Minute)) || table.state.holderID != "42" || table.state.holdCode != "C" {
		t.Fatalf("hold = %+v, state %+v", hold, table.state)
	}
	if _, err := control.TakeOver(ctx, domain.TurnTakeover{TenantID: 7, RequestID: "request-1", Fence: 3, Holder: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "a"}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an agent took a turn over: %v", err)
	}
	sql := operatorQueries[qOperatorTakeover]
	for _, part := range []string{"status_code = " + sqlTurnHeld + " AND turn.fence = ?", "fence = turn.fence + 1", "CURRENT_TIMESTAMP + make_interval(secs => ?)",
		"o.status_code IN (" + sqlCallOpen + ")", "earlier.turn_no < turn.turn_no AND earlier.status_code IN (" + sqlTurnLive + ")"} {
		if !strings.Contains(sql, part) {
			t.Errorf("takeover lacks %q", part)
		}
	}
}

// §8.7: every act renews the lease by the store clock; once the store clock passes it the act is
// refused as fence lost, the sweeper holds the turn again under a new fence, and the person's
// later act is refused too.
func TestTableOperatorLeaseLapsesByTheStoreClock(t *testing.T) {
	ctx := context.Background()
	table := newOperatorTable()
	control := operatorControl(table, &settlerFake{})
	takenOver(t, table, control)
	table.now = table.now.Add(10 * time.Minute)
	acted, err := control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorNote, Body: domain.OperatorEntryBody{Text: "check the refund"}})
	if err != nil || acted.Entry.EntryNo != 1 || acted.Entry.Position != (domain.OperatorPosition{StepNo: 2, ExecutionStepID: 12, AfterEntryNo: 3}) {
		t.Fatalf("note = %+v, %v", acted, err)
	}
	if !table.state.holdUntil.Equal(table.now.Add(DefaultTakeoverLease)) {
		t.Fatalf("the act did not renew the lease: %s", table.state.holdUntil)
	}
	table.now = table.now.Add(DefaultTakeoverLease)
	if _, err = control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorNote, Body: domain.OperatorEntryBody{Text: "late"}}); !errors.Is(err, domain.ErrFenceLost) || !strings.Contains(err.Error(), "lapsed") {
		t.Fatalf("an act at the lease's end = %v, want a lapse", err)
	}
	lapsed, err := control.LapseHolds(ctx, 10)
	if err != nil || len(lapsed) != 1 || lapsed[0].Fence != 5 || lapsed[0].Holder != operatorLease.Holder || lapsed[0].AgentID != "writer-a" ||
		table.state.status != domain.TurnHeld || table.state.holderID != "" || table.state.holdCode != "C" {
		t.Fatalf("lapse = %+v, %v, state %+v", lapsed, err, table.state)
	}
	if _, err = control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorNote, Body: domain.OperatorEntryBody{Text: "stale"}}); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("an act after the sweep = %v", err)
	}
	if len(table.state.entries) != 1 {
		t.Fatalf("entries = %d", len(table.state.entries))
	}
	if _, err = control.LapseHolds(ctx, 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a zero batch = %v", err)
	}
	sql := operatorQueries[qOperatorLapse]
	for _, part := range []string{"hold_until <= CURRENT_TIMESTAMP", "LIMIT ?", "FOR UPDATE SKIP LOCKED", "fence = turn.fence + 1", "status_code = " + sqlTurnHeld} {
		if !strings.Contains(sql, part) {
			t.Errorf("lapse lacks %q", part)
		}
	}
	if guard := operatorQueries[qOperatorGuard]; !strings.Contains(guard, "turn.hold_until > CURRENT_TIMESTAMP") || !strings.Contains(guard, "turn.holder_id = ?") {
		t.Errorf("the holder's guard does not check a live lease: %s", guard)
	}
}

// §5: a reply takes the next frame sequence: one per step published, one per reply before it.
func TestTableOperatorReplyTakesTheNextFrameSequence(t *testing.T) {
	ctx := context.Background()
	table := newOperatorTable()
	control := operatorControl(table, &settlerFake{})
	takenOver(t, table, control)
	for want := int64(1); want <= 2; want++ {
		acted, err := control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorReply,
			Body: domain.OperatorEntryBody{Frame: &domain.TurnReply{TenantID: 7, RequestID: "request-1", Payload: []byte("done"), Final: true}}})
		if err != nil || acted.Entry.Body.Frame.Sequence != want || acted.Entry.Body.Frame.Fence != 4 || acted.Entry.Body.Frame.Final {
			t.Fatalf("reply %d = %+v, %v", want, acted.Entry.Body.Frame, err)
		}
	}
	if _, err := control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorReply, Body: domain.OperatorEntryBody{Text: "no frame"}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a reply without its frame = %v", err)
	}
	control.Operation.EntryMaxBytes = 32
	if _, err := control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorNote, Body: domain.OperatorEntryBody{Text: strings.Repeat("x", 64)}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an entry above agent_operator_entry_max_bytes = %v", err)
	}
	control.Operation.EntryMaxBytes, control.Operation.MaxEntries = 0, 2
	if _, err := control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorNote, Body: domain.OperatorEntryBody{Text: "third"}}); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("an entry beyond agent_operator_max_entries = %v", err)
	}
}

// §5: an operator call is recorded in flight, origin O, under the next entry number with an
// op: key; its gateway answer settles it with its entry in one transaction; while it is in flight
// no other act is admitted; and a call nobody can establish holds the turn under a new fence.
func TestTableOperatorCallSettlesWithItsEntry(t *testing.T) {
	ctx := context.Background()
	table := newOperatorTable()
	control := operatorControl(table, &settlerFake{})
	takenOver(t, table, control)
	tool := domain.ToolReference{ToolID: "shop.refund", Version: "2"}
	intent, err := control.IntendCall(ctx, operatorLease, tool, time.Minute)
	if err != nil || intent.Key.CallID != "op:4:1" || intent.Key.ExecutionStepID != 12 || intent.EntryNo != 1 ||
		intent.IdempotencyKey != operatorIdempotencyKey(operatorLease, 1) || len(intent.IdempotencyKey) != len("op:")+64 {
		t.Fatalf("intent = %+v, %v", intent, err)
	}
	if call := table.state.calls["op:4:1"]; call == nil || call.origin != domain.CallFromOperator || call.status != domain.CallInFlight || call.fence != 4 {
		t.Fatalf("outcome row = %+v", call)
	}
	if _, err = control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorNote, Body: domain.OperatorEntryBody{Text: "meanwhile"}}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a note while an operator call is in flight = %v", err)
	}
	usage := domain.Usage{ToolCalls: 1, SearchQueries: 2, CostMinorUnits: 40, Currency: "USD"}
	settlement := &domain.CallSettlement{Key: intent.Key, Status: domain.CallCompleted, Resolution: domain.ResolvedByGateway, Record: domain.CallRecord{Output: []byte(`{}`)}}
	use := &domain.OperatorToolUse{CallID: intent.Key.CallID, ToolID: tool.ToolID, Version: tool.Version, Status: domain.CallCompleted}
	settler := control.Settlement.Budget.(*settlerFake)
	settler.err = errors.New("budget store unavailable")
	if _, err = control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorTool, Body: domain.OperatorEntryBody{Tool: use}, Call: &intent, Settlement: settlement, Reservation: operatorReservation, Usage: usage}); err == nil || len(table.state.entries) != 0 || table.state.calls["op:4:1"].status != domain.CallInFlight {
		t.Fatalf("failed spend settlement = %v, entries %d, call %s", err, len(table.state.entries), table.state.calls["op:4:1"].status)
	}
	settler.err = nil
	wrong := *use
	wrong.CallID = "another-call"
	if _, err = control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorTool, Body: domain.OperatorEntryBody{Tool: &wrong}, Call: &intent, Settlement: settlement, Reservation: operatorReservation, Usage: usage}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an entry for another intent = %v", err)
	}
	acted, err := control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorTool, Body: domain.OperatorEntryBody{Tool: use}, Call: &intent, Settlement: settlement, Reservation: operatorReservation, Usage: usage})
	if err != nil || acted.Entry.EntryNo != 1 || acted.HeldFence != 0 || table.state.calls["op:4:1"].status != domain.CallCompleted || acted.Entry.Exponent != 2 {
		t.Fatalf("settled call = %+v, %v", acted, err)
	}
	if row := table.state.entries[0]; row[4] != "T" || row[15] != int64(40) || row[16] != int64(2) || row[17] != "USD" || row[14] != 1 {
		t.Fatalf("entry row = %v", row)
	}
	// A call nobody can establish holds the turn again.
	second, err := control.IntendCall(ctx, operatorLease, tool, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	use = &domain.OperatorToolUse{CallID: second.Key.CallID, ToolID: tool.ToolID, Version: tool.Version, Status: domain.CallUnknown}
	acted, err = control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorTool, Body: domain.OperatorEntryBody{Tool: use}, Call: &second, Reservation: operatorReservation, Usage: usage})
	if err != nil || acted.HeldFence != 5 || table.state.status != domain.TurnHeld || table.state.holdCode != "U" || table.state.calls["op:4:2"].status != domain.CallUnknown {
		t.Fatalf("unknown call = %+v, %v, state %+v", acted, err, table.state)
	}
	// A turn that ran no step has none to file a call under.
	empty := newOperatorTable()
	empty.state.openStep, empty.state.latestStep, empty.state.committed, empty.state.journal = 0, 0, 0, 0
	emptyControl := operatorControl(empty, &settlerFake{})
	takenOver(t, empty, emptyControl)
	if _, err = emptyControl.IntendCall(ctx, operatorLease, tool, time.Minute); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a call on a turn without a step = %v", err)
	}
}

// §5: a withdrawal names a call the step in progress proposed and never sent.
func TestTableOperatorWithdrawalNamesAProposedUnsentCall(t *testing.T) {
	ctx := context.Background()
	table := newOperatorTable()
	control := operatorControl(table, &settlerFake{})
	objects := control.Objects
	journal := []domain.LoopEntry{
		{EntryNo: 1, Kind: domain.LoopEntryModel, Model: &domain.ModelResult{ToolCalls: []domain.ModelToolCall{{CallID: "c1", Name: "a"}, {CallID: "c2", Name: "b"}, {CallID: "c3", Name: "c"}}}},
		{EntryNo: 2, Kind: domain.LoopEntryObservation, Observation: &domain.ModelToolObservation{CallID: "c1"}},
	}
	for _, entry := range journal {
		payload, _ := json.Marshal(entry)
		ref, err := objects.Dehydrate(ctx, fmt.Sprintf("loop/%d", entry.EntryNo), payload)
		if err != nil {
			t.Fatal(err)
		}
		table.state.journalRows = append(table.state.journalRows, []any{int64(entry.EntryNo), ref.URI, ref.Digest})
	}
	table.state.calls["c3"] = &opCall{status: domain.CallCompleted, origin: domain.CallFromAgent, fence: 2, step: 12}
	takenOver(t, table, control)
	withdraw := func(callID string) error {
		_, err := control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorWithdraw, Body: domain.OperatorEntryBody{CallID: callID}})
		return err
	}
	for callID, want := range map[string]error{"c1": domain.ErrConflict, "c3": domain.ErrConflict, "c9": domain.ErrNotFound} {
		if err := withdraw(callID); !errors.Is(err, want) {
			t.Fatalf("withdraw %s = %v, want %v", callID, err, want)
		}
	}
	if err := withdraw("c2"); err != nil {
		t.Fatalf("withdraw c2 = %v", err)
	}
	if err := withdraw("c2"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a second withdrawal = %v", err)
	}
	entries, err := control.OperatorEntries(ctx, 7, "request-1")
	if err != nil || len(entries) != 1 || entries[0].Body.CallID != "c2" || entries[0].Kind != domain.OperatorWithdraw {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
}

// Decision 10 and §5: a handback appends its instruction and requeues the turn in one transaction;
// an open outcome refuses it and leaves neither the instruction nor a transition behind.
func TestTableHandbackIsAtomicWithItsInstruction(t *testing.T) {
	ctx := context.Background()
	table := newOperatorTable()
	control := operatorControl(table, &settlerFake{})
	takenOver(t, table, control)
	table.state.openAgent = 1
	if _, err := control.HandBack(ctx, operatorLease, "retry"); !errors.Is(err, domain.ErrConflict) || len(table.state.entries) != 0 ||
		table.state.status != domain.TurnOperated || table.state.requeued != 0 || table.rollbacks == 0 {
		t.Fatalf("refused handback = %v, state %+v", err, table.state)
	}
	table.state.openAgent = 0
	handed, err := control.HandBack(ctx, operatorLease, "  retry the refund  ")
	if err != nil || handed.Fence != 5 || handed.Instruction == nil || handed.Instruction.Body.Text != "retry the refund" ||
		table.state.status != domain.TurnQueued || table.state.requeued != 1 || table.state.holderID != "" {
		t.Fatalf("handback = %+v, %v, state %+v", handed, err, table.state)
	}
	if !strings.Contains(operatorQueries[qOperatorHandback], "o.status_code IN ("+sqlCallOpen+")") || !strings.Contains(operatorQueries[qOperatorHandback], "fence = turn.fence + 1") {
		t.Errorf("handback is not gated: %s", operatorQueries[qOperatorHandback])
	}
}

// Decision 4, decision 6, and §8.17: a finish completes the turn with the reply as the person's,
// settles the agent's held reservation at what the agent spent, and writes the agent's line and
// one operator line aggregating every operator call, in one transaction; an open outcome leaves
// everything as it was.
func TestTableFinishSettlesTheAgentAndTheOperatorsInOneUnit(t *testing.T) {
	ctx := context.Background()
	table := newOperatorTable()
	settler := &settlerFake{}
	control := operatorControl(table, settler)
	objects := control.Objects
	committed, _ := json.Marshal(domain.StepResult{Usage: domain.Usage{InputTokens: 10, OutputTokens: 5, CostMinorUnits: 7, Currency: "USD"}})
	ref, err := objects.Dehydrate(ctx, "step-result", committed)
	if err != nil {
		t.Fatal(err)
	}
	table.state.steps = [][]any{{int64(11), string(domain.StepCommitted), ref.URI, ref.Digest}}
	takenOver(t, table, control)
	tool := domain.ToolReference{ToolID: "shop.refund", Version: "2"}
	for _, cost := range []int64{40, 60} {
		intent, err := control.IntendCall(ctx, operatorLease, tool, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		_, err = control.Act(ctx, domain.OperatorAct{Lease: operatorLease, Kind: domain.OperatorTool, Call: &intent,
			Body:        domain.OperatorEntryBody{Tool: &domain.OperatorToolUse{CallID: intent.Key.CallID, ToolID: tool.ToolID, Version: tool.Version, Status: domain.CallCompleted}},
			Settlement:  &domain.CallSettlement{Key: intent.Key, Status: domain.CallCompleted, Resolution: domain.ResolvedByGateway},
			Reservation: operatorReservation,
			Usage:       domain.Usage{ToolCalls: 1, CostMinorUnits: cost, Currency: "USD"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	finish := domain.OperatorFinish{
		Lease: operatorLease, Frame: domain.TurnReply{TenantID: 7, RequestID: "request-1", Payload: []byte(`{"answer":"refunded"}`)},
		Reservation: domain.BudgetReservation{TenantID: 7, ReservationID: "res-1", RequestID: "request-1", Currency: "USD"}, Currency: "USD",
		SubjectRef: "writer-a@v3", Attribution: domain.UsageAttribution{Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer-a"},
			OnBehalfOf: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "end-user-5"}},
	}
	table.state.openAgent = 1
	if _, err = control.Finish(ctx, finish); !errors.Is(err, domain.ErrConflict) || len(table.state.usageLines) != 0 || len(table.state.entries) != 2 || table.state.status != domain.TurnOperated {
		t.Fatalf("finish with an open outcome = %v, state %+v", err, table.state)
	}
	table.state.openAgent = 0
	settler.calls = nil
	entry, err := control.Finish(ctx, finish)
	if err != nil || entry.Kind != domain.OperatorReply || entry.Body.Frame.Sequence != 1 || table.state.status != domain.TurnCompleted || table.state.holderID != "42" {
		t.Fatalf("finish = %+v, %v, state %+v", entry, err, table.state)
	}
	if !slices.Equal(settler.calls, []string{"res-1/7/false"}) {
		t.Fatalf("the agent's reservation settled as %v, want at its own spend", settler.calls)
	}
	if len(table.state.usageLines) != 2 {
		t.Fatalf("usage lines = %v", table.state.usageLines)
	}
	operator, agent := table.state.usageLines[0], table.state.usageLines[1]
	if operator[3] != "operator" || operator[5] != "human" || operator[6] != "42" || operator[8] != "human" || operator[9] != "end-user-5" ||
		operator[13] != 2 || operator[15] != int64(100) || operator[16] != "USD" {
		t.Fatalf("operator line = %v", operator)
	}
	if agent[3] != "model_output" || agent[5] != "agent" || agent[15] != int64(7) {
		t.Fatalf("agent line = %v", agent)
	}
}

// Every new statement stays inside its tenant, and only the sweeper locks rows it skips.
func TestOperatorStatementsStayInTheirTenant(t *testing.T) {
	statements := map[string]string{}
	maps.Copy(statements, operatorQueries)
	maps.Copy(statements, turnSettlementQueries)
	for name, sql := range statements {
		if name == qOperatorLapse {
			if !strings.Contains(sql, "turn.partner_id = lapsed.partner_id") || !strings.Contains(sql, "c.partner_id = turn.partner_id") {
				t.Errorf("the sweep crosses tenants inside a row:\n%s", sql)
			}
			continue
		}
		if !strings.Contains(sql, "partner_id = ?") && !strings.Contains(sql, "(partner_id, request_id") {
			t.Errorf("%s is not scoped to a tenant:\n%s", name, sql)
		}
		if strings.Contains(sql, "FOR UPDATE") || strings.Contains(sql, "advisory") {
			t.Errorf("%s takes an explicit lock:\n%s", name, sql)
		}
	}
	position := operatorQueries[qOperatorPosition]
	for _, child := range []string{"s.partner_id = t.partner_id", "e.partner_id = t.partner_id", "y.partner_id = t.partner_id", "x.partner_id = t.partner_id", "o.partner_id = t.partner_id", "snap.partner_id = t.partner_id"} {
		if !strings.Contains(position, child) {
			t.Errorf("the position read joins outside the tenant: lacks %s", child)
		}
	}
}
