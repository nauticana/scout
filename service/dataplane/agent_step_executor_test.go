package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

type stubDelegation struct {
	authorization domain.DelegationAuthorization
	err           error
}

func (s stubDelegation) Authorize(context.Context, domain.Principal, domain.PrincipalRef, string) (domain.DelegationAuthorization, error) {
	return s.authorization, s.err
}

type stubWork struct {
	assigned  []domain.WorkItem
	ancestors []domain.WorkItem
	completed []string
}

func (w *stubWork) Assign(_ context.Context, item domain.WorkItem) (domain.WorkItem, error) {
	item.ID = int64(len(w.assigned) + 1)
	w.assigned = append(w.assigned, item)
	return item, nil
}

func (w *stubWork) Get(context.Context, int64, int64) (domain.WorkItem, error) {
	return domain.WorkItem{}, domain.ErrNotFound
}

func (w *stubWork) Pending(context.Context, int64, domain.PrincipalRef, int) ([]domain.WorkItem, error) {
	return nil, nil
}

func (w *stubWork) Complete(_ context.Context, _, _ int64, status string) error {
	w.completed = append(w.completed, status)
	return nil
}

func (w *stubWork) Ancestors(context.Context, int64, int64) ([]domain.WorkItem, error) {
	return w.ancestors, nil
}

type stubInvoker struct {
	call domain.DelegatedCall
	err  error
}

func (i *stubInvoker) Invoke(_ context.Context, call domain.DelegatedCall) (domain.StepResult, error) {
	i.call = call
	return domain.StepResult{State: []byte("delegated"), NextStepID: "done"}, i.err
}

type stubCodec struct{}

func (stubCodec) Dehydrate(_ context.Context, name string, payload []byte) (domain.ObjectRef, error) {
	return domain.ObjectRef{URI: "scout://" + name, Digest: string(make([]byte, 0, 64))}, nil
}

func (stubCodec) Hydrate(context.Context, domain.ObjectRef) ([]byte, error) { return nil, nil }

func (stubCodec) Delete(context.Context, domain.ObjectRef) error { return nil }

func delegatingStep(target string) domain.StepInput {
	return domain.StepInput{
		Step: domain.ExecutionStep{
			ExecutionStepID: 5, StepID: "delegate", Kind: AgentStepKind,
			Configuration: []byte(`{"target":{"kind":"agent","id":"` + target + `"},"action":"invoice:approve"}`),
		},
		Snapshot:  domain.SessionSnapshot{ConversationID: "c1", LatestTurnNo: 3, State: []byte("state")},
		Principal: domain.Principal{Kind: domain.PrincipalAgent, ID: "caller", TenantID: 7, Release: "3"},
		Bounds:    domain.DelegationBounds{RemainingDepth: 3, BudgetMinorUnits: 1000, Currency: "EUR"},
		RequestID: "request",
	}
}

func executor(work *stubWork, invoker *stubInvoker, bounds domain.DelegationBounds) *AgentStepExecutor {
	return &AgentStepExecutor{
		Delegation: stubDelegation{authorization: domain.DelegationAuthorization{
			GrantID: "g1", Authority: domain.AuthorityHop{GrantID: "g1"}, Bounds: bounds,
		}}, Work: work, Invoker: invoker, Objects: stubCodec{},
	}
}

func TestAgentStepNarrowsBoundsBeforeInvoking(t *testing.T) {
	work, invoker := &stubWork{}, &stubInvoker{}
	step := executor(work, invoker, domain.DelegationBounds{RemainingDepth: 9, BudgetMinorUnits: 9000, Currency: "EUR"})
	if _, err := step.Execute(context.Background(), delegatingStep("assistant")); err != nil {
		t.Fatal(err)
	}
	// The grant offers more than the caller holds; the hop must still shrink.
	if invoker.call.Bounds.RemainingDepth != 2 || invoker.call.Bounds.BudgetMinorUnits != 1000 {
		t.Fatalf("bounds = %+v, want the caller's bounds to bind", invoker.call.Bounds)
	}
	if len(work.assigned) != 1 || work.assigned[0].Assignee.ID != "assistant" {
		t.Fatalf("assigned = %+v", work.assigned)
	}
	if work.assigned[0].GrantID != "g1" || invoker.call.Authority.GrantID != "g1" {
		t.Fatalf("delegation lost grant identity: item = %+v, call = %+v", work.assigned[0], invoker.call)
	}
	if len(work.completed) != 1 || work.completed[0] != "completed" {
		t.Fatalf("completed = %v", work.completed)
	}
}

func TestAgentStepRefusesSelfDelegation(t *testing.T) {
	step := executor(&stubWork{}, &stubInvoker{}, domain.DelegationBounds{RemainingDepth: 2})
	_, err := step.Execute(context.Background(), delegatingStep("caller"))
	if !errors.Is(err, domain.ErrLoopDetected) {
		t.Fatalf("error = %v, want self-delegation refused", err)
	}
}

func TestAgentStepRefusesATargetAlreadyInTheChain(t *testing.T) {
	work := &stubWork{ancestors: []domain.WorkItem{
		{Assignee: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "assistant"}},
	}}
	step := executor(work, &stubInvoker{}, domain.DelegationBounds{RemainingDepth: 2})
	input := delegatingStep("assistant")
	input.WorkItemID = 9
	_, err := step.Execute(context.Background(), input)
	if !errors.Is(err, domain.ErrLoopDetected) {
		t.Fatalf("error = %v, want a cycle refused before any work is assigned", err)
	}
	if len(work.assigned) != 0 {
		t.Fatalf("assigned = %+v, want nothing assigned on a cycle", work.assigned)
	}
}

func TestAgentStepStopsWhenDepthIsExhausted(t *testing.T) {
	step := executor(&stubWork{}, &stubInvoker{}, domain.DelegationBounds{RemainingDepth: 2})
	input := delegatingStep("assistant")
	input.Bounds.RemainingDepth = 0
	input.Principal.Authority = domain.AuthorityChain{{GrantID: "parent", Grantor: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}}
	if _, err := step.Execute(context.Background(), input); !errors.Is(err, domain.ErrDelegationDepth) {
		t.Fatalf("error = %v, want an exhausted chain refused", err)
	}
}

func TestAgentStepClosesTheWorkItemWhenTheCallFails(t *testing.T) {
	work := &stubWork{}
	step := executor(work, &stubInvoker{err: errors.New("unreachable")}, domain.DelegationBounds{RemainingDepth: 2})
	if _, err := step.Execute(context.Background(), delegatingStep("assistant")); err == nil {
		t.Fatal("a failed delegation must surface its error")
	}
	if len(work.completed) != 1 || work.completed[0] != "failed" {
		t.Fatalf("completed = %v, want the item closed as failed", work.completed)
	}
}

func TestAgentStepRequiresAPrincipal(t *testing.T) {
	step := executor(&stubWork{}, &stubInvoker{}, domain.DelegationBounds{RemainingDepth: 2})
	input := delegatingStep("assistant")
	input.Principal = domain.Principal{}
	if _, err := step.Execute(context.Background(), input); !errors.Is(err, domain.ErrPrincipalUnknown) {
		t.Fatalf("error = %v, want a zero principal refused", err)
	}
}
