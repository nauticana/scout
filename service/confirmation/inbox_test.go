package confirmation

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/nauticana/keel/approval"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// viewer sees tenants and refuses decisions or reconciliations when told to.
type viewer struct {
	tenants       []int64
	deciderErr    error
	reconcilerErr error
}

func (v *viewer) Tenants() []int64 { return v.tenants }
func (v *viewer) Decider(context.Context, domain.MCPConfirmation) error {
	return v.deciderErr
}
func (v *viewer) Reconciler(context.Context, domain.MCPConfirmation) error {
	return v.reconcilerErr
}

// authorizer counts how often a viewer is resolved.
type authorizer struct {
	viewer *viewer
	err    error
	calls  int
}

func (a *authorizer) Viewer(context.Context, domain.PrincipalRef) (contract.MCPInboxViewer, error) {
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	return a.viewer, nil
}

type keelCall struct {
	partnerID, id, actorID int64
	approved               bool
	note                   string
}

// makerChecker plays keel's approval service: a decision runs OnDecided in the store's transaction.
type makerChecker struct {
	inbox                  *Inbox
	db                     *confirmationDB
	decideErr, withdrawErr error
	decisions, withdrawals []keelCall
}

func (m *makerChecker) Decide(ctx context.Context, partnerID, id, checkerID int64, approved bool, note string) (*approval.Request, error) {
	m.decisions = append(m.decisions, keelCall{partnerID, id, checkerID, approved, note})
	if m.decideErr != nil {
		return nil, m.decideErr
	}
	status := approval.StatusRejected
	if approved {
		status = approval.StatusApproved
	}
	var objectID int64
	for _, row := range m.db.rows {
		if row.tenant == partnerID && row.approvalID == id {
			objectID = row.id
		}
	}
	request := &approval.Request{ID: id, PartnerID: partnerID, ObjectType: ApprovalObjectType, ObjectID: objectID,
		Status: status, CheckerID: checkerID, DecisionNote: note}
	return request, m.inbox.OnDecided(ctx, m.db, request)
}

func (m *makerChecker) Withdraw(_ context.Context, partnerID, id, makerID int64, note string) (*approval.Request, error) {
	m.withdrawals = append(m.withdrawals, keelCall{partnerID, id, makerID, false, note})
	return &approval.Request{ID: id, PartnerID: partnerID}, m.withdrawErr
}

type inboxFixture struct {
	*fixture
	auth  *authorizer
	keel  *makerChecker
	inbox *Inbox
}

func newInboxFixture() *inboxFixture {
	f := &inboxFixture{fixture: newFixture(), auth: &authorizer{viewer: &viewer{tenants: []int64{7}}}}
	f.inbox = &Inbox{Store: f.store, Executor: f.executor, Authorizer: f.auth}
	f.keel = &makerChecker{inbox: f.inbox, db: f.db}
	f.inbox.Approvals = f.keel
	return f
}

func (f *inboxFixture) prepare(t *testing.T, tenantID int64, by domain.PrincipalRef, approvalID int64) domain.MCPConfirmationKey {
	t.Helper()
	d := draft(domain.MCPConfirmationInbox)
	d.TenantID, d.Maker, d.ApprovalRequired = tenantID, by, approvalID > 0
	d.Tool = "tool_" + strconv.FormatInt(f.db.next+1, 10)
	d.Digest = ActionDigest(d.Tool, d.Payload)
	prepared, created, err := f.store.Prepare(context.Background(), d)
	if err != nil || !created {
		t.Fatalf("Prepare = %v, %v", created, err)
	}
	key := domain.MCPConfirmationKey{TenantID: tenantID, ID: prepared.ID}
	if approvalID > 0 {
		if err := f.store.AttachApproval(context.Background(), key, approvalID); err != nil {
			t.Fatal(err)
		}
	}
	f.db.advance(time.Second)
	return key
}

func TestInboxListsViewedTenantsAndThePersonsOwn(t *testing.T) {
	f := newInboxFixture()
	ctx := context.Background()
	f.auth.viewer.tenants = []int64{7, 8}
	ownHere := f.prepare(t, 7, maker, 0)
	othersHere := f.prepare(t, 8, checker, 0)
	ownElsewhere := f.prepare(t, 9, maker, 0)
	f.prepare(t, 10, checker, 0)

	entries, err := f.inbox.List(ctx, maker, f.db.now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if f.auth.calls != 1 {
		t.Fatalf("the viewer is resolved once per list, got %d", f.auth.calls)
	}
	var ids []int64
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	if !slices.Equal(ids, []int64{ownElsewhere.ID, othersHere.ID, ownHere.ID}) {
		t.Fatalf("listed %v, want viewed tenants and the person's own, newest first", ids)
	}
	args := f.db.inboxArgs[0]
	if !slices.Equal(args[0].([]int64), []int64{7, 8}) || args[1] != string(maker.Kind) || args[2] != maker.ID || args[4] != DefaultBatch {
		t.Fatalf("inbox query args %v", args)
	}
	if !entries[0].CanWithdraw || !entries[0].CanDecide || entries[1].CanWithdraw || !entries[1].CanDecide || entries[0].CanReconcile {
		t.Fatalf("permissions %+v", entries)
	}

	f.auth.viewer.tenants = nil
	entries, err = f.inbox.List(ctx, maker, f.db.now, 1000000)
	if err != nil || len(entries) != 2 || entries[0].ID != ownElsewhere.ID || entries[1].ID != ownHere.ID {
		t.Fatalf("no tenants still lists the person's own: %+v, %v", entries, err)
	}
	if limit := f.db.inboxArgs[1][4]; limit != DefaultMaxBatch {
		t.Fatalf("limit %v is not capped", limit)
	}
	if _, err = f.inbox.List(ctx, maker, f.db.now, -1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative limit: want ErrValidation, got %v", err)
	}
	if _, err = f.store.Inbox(ctx, []int64{0}, maker, f.db.now, 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a zero tenant: want ErrValidation, got %v", err)
	}
}

func TestInboxRefusesWithoutAPersonOrScope(t *testing.T) {
	f := newInboxFixture()
	ctx := context.Background()
	key := f.prepare(t, 7, maker, 0)
	nobody := domain.PrincipalRef{}
	if _, err := f.inbox.List(ctx, nobody, time.Time{}, 0); !errors.Is(err, domain.ErrPrincipalUnknown) {
		t.Fatalf("List by nobody: %v", err)
	}
	if _, err := f.inbox.Decide(ctx, nobody, key, true, ""); !errors.Is(err, domain.ErrPrincipalUnknown) {
		t.Fatalf("Decide by nobody: %v", err)
	}
	if err := f.inbox.Withdraw(ctx, nobody, key); !errors.Is(err, domain.ErrPrincipalUnknown) {
		t.Fatalf("Withdraw by nobody: %v", err)
	}
	if _, err := f.inbox.Reconcile(ctx, nobody, key, domain.MCPConfirmationExecuted, "seen"); !errors.Is(err, domain.ErrPrincipalUnknown) {
		t.Fatalf("Reconcile by nobody: %v", err)
	}
	if f.auth.calls != 0 {
		t.Fatalf("nobody resolved a viewer %d times", f.auth.calls)
	}

	f.auth.err = errors.New("directory down")
	if _, err := f.inbox.List(ctx, checker, time.Time{}, 0); !errors.Is(err, f.auth.err) {
		t.Fatalf("a viewer error must propagate: %v", err)
	}
	f.auth.err = nil
	if _, err := (&Inbox{Store: f.store}).List(ctx, checker, time.Time{}, 0); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no authorizer: want ErrNotReady, got %v", err)
	}

	f.auth.viewer.tenants = []int64{8}
	if _, err := f.inbox.Decide(ctx, checker, key, true, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a confirmation outside the view: want ErrNotFound, got %v", err)
	}
	f.auth.viewer.tenants = []int64{7}
	f.auth.viewer.deciderErr = errors.New("not an approver")
	if _, err := f.inbox.Decide(ctx, checker, key, true, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a refused decider: want ErrForbidden, got %v", err)
	}
	if entries, _ := f.inbox.List(ctx, checker, time.Time{}, 0); len(entries) != 1 || entries[0].CanDecide {
		t.Fatalf("a refused decider cannot decide: %+v", entries)
	}
}

func TestInboxDecidesDirectlyOrThroughKeel(t *testing.T) {
	f := newInboxFixture()
	ctx := context.Background()
	direct := f.prepare(t, 7, maker, 0)
	decided, err := f.inbox.Decide(ctx, checker, direct, true, "ok")
	if err != nil || decided.Status != domain.MCPConfirmationApproved || decided.Decider != checker || len(f.keel.decisions) != 0 {
		t.Fatalf("direct Decide = %+v, %v, keel %v", decided, err, f.keel.decisions)
	}

	keyed := f.prepare(t, 7, maker, 900)
	if _, err = f.inbox.Decide(ctx, domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "a-1"}, keyed, true, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a non-human checker: want ErrValidation, got %v", err)
	}
	decided, err = f.inbox.Decide(ctx, checker, keyed, false, "no")
	if err != nil || decided.Status != domain.MCPConfirmationDeclined || decided.Decider != checker {
		t.Fatalf("keel Decide = %+v, %v", decided, err)
	}
	if want := (keelCall{7, 900, 42, false, "no"}); len(f.keel.decisions) != 1 || f.keel.decisions[0] != want {
		t.Fatalf("keel saw %+v", f.keel.decisions)
	}

	d := draft(domain.MCPConfirmationInbox)
	d.TenantID, d.ApprovalRequired, d.Tool = 7, true, "other_tool"
	unattached, _, _ := f.store.Prepare(ctx, d)
	if _, err = f.inbox.Decide(ctx, checker, domain.MCPConfirmationKey{TenantID: 7, ID: unattached.ID}, true, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("no approval request yet: want ErrConflict, got %v", err)
	}
}

func TestInboxMapsKeelErrors(t *testing.T) {
	for keelErr, want := range map[error]error{
		approval.ErrInvalidState: domain.ErrConflict,
		approval.ErrExpired:      domain.ErrConflict,
		approval.ErrNotFound:     domain.ErrNotFound,
		approval.ErrSameActor:    domain.ErrForbidden,
	} {
		f := newInboxFixture()
		key := f.prepare(t, 7, maker, 900)
		f.keel.decideErr = keelErr
		if _, err := f.inbox.Decide(context.Background(), checker, key, true, ""); !errors.Is(err, want) || !errors.Is(err, keelErr) {
			t.Fatalf("%v: want %v, got %v", keelErr, want, err)
		}
	}
}

func TestInboxWithdrawClosesTheKeelRequest(t *testing.T) {
	f := newInboxFixture()
	ctx := context.Background()
	key := f.prepare(t, 7, maker, 900)
	if err := f.inbox.Withdraw(ctx, checker, key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another person's confirmation: want ErrNotFound, got %v", err)
	}
	f.keel.withdrawErr = approval.ErrInvalidState
	if err := f.inbox.Withdraw(ctx, maker, key); err != nil {
		t.Fatalf("a keel request already closed is tolerated: %v", err)
	}
	if want := (keelCall{7, 900, 41, false, ""}); len(f.keel.withdrawals) != 1 || f.keel.withdrawals[0] != want {
		t.Fatalf("keel saw %+v", f.keel.withdrawals)
	}
	if stored, _ := f.store.Get(ctx, key); stored.Status != domain.MCPConfirmationWithdrawn {
		t.Fatalf("withdrawn: %+v", stored)
	}

	other := newInboxFixture()
	key = other.prepare(t, 7, maker, 900)
	other.keel.withdrawErr = approval.ErrNotMaker
	if err := other.inbox.Withdraw(ctx, maker, key); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a keel refusal is reported: want ErrForbidden, got %v", err)
	}
}

func TestInboxReconcilerIsNotADecider(t *testing.T) {
	f := newInboxFixture()
	ctx := context.Background()
	key := unknownConfirmation(t, f.fixture)
	f.auth.viewer.reconcilerErr = errors.New("not an operator")
	if entries, _ := f.inbox.List(ctx, checker, time.Time{}, 0); len(entries) != 1 || entries[0].CanReconcile || entries[0].CanDecide {
		t.Fatalf("permissions %+v", entries)
	}
	if _, err := f.inbox.Reconcile(ctx, checker, key, domain.MCPConfirmationExecuted, "seen"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a refused reconciler: want ErrForbidden, got %v", err)
	}
	f.auth.viewer.reconcilerErr = nil
	f.checks.reconcilerErr = map[string]error{checker.ID: errors.New("no longer an operator")}
	if _, err := f.executor.Reconcile(ctx, key, checker, domain.MCPConfirmationExecuted, "seen"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("the checker refuses a reconciler who may decide: want ErrForbidden, got %v", err)
	}
	f.checks.reconcilerErr = nil
	if entries, _ := f.inbox.List(ctx, checker, time.Time{}, 0); !entries[0].CanReconcile {
		t.Fatalf("permissions %+v", entries)
	}
	reconciled, err := f.inbox.Reconcile(ctx, checker, key, domain.MCPConfirmationExecuted, "seen")
	if err != nil || reconciled.Status != domain.MCPConfirmationExecuted || reconciled.Reconciler != checker {
		t.Fatalf("Reconcile = %+v, %v", reconciled, err)
	}
}

func TestInboxOnDecidedIgnoresOtherObjectTypes(t *testing.T) {
	f := newInboxFixture()
	ctx := context.Background()
	key := f.prepare(t, 7, maker, 900)
	other := &approval.Request{ID: 900, PartnerID: 7, ObjectType: "invoice", ObjectID: key.ID, Status: approval.StatusApproved, CheckerID: 42}
	if err := f.inbox.OnDecided(ctx, f.db, other); err != nil {
		t.Fatal(err)
	}
	if err := (&Inbox{}).OnDecided(ctx, f.db, other); err != nil {
		t.Fatal(err)
	}
	if stored, _ := f.store.Get(ctx, key); stored.Status != domain.MCPConfirmationPending {
		t.Fatalf("another object type changed the confirmation: %+v", stored)
	}
	other.ObjectType = ApprovalObjectType
	if err := (&Inbox{}).OnDecided(ctx, f.db, other); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no store: want ErrNotReady, got %v", err)
	}
	if err := f.inbox.OnDecided(ctx, f.db, other); err != nil {
		t.Fatal(err)
	}
	if stored, _ := f.store.Get(ctx, key); stored.Status != domain.MCPConfirmationApproved {
		t.Fatalf("a confirmation approval decides it: %+v", stored)
	}
}
