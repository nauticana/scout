package confirmationtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/confirmation"
	"github.com/nauticana/scout/service/confirmation/confirmationtest"
)

func newStore() (*confirmationtest.Store, func(time.Duration)) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	return &confirmationtest.Store{Now: func() time.Time { return now }}, func(by time.Duration) { now = now.Add(by) }
}

func TestStoreConformance(t *testing.T) {
	confirmationtest.RunStoreSuite(t, func(*testing.T) confirmationtest.Harness {
		store, advance := newStore()
		return confirmationtest.Harness{Store: store, Prepare: store.Prepare, MarkLapsed: store.MarkLapsed, Advance: advance}
	})
}

type allow struct{}

func (allow) Maker(context.Context, domain.MCPConfirmation) error { return nil }
func (allow) Decider(context.Context, domain.MCPConfirmation, domain.PrincipalRef) error {
	return nil
}

type runnerFunc func(context.Context, domain.MCPConfirmation) (json.RawMessage, error)

func (f runnerFunc) RunConfirmed(ctx context.Context, c domain.MCPConfirmation) (json.RawMessage, error) {
	return f(ctx, c)
}

func TestExecutorRunsOverTheMemoryStore(t *testing.T) {
	store, _ := newStore()
	runs := 0
	var outcome error
	executor := &confirmation.Executor{Store: store, Checker: allow{}, OnFailure: func(domain.MCPConfirmationKey, error) {},
		Runner: runnerFunc(func(context.Context, domain.MCPConfirmation) (json.RawMessage, error) {
			runs++
			return json.RawMessage(`{"done":true}`), outcome
		})}
	maker, decider := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "1"}, domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "2"}
	prepare := func(tool string) domain.MCPConfirmationKey {
		payload := []byte(`{}`)
		stored, _, err := store.Prepare(t.Context(), domain.MCPConfirmationDraft{TenantID: 7, Maker: maker, Tool: tool,
			Digest: confirmation.ActionDigest(tool, payload), Payload: payload, Preview: json.RawMessage(`{}`),
			Channel: domain.MCPConfirmationInbox, TTL: time.Minute})
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		return domain.MCPConfirmationKey{TenantID: 7, ID: stored.ID}
	}

	key := prepare("publish")
	if _, err := executor.Decide(t.Context(), key, decider, true, ""); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if ran, err := executor.RunApproved(t.Context(), 10); err != nil || ran != 1 {
		t.Fatalf("run approved = %d, err = %v", ran, err)
	}
	if stored, err := executor.Execute(t.Context(), key); err != nil || stored.Status != domain.MCPConfirmationExecuted || runs != 1 {
		t.Fatalf("stored = %+v, runs = %d, err = %v", stored, runs, err)
	}

	outcome = fmt.Errorf("%w: timed out", domain.ErrEffectUnknown)
	key = prepare("delete")
	if _, err := executor.Decide(t.Context(), key, decider, true, ""); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if stored, err := executor.Execute(t.Context(), key); err != nil || stored.Status != domain.MCPConfirmationUnknown {
		t.Fatalf("stored = %+v, err = %v", stored, err)
	}
	if stored, err := executor.Reconcile(t.Context(), key, decider, domain.MCPConfirmationFailed, "not deleted"); err != nil || stored.Status != domain.MCPConfirmationFailed {
		t.Fatalf("reconciled = %+v, err = %v", stored, err)
	}
	if _, err := executor.Execute(t.Context(), key); err != nil || runs != 2 {
		t.Fatalf("a reconciled confirmation ran again: runs = %d, err = %v", runs, err)
	}
	if _, err := executor.Decide(t.Context(), key, decider, true, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("decision on a closed confirmation = %v, want conflict", err)
	}
}

func TestStoreCopiesJSON(t *testing.T) {
	store, _ := newStore()
	maker := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "1"}
	payload, preview := json.RawMessage(`{"value":1}`), json.RawMessage(`{"label":"one"}`)
	draft := domain.MCPConfirmationDraft{TenantID: 7, Maker: maker, Tool: "publish", Digest: confirmation.ActionDigest("publish", payload),
		Payload: payload, Preview: preview, Channel: domain.MCPConfirmationElicitation, TTL: time.Minute}
	stored, _, err := store.Prepare(t.Context(), draft)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	payload[9], preview[10] = '9', 'x'
	stored.Payload[9], stored.Preview[10] = '8', 'y'
	got, err := store.Get(t.Context(), domain.MCPConfirmationKey{TenantID: 7, ID: stored.ID})
	if err != nil || string(got.Payload) != `{"value":1}` || string(got.Preview) != `{"label":"one"}` {
		t.Fatalf("stored = %+v, err = %v", got, err)
	}
	key := domain.MCPConfirmationKey{TenantID: 7, ID: stored.ID}
	decider := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "2"}
	if err = store.Decide(t.Context(), key, decider, true, ""); err != nil {
		t.Fatalf("decide: %v", err)
	}
	fence, claimed, err := store.Claim(t.Context(), key, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed = %v, err = %v", claimed, err)
	}
	result := json.RawMessage(`{"done":true}`)
	if err = store.Complete(t.Context(), key, fence, domain.MCPConfirmationExecuted, result, ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	result[8] = 'f'
	got, err = store.Get(t.Context(), key)
	if err != nil || string(got.Result) != `{"done":true}` {
		t.Fatalf("result = %s, err = %v", got.Result, err)
	}
}
