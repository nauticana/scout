package confirmation

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nauticana/keel/approval"

	"github.com/nauticana/scout/domain"
)

var (
	maker   = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "41"}
	checker = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}
)

// checks refuses whoever is listed, at the moment of asking.
type checks struct {
	makerErr   error
	deciderErr map[string]error
}

func (c *checks) Maker(context.Context, domain.MCPConfirmation) error { return c.makerErr }

func (c *checks) Decider(_ context.Context, _ domain.MCPConfirmation, decider domain.PrincipalRef) error {
	return c.deciderErr[decider.ID]
}

type runner struct {
	calls  atomic.Int32
	result json.RawMessage
	err    error
	block  bool
}

func (r *runner) RunConfirmed(ctx context.Context, _ domain.MCPConfirmation) (json.RawMessage, error) {
	r.calls.Add(1)
	if r.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.result, r.err
}

type fixture struct {
	db       *confirmationDB
	store    *TableStore
	checks   *checks
	runner   *runner
	executor *Executor
	failures []error
}

func newFixture() *fixture {
	f := &fixture{db: newConfirmationDB(), checks: &checks{deciderErr: map[string]error{}}, runner: &runner{result: json.RawMessage(`{"published":true}`)}}
	f.store = &TableStore{DB: f.db}
	f.executor = &Executor{Store: f.store, Checker: f.checks, Runner: f.runner,
		OnFailure: func(_ domain.MCPConfirmationKey, err error) { f.failures = append(f.failures, err) }}
	return f
}

func draft(channel domain.MCPConfirmationChannel) domain.MCPConfirmationDraft {
	args := []byte(`{"page":"home"}`)
	return domain.MCPConfirmationDraft{TenantID: 7, Maker: maker, ClientRef: "client-a", Tool: "publish_page",
		Digest: ActionDigest("publish_page", args), Payload: args, Preview: json.RawMessage(`{"action":"Publish home"}`),
		Channel: channel, TTL: 10 * time.Minute}
}

func answer(id int64, action domain.MCPElicitationAction, approve bool) domain.MCPToolCall {
	return domain.MCPToolCall{Caller: domain.MCPCaller{TenantID: 7}, State: statePrefix + strconv.FormatInt(id, 10),
		Elicited: map[string]domain.MCPElicitationResult{elicitationID: {Action: action, Content: map[string]any{approveField: approve}}}}
}

func TestPrepareGuardsAnOpenActionPerMaker(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	first, created, err := f.store.Prepare(ctx, draft(domain.MCPConfirmationElicitation))
	if err != nil || !created || first.Status != domain.MCPConfirmationPending || first.Maker != maker {
		t.Fatalf("Prepare = %+v, %v, %v", first, created, err)
	}
	again, created, err := f.store.Prepare(ctx, draft(domain.MCPConfirmationElicitation))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("the maker's identical action must return the open one: %+v, %v, %v", again, created, err)
	}
	other := draft(domain.MCPConfirmationElicitation)
	other.Maker = checker
	if _, _, err := f.store.Prepare(ctx, other); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("another maker's identical action: want ErrConflict, got %v", err)
	}
	otherClient := draft(domain.MCPConfirmationElicitation)
	otherClient.ClientRef = "client-b"
	if _, _, err := f.store.Prepare(ctx, otherClient); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("another client's identical action: want ErrConflict, got %v", err)
	}
}

func TestPrepareRefusesAnIncompleteDraft(t *testing.T) {
	f := newFixture()
	for name, mutate := range map[string]func(*domain.MCPConfirmationDraft){
		"tenant":  func(d *domain.MCPConfirmationDraft) { d.TenantID = 0 },
		"maker":   func(d *domain.MCPConfirmationDraft) { d.Maker.ID = " " },
		"digest":  func(d *domain.MCPConfirmationDraft) { d.Digest = strings.ToUpper(d.Digest) },
		"payload": func(d *domain.MCPConfirmationDraft) { d.Payload = json.RawMessage(`{"a":`) },
		"preview": func(d *domain.MCPConfirmationDraft) { d.Preview = nil },
		"channel": func(d *domain.MCPConfirmationDraft) { d.Channel = "email" },
		"maker-checker channel": func(d *domain.MCPConfirmationDraft) {
			d.Channel, d.ApprovalRequired = domain.MCPConfirmationElicitation, true
		},
		"expiry": func(d *domain.MCPConfirmationDraft) { d.TTL = time.Millisecond },
		"oversize": func(d *domain.MCPConfirmationDraft) {
			d.Payload = json.RawMessage(`"` + strings.Repeat("x", MaxDocumentBytes) + `"`)
		},
	} {
		bad := draft(domain.MCPConfirmationInbox)
		mutate(&bad)
		if _, _, err := f.store.Prepare(context.Background(), bad); !errors.Is(err, domain.ErrValidation) && !errors.Is(err, domain.ErrPrincipalUnknown) {
			t.Fatalf("%s: want a validation error, got %v", name, err)
		}
	}
	if len(f.db.rows) != 0 {
		t.Fatalf("nothing may be stored: %d rows", len(f.db.rows))
	}
}

func TestPrepareQueuesTheInboxNoticeWithTheConfirmation(t *testing.T) {
	f := newFixture()
	f.store.NotifyThroughOutbox = true
	ctx := context.Background()
	inbox, _, err := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox)); err != nil {
		t.Fatal(err)
	}
	if len(f.db.events) != 1 || f.db.events[0] != Aggregate+"/"+PendingEvent || f.db.row(inbox.ID).eventID == 0 {
		t.Fatalf("one linked notice per created inbox confirmation: %v, %+v", f.db.events, f.db.row(inbox.ID))
	}
	elicit := draft(domain.MCPConfirmationElicitation)
	elicit.Tool = "other_tool"
	if _, _, err := f.store.Prepare(ctx, elicit); err != nil || len(f.db.events) != 1 {
		t.Fatalf("an elicitation confirmation queues no notice: %v, %v", f.db.events, err)
	}
}

func TestAnswerApprovesAndRunsTheStoredActionOnce(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	d := draft(domain.MCPConfirmationElicitation)
	prepared, _, err := f.store.Prepare(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	done, err := f.executor.Answer(ctx, answer(prepared.ID, domain.MCPElicitationAccept, true), maker, d.ClientRef, d.Tool, d.Digest)
	if err != nil || done.Status != domain.MCPConfirmationExecuted || string(done.Result) != `{"published":true}` || done.Decider != maker {
		t.Fatalf("Answer = %+v, %v", done, err)
	}
	replayed, err := f.executor.Answer(ctx, answer(prepared.ID, domain.MCPElicitationAccept, true), maker, d.ClientRef, d.Tool, d.Digest)
	if err != nil || replayed.Status != domain.MCPConfirmationExecuted || f.runner.calls.Load() != 1 {
		t.Fatalf("a replayed answer must not run again: %+v, %v, runs %d", replayed, err, f.runner.calls.Load())
	}
	if _, err := f.executor.Answer(ctx, answer(prepared.ID, domain.MCPElicitationAccept, true), checker, d.ClientRef, d.Tool, d.Digest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("someone else's state: want ErrNotFound, got %v", err)
	}
	if _, err := f.executor.Answer(ctx, answer(prepared.ID, domain.MCPElicitationAccept, true), maker, d.ClientRef, d.Tool, strings.Repeat("0", 64)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a changed action: want ErrNotFound, got %v", err)
	}
}

func TestAnswerDeclinesAnythingButAnAcceptedYes(t *testing.T) {
	for _, call := range []domain.MCPToolCall{
		answer(1, domain.MCPElicitationAccept, false),
		answer(1, domain.MCPElicitationCancel, true),
		answer(1, domain.MCPElicitationDecline, true),
	} {
		f := newFixture()
		d := draft(domain.MCPConfirmationElicitation)
		if _, _, err := f.store.Prepare(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		done, err := f.executor.Answer(context.Background(), call, maker, d.ClientRef, d.Tool, d.Digest)
		if err != nil || done.Status != domain.MCPConfirmationDeclined || f.runner.calls.Load() != 0 {
			t.Fatalf("%+v: want declined and not run, got %+v, %v", call.Elicited, done, err)
		}
	}
	if _, _, ok := Answered(domain.MCPToolCall{State: "other:1"}); ok {
		t.Fatal("a foreign state answers nothing")
	}
	if _, _, ok := Answered(domain.MCPToolCall{State: statePrefix + "1"}); ok {
		t.Fatal("a state without an answer answers nothing")
	}
}

func TestExecuteReauthorizesBeforeRunning(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	prepared, _, _ := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox))
	key := domain.MCPConfirmationKey{TenantID: 7, ID: prepared.ID}
	f.checks.deciderErr[checker.ID] = errors.New("not an administrator")
	if _, err := f.executor.Decide(ctx, key, checker, true, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an unauthorized decider: want ErrForbidden, got %v", err)
	}
	delete(f.checks.deciderErr, checker.ID)
	if _, err := f.executor.Decide(ctx, key, checker, true, "looks right"); err != nil {
		t.Fatal(err)
	}
	f.checks.makerErr = errors.New("left the workspace")
	ran, err := f.executor.Execute(ctx, key)
	if err != nil || ran.Status != domain.MCPConfirmationFailed || ran.Error != "Not run: the action is no longer authorized." {
		t.Fatalf("a maker who lost access: %+v, %v", ran, err)
	}
	if f.runner.calls.Load() != 0 || len(f.failures) != 1 || !strings.Contains(f.failures[0].Error(), "left the workspace") {
		t.Fatalf("not run, and the cause goes to OnFailure only: runs %d, %v", f.runner.calls.Load(), f.failures)
	}
}

func TestDecisionsDoNotRequireWorkerDependencies(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	prepared, _, _ := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox))
	executor := &Executor{Store: f.store, Checker: f.checks}
	stored, err := executor.Decide(ctx, domain.MCPConfirmationKey{TenantID: 7, ID: prepared.ID}, checker, false, "no")
	if err != nil || stored.Status != domain.MCPConfirmationDeclined {
		t.Fatalf("Decide = %+v, %v", stored, err)
	}
}

func TestRunApprovedRecordsUnknownAndReconciles(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	prepared, _, _ := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox))
	key := domain.MCPConfirmationKey{TenantID: 7, ID: prepared.ID}
	if _, err := f.executor.Decide(ctx, key, checker, true, ""); err != nil {
		t.Fatal(err)
	}
	f.runner.result, f.runner.err = nil, errors.Join(domain.ErrEffectUnknown, errors.New("vendor timed out"))
	if count, err := f.executor.RunApproved(ctx, 0); err != nil || count != 1 {
		t.Fatalf("RunApproved = %d, %v", count, err)
	}
	stored, _ := f.store.Get(ctx, key)
	if stored.Status != domain.MCPConfirmationUnknown || strings.Contains(stored.Error, "vendor") {
		t.Fatalf("an unobservable effect is unknown with a client-safe reason: %+v", stored)
	}
	if count, _ := f.executor.RunApproved(ctx, 0); count != 0 || f.runner.calls.Load() != 1 {
		t.Fatal("an unknown outcome is never run again")
	}
	if _, err := f.executor.Reconcile(ctx, key, checker, domain.MCPConfirmationExecuted, " "); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a reconciliation needs a note: %v", err)
	}
	reconciled, err := f.executor.Reconcile(ctx, key, checker, domain.MCPConfirmationExecuted, "the page is live")
	if err != nil || reconciled.Status != domain.MCPConfirmationExecuted || reconciled.Reconciler != checker {
		t.Fatalf("Reconcile = %+v, %v", reconciled, err)
	}
}

func TestALostClaimLeavesTheOutcomeToReconciliation(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	prepared, _, _ := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox))
	key := domain.MCPConfirmationKey{TenantID: 7, ID: prepared.ID}
	if _, err := f.executor.Decide(ctx, key, checker, true, ""); err != nil {
		t.Fatal(err)
	}
	f.executor.Lease = time.Second
	f.runner.block, f.db.renewErr = true, true
	ran, err := f.executor.Execute(ctx, key)
	if !errors.Is(err, domain.ErrEffectUnknown) || ran.Status != domain.MCPConfirmationExecuting {
		t.Fatalf("a lost claim writes nothing: %+v, %v", ran, err)
	}
	if fence, ok, _ := f.store.Claim(ctx, key, time.Second); ok || fence != 0 {
		t.Fatal("a running confirmation cannot be claimed again")
	}
	f.db.advance(2 * time.Second)
	if lapsed, err := f.store.MarkLapsed(ctx, 0); err != nil || lapsed != 1 {
		t.Fatalf("MarkLapsed = %d, %v", lapsed, err)
	}
	if stored, _ := f.store.Get(ctx, key); stored.Status != domain.MCPConfirmationUnknown {
		t.Fatalf("a lapsed claim is unknown: %+v", stored)
	}
	if err := f.store.Complete(ctx, key, 1, domain.MCPConfirmationExecuted, nil, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a stale fence completes nothing: %v", err)
	}
}

func TestAMakerCheckerConfirmationIsDecidedOnlyThroughKeel(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	d := draft(domain.MCPConfirmationInbox)
	d.ApprovalRequired = true
	prepared, _, _ := f.store.Prepare(ctx, d)
	key := domain.MCPConfirmationKey{TenantID: 7, ID: prepared.ID}
	if _, err := f.executor.Decide(ctx, key, checker, true, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("the setup window must not bypass maker-checker: want ErrForbidden, got %v", err)
	}
	if err := f.store.AttachApproval(ctx, key, 900); err != nil {
		t.Fatal(err)
	}
	if _, err := f.executor.Decide(ctx, key, checker, true, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a direct decision bypassing maker-checker: want ErrForbidden, got %v", err)
	}
	request := &approval.Request{ID: 901, PartnerID: 7, ObjectType: ApprovalObjectType, ObjectID: prepared.ID, Status: approval.StatusApproved, CheckerID: 42}
	f.db.catalogNil = true
	if err := f.store.DecideApprovalTx(ctx, f.db, request); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a transaction without a bound query service: want ErrNotReady, got %v", err)
	}
	f.db.catalogNil = false
	if err := f.store.DecideApprovalTx(ctx, f.db, request); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("another approval request: want ErrConflict, got %v", err)
	}
	request.ID = 900
	if err := f.store.DecideApprovalTx(ctx, f.db, request); err != nil {
		t.Fatal(err)
	}
	if stored, _ := f.store.Get(ctx, key); stored.Status != domain.MCPConfirmationApproved || stored.Decider != checker {
		t.Fatalf("keel's checker decides: %+v", stored)
	}
}

func TestInvalidRunnerResultIsUnknownNotSuccessOrFailure(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	prepared, _, _ := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox))
	key := domain.MCPConfirmationKey{TenantID: 7, ID: prepared.ID}
	if _, err := f.executor.Decide(ctx, key, checker, true, ""); err != nil {
		t.Fatal(err)
	}
	f.runner.result = json.RawMessage(`{"incomplete":`)
	stored, err := f.executor.Execute(ctx, key)
	if err != nil || stored.Status != domain.MCPConfirmationUnknown || len(stored.Result) != 0 {
		t.Fatalf("a success with an unstorable result is unknown: %+v, %v", stored, err)
	}
	if len(f.failures) != 1 || !errors.Is(f.failures[0], domain.ErrValidation) {
		t.Fatalf("the internal validation cause must be observable: %v", f.failures)
	}
	if _, _, err := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox)); err != nil {
		t.Fatal(err)
	}
	if count, _ := f.executor.RunApproved(ctx, 0); count != 0 || f.runner.calls.Load() != 1 {
		t.Fatal("the unknown action still blocks an identical one from running again")
	}
}

func TestExpiryClosesAPendingConfirmation(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	prepared, _, _ := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox))
	key := domain.MCPConfirmationKey{TenantID: 7, ID: prepared.ID}
	f.db.advance(10 * time.Minute)
	if stored, _ := f.store.Get(ctx, key); !stored.Expired {
		t.Fatal("a pending confirmation at its expiry is expired on the store clock")
	}
	if _, err := f.executor.Decide(ctx, key, checker, true, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("deciding an expired confirmation: want ErrConflict, got %v", err)
	}
	expired, err := f.store.ExpireDue(ctx, 0)
	if err != nil || len(expired) != 1 || expired[0].ID != prepared.ID || expired[0].Maker != maker {
		t.Fatalf("ExpireDue = %+v, %v", expired, err)
	}
	if _, err := f.store.Withdraw(ctx, key, maker); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("withdrawing an expired confirmation: want ErrConflict, got %v", err)
	}
}

func TestMakerCheckerCannotApprovePastTheConfirmationExpiry(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	d := draft(domain.MCPConfirmationInbox)
	d.ApprovalRequired = true
	prepared, _, _ := f.store.Prepare(ctx, d)
	key := domain.MCPConfirmationKey{TenantID: 7, ID: prepared.ID}
	if err := f.store.AttachApproval(ctx, key, 900); err != nil {
		t.Fatal(err)
	}
	f.db.advance(10 * time.Minute)
	request := &approval.Request{ID: 900, PartnerID: 7, ObjectType: ApprovalObjectType, ObjectID: prepared.ID,
		Status: approval.StatusApproved, CheckerID: 42}
	if err := f.store.DecideApprovalTx(ctx, f.db, request); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("an expired confirmation must reject keel's decision: %v", err)
	}
}

func TestWithdrawIsTheMakersOnly(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	prepared, _, _ := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox))
	key := domain.MCPConfirmationKey{TenantID: 7, ID: prepared.ID}
	if _, err := f.store.Withdraw(ctx, key, checker); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("someone else's confirmation: want ErrNotFound, got %v", err)
	}
	if _, err := f.store.Withdraw(ctx, domain.MCPConfirmationKey{TenantID: 8, ID: prepared.ID}, maker); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant: want ErrNotFound, got %v", err)
	}
	if _, err := f.store.Withdraw(ctx, key, maker); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox)); err != nil {
		t.Fatalf("a withdrawn action no longer blocks preparing it again: %v", err)
	}
}

func TestToolResultAsksOnlyTheMakerInTheirClient(t *testing.T) {
	pending := domain.MCPConfirmation{ID: 5, Maker: maker, ClientRef: "client-a", Tool: "publish_page",
		Channel: domain.MCPConfirmationElicitation, Status: domain.MCPConfirmationPending, Preview: json.RawMessage(`{"action":"Publish"}`)}
	asked := ToolResult(pending, maker, "client-a", "Publish home", "https://app.example/approvals?id=5")
	if asked.Elicit[elicitationID].Message != "Publish home" || asked.State != statePrefix+"5" {
		t.Fatalf("the maker is asked: %+v", asked)
	}
	for name, result := range map[string]domain.MCPToolResult{
		"another viewer": ToolResult(pending, checker, "client-a", "Publish home", ""),
		"another client": ToolResult(pending, maker, "client-b", "Publish home", ""),
		"no preview":     ToolResult(pending, maker, "client-a", " ", ""),
	} {
		if result.Elicit != nil || result.State != "" {
			t.Fatalf("%s must not be asked: %+v", name, result)
		}
	}
	data := asked.Data.(map[string]any)
	if data["approval_url"] == nil || data["status"] != "pending" || data["result"] != nil {
		t.Fatalf("pending data: %+v", data)
	}
	expired := pending
	expired.Expired = true
	if result := ToolResult(expired, maker, "client-a", "Publish home", "u"); result.Elicit != nil || result.Data.(map[string]any)["status"] != "expired" {
		t.Fatalf("an expired confirmation is reported, not asked: %+v", result)
	}
}
