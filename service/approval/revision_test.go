package approval

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// memoryArchive is a content-addressed archive; tamper replaces what Hydrate returns.
type memoryArchive struct {
	objects map[string][]byte
	tamper  []byte
	writes  int
}

func (a *memoryArchive) Dehydrate(_ context.Context, name string, payload []byte) (domain.ObjectRef, error) {
	a.writes++
	digest := ArgumentsDigest(payload)
	uri := "mem://" + name + "/" + digest
	a.objects[uri] = bytes.Clone(payload)
	return domain.ObjectRef{URI: uri, Digest: digest}, nil
}

func (a *memoryArchive) Hydrate(_ context.Context, ref domain.ObjectRef) ([]byte, error) {
	if a.tamper != nil {
		return a.tamper, nil
	}
	payload, ok := a.objects[ref.URI]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return payload, nil
}

type revisionFixture struct {
	table    *approvalTable
	store    *TableStore
	archive  *memoryArchive
	gate     *Gate
	resumer  *Resumer
	original domain.ToolCall
	key      domain.ApprovalKey
	// mayDecideRevision lets the editor decide the revised proposal.
	mayDecideRevision                bool
	resumed, failed, requeued, audit int
}

var editor = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}

func newRevisionFixture(t *testing.T) *revisionFixture {
	t.Helper()
	f := &revisionFixture{archive: &memoryArchive{objects: map[string][]byte{}}, mayDecideRevision: true}
	f.table, f.store = newApprovalStore(false)
	authorizer := fake.ApprovalAuthorizerFunc(func(_ context.Context, request domain.ApprovalRequest, decider domain.PrincipalRef) error {
		if decider != editor || request.RevisesStepID != 0 && !f.mayDecideRevision {
			return domain.ErrForbidden
		}
		return nil
	})
	audit := &fake.AuditSink{RecordFunc: func(_ context.Context, record domain.DecisionRecord) error {
		if strings.Contains(record.Reason, "amount") {
			t.Fatalf("revised arguments leaked into audit: %+v", record)
		}
		f.audit++
		return nil
	}}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	f.gate = &Gate{Store: f.store, Archive: f.archive, Authorizer: authorizer, Audit: audit, Now: now}
	f.resumer = &Resumer{
		Store: f.store, Authorizer: authorizer, Archive: f.archive, Audit: audit, Now: now,
		Records: &fake.TurnRecordStore{
			ResumeFunc:     func(context.Context, int64, string) error { f.resumed++; return nil },
			FailParkedFunc: func(context.Context, int64, string, string) error { f.failed++; return nil },
		},
		Queue: requeueFunc(func(context.Context, int64, string) error { f.requeued++; return nil }),
	}
	f.original = toolCall(`{"amount":1000}`)
	f.original.IdempotencyKey = "loop:call-1"
	if decision, err := f.gate.Decide(context.Background(), f.original, revisionRule); err != nil || decision != domain.ApprovalPending {
		t.Fatalf("open original: %q, %v", decision, err)
	}
	f.key = domain.ApprovalKey{TenantID: 7, RequestID: "request", ExecutionStepID: stepIDAt(f.original, 0)}
	return f
}

// revisionRule is the rule under which the fixture parks its call.
var revisionRule = autonomyAsk

func (f *revisionFixture) edit(arguments string) (domain.ApprovalRequest, error) {
	return f.resumer.Resolve(context.Background(), domain.ApprovalVerdict{
		RequestKey: f.key, Status: domain.ApprovalStatusEdited, Decider: editor,
		ProposedDigest: ProposalDigest(f.original), Reason: "lower the refund", RevisedArguments: []byte(arguments),
	})
}

func (f *revisionFixture) revisedCall(t *testing.T) domain.ToolCall {
	t.Helper()
	revision, revised, err := f.gate.Revision(context.Background(), f.original)
	if err != nil || !revised {
		t.Fatalf("Revision = %+v, %v, %v", revision, revised, err)
	}
	call := f.original
	call.Arguments, call.IdempotencyKey, call.RevisesProposal = revision.Arguments, "revision:1", revision.RevisesProposal
	return call
}

func TestEditedVerdictArchivesTheRevisionAndResumesTheTurn(t *testing.T) {
	f := newRevisionFixture(t)
	request, err := f.edit(` { "amount" : 5 } `)
	if err != nil || request.Status != domain.ApprovalStatusEdited {
		t.Fatalf("edit = %+v, %v", request, err)
	}
	if f.resumed != 1 || f.requeued != 1 || f.failed != 0 || f.audit != 1 {
		t.Fatalf("resumed %d, requeued %d, failed %d, audited %d: an edit continues the turn", f.resumed, f.requeued, f.failed, f.audit)
	}
	decision := f.table.decisions[request.ID]
	if decision[11] == nil || decision[12] != ArgumentsDigest([]byte(`{"amount":5}`)) {
		t.Fatalf("decision row keeps uri %v and digest %v, want the compact revision's", decision[11], decision[12])
	}
	revision, revised, err := f.gate.Revision(context.Background(), f.original)
	if err != nil || !revised || string(revision.Arguments) != `{"amount":5}` || revision.RevisesProposal != f.key.ExecutionStepID ||
		revision.ArgumentsDigest != ArgumentsDigest(revision.Arguments) {
		t.Fatalf("Revision = %+v, %v, %v", revision, revised, err)
	}
	// An exact retry of the edit replays the stored verdict without archiving again.
	if _, err := f.edit(`{"amount":5}`); err != nil || f.archive.writes != 1 {
		t.Fatalf("retry of the same edit: %v, archived %d times", err, f.archive.writes)
	}
	if _, err := f.edit(`{"amount":6}`); !errors.Is(err, domain.ErrConflict) || f.archive.writes != 1 {
		t.Fatalf("a different second edit: want ErrConflict and no archive write, got %v, %d writes", err, f.archive.writes)
	}
}

func TestEditOfAClosedOrStaleProposalArchivesNothing(t *testing.T) {
	for _, status := range []domain.ApprovalStatus{domain.ApprovalStatusApproved, domain.ApprovalStatusRejected} {
		f := newRevisionFixture(t)
		if _, err := f.resumer.Resolve(context.Background(), domain.ApprovalVerdict{
			RequestKey: f.key, Status: status, Decider: editor, ProposedDigest: ProposalDigest(f.original),
		}); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if _, err := f.edit(`{"amount":5}`); !errors.Is(err, domain.ErrConflict) || f.archive.writes != 0 {
			t.Fatalf("edit of a %s request: want ErrConflict and no archive write, got %v, %d writes", status, err, f.archive.writes)
		}
	}
	f := newRevisionFixture(t)
	_, err := f.resumer.Resolve(context.Background(), domain.ApprovalVerdict{
		RequestKey: f.key, Status: domain.ApprovalStatusEdited, Decider: editor,
		ProposedDigest: strings.Repeat("0", 64), RevisedArguments: []byte(`{"amount":5}`),
	})
	if !errors.Is(err, domain.ErrConflict) || f.archive.writes != 0 {
		t.Fatalf("edit of another proposal: want ErrConflict and no archive write, got %v, %d writes", err, f.archive.writes)
	}
}

func TestOriginalOfAnEditedProposalNeverRuns(t *testing.T) {
	f := newRevisionFixture(t)
	if _, err := f.edit(`{"amount":5}`); err != nil {
		t.Fatal(err)
	}
	decision, err := f.gate.Decide(context.Background(), f.original, revisionRule)
	if decision != domain.ApprovalDenied || !errors.Is(err, domain.ErrProposalRevised) {
		t.Fatalf("original after edit: %q, %v", decision, err)
	}
}

func TestRevisedCallIsApprovedByAnEditorWhoMayDecideIt(t *testing.T) {
	f := newRevisionFixture(t)
	if _, err := f.edit(`{"amount":5}`); err != nil {
		t.Fatal(err)
	}
	call := f.revisedCall(t)
	for range 2 {
		decision, err := f.gate.Decide(context.Background(), call, revisionRule)
		if err != nil || decision != domain.ApprovalApproved {
			t.Fatalf("revised call: %q, %v", decision, err)
		}
	}
	revised, err := f.store.Get(context.Background(), domain.ApprovalKey{TenantID: 7, RequestID: "request", ExecutionStepID: stepIDAt(call, 0)})
	if err != nil || revised.RevisesStepID != f.key.ExecutionStepID || revised.ProposedDigest != ProposalDigest(call) ||
		revised.Evidence.Digest != ArgumentsDigest(call.Arguments) || revised.Status != domain.ApprovalStatusApproved {
		t.Fatalf("revised proposal = %+v, %v", revised, err)
	}
	if decider := f.table.decisions[revised.ID]; decider[4] != int64(42) || decider[11] != nil {
		t.Fatalf("revision decision = %v, want the editor's approval without a revision of its own", decider)
	}
}

func TestRevisedCallAwaitsItsOwnApproverWhenTheEditorMayNotDecideIt(t *testing.T) {
	f := newRevisionFixture(t)
	if _, err := f.edit(`{"amount":5}`); err != nil {
		t.Fatal(err)
	}
	f.mayDecideRevision = false
	decision, err := f.gate.Decide(context.Background(), f.revisedCall(t), revisionRule)
	if err != nil || decision != domain.ApprovalPending {
		t.Fatalf("revised call: %q, %v, want pending for its approver", decision, err)
	}
}

func TestRevisedCallMustCarryTheEdit(t *testing.T) {
	f := newRevisionFixture(t)
	if _, err := f.edit(`{"amount":5}`); err != nil {
		t.Fatal(err)
	}
	valid := f.revisedCall(t)
	forged := valid
	forged.Arguments = []byte(`{"amount":5000}`)
	otherTool := valid
	otherTool.ToolID = "transfer"
	unedited := valid
	unedited.RevisesProposal = 999
	for name, call := range map[string]domain.ToolCall{"other arguments": forged, "other tool": otherTool, "unedited step": unedited} {
		decision, err := f.gate.Decide(context.Background(), call, revisionRule)
		if decision != domain.ApprovalDenied || err == nil {
			t.Fatalf("%s: %q, %v, want refused", name, decision, err)
		}
	}
}

func TestRevisionFailsClosedOnTamperedArguments(t *testing.T) {
	f := newRevisionFixture(t)
	if _, err := f.edit(`{"amount":5}`); err != nil {
		t.Fatal(err)
	}
	f.archive.tamper = []byte(`{"amount":5000}`)
	if _, revised, err := f.gate.Revision(context.Background(), f.original); revised || !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("tampered revision: revised %v, %v", revised, err)
	}
}

func TestUneditedProposalHasNoRevision(t *testing.T) {
	f := newRevisionFixture(t)
	if _, revised, err := f.gate.Revision(context.Background(), f.original); revised || err != nil {
		t.Fatalf("pending proposal: revised %v, %v", revised, err)
	}
	stranger := toolCall(`{"amount":1}`)
	stranger.ToolID = "never-asked"
	if _, revised, err := f.gate.Revision(context.Background(), stranger); revised || err != nil {
		t.Fatalf("unknown proposal: revised %v, %v", revised, err)
	}
}

func TestEditRefusesInvalidRevisions(t *testing.T) {
	for name, arguments := range map[string]string{
		"empty": "  ", "array": `[1]`, "string": `"x"`, "malformed": `{"amount":`, "trailing": `{"a":1} {"b":2}`,
		"too large": `{"note":"` + strings.Repeat("x", 100) + `"}`,
	} {
		f := newRevisionFixture(t)
		f.resumer.MaxRevisedArgumentBytes = 64
		if _, err := f.edit(arguments); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
		if stored, _ := f.store.Get(context.Background(), f.key); stored.Status != domain.ApprovalStatusPending || f.resumed+f.failed != 0 {
			t.Fatalf("%s: a refused edit must leave the proposal open: %+v", name, stored)
		}
	}
	f := newRevisionFixture(t)
	f.resumer.Archive = nil
	if _, err := f.edit(`{"amount":5}`); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no archive: want ErrNotReady, got %v", err)
	}
	_, err := f.resumer.Resolve(context.Background(), domain.ApprovalVerdict{
		RequestKey: f.key, Status: domain.ApprovalStatusApproved, Decider: editor,
		ProposedDigest: ProposalDigest(f.original), RevisedArguments: []byte(`{"amount":5}`),
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("approval with revised arguments: want ErrValidation, got %v", err)
	}
}

func TestEditRequiresTheDeciderToDecideTheOriginal(t *testing.T) {
	f := newRevisionFixture(t)
	_, err := f.resumer.Resolve(context.Background(), domain.ApprovalVerdict{
		RequestKey: f.key, Status: domain.ApprovalStatusEdited, Decider: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "7"},
		ProposedDigest: ProposalDigest(f.original), RevisedArguments: []byte(`{"amount":5}`),
	})
	if !errors.Is(err, domain.ErrForbidden) || len(f.archive.objects) != 0 {
		t.Fatalf("unauthorized edit: %v, archived %d", err, len(f.archive.objects))
	}
}

func TestStoreRefusesAnEditWithoutItsRevision(t *testing.T) {
	_, store := newApprovalStore(false)
	ctx := context.Background()
	request := owed("turn-1", "")
	if _, err := store.Open(ctx, request); err != nil {
		t.Fatal(err)
	}
	verdict := domain.ApprovalVerdict{
		RequestKey: domain.ApprovalKey{TenantID: 7, RequestID: "turn-1", ExecutionStepID: 3}, Status: domain.ApprovalStatusEdited,
		Decider: editor, ProposedDigest: request.ProposedDigest,
	}
	if _, err := store.Resolve(ctx, verdict); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("edit without revision: want ErrValidation, got %v", err)
	}
	verdict.Status, verdict.Revision = domain.ApprovalStatusApproved, domain.ObjectRef{URI: "mem://x", Digest: strings.Repeat("c", 64)}
	if _, err := store.Resolve(ctx, verdict); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("approval with a revision: want ErrValidation, got %v", err)
	}
	if _, err := store.Revision(ctx, verdict.RequestKey); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("revision of an unedited request: want ErrNotFound, got %v", err)
	}
	selfRevision := owed("turn-2", "")
	selfRevision.RevisesStepID = selfRevision.ExecutionStepID
	if _, err := store.Open(ctx, selfRevision); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a request revising itself: want ErrValidation, got %v", err)
	}
}
