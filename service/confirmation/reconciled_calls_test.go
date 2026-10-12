package confirmation

import (
	"context"
	"errors"
	"testing"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// reconciledCalls records what the reconciling transaction asked it to settle.
type reconciledCalls struct {
	db       *confirmationDB
	keys     []domain.MCPConfirmationKey
	outcomes []domain.MCPConfirmationStatus
	inTx     []bool
	err      error
}

func (calls *reconciledCalls) ConfirmationReconciledTx(_ context.Context, tx keelport.TxQueryService, key domain.MCPConfirmationKey, outcome domain.MCPConfirmationStatus, _ domain.PrincipalRef) error {
	calls.keys, calls.outcomes = append(calls.keys, key), append(calls.outcomes, outcome)
	calls.inTx = append(calls.inTx, tx == keelport.TxQueryService(calls.db))
	return calls.err
}

// unknownConfirmation is an approved confirmation whose run ended unknown.
func unknownConfirmation(t *testing.T, f *fixture) domain.MCPConfirmationKey {
	t.Helper()
	ctx := context.Background()
	prepared, _, _ := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox))
	key := domain.MCPConfirmationKey{TenantID: 7, ID: prepared.ID}
	if _, err := f.executor.Decide(ctx, key, checker, true, ""); err != nil {
		t.Fatal(err)
	}
	f.runner.result, f.runner.err = nil, errors.Join(domain.ErrEffectUnknown, errors.New("vendor timed out"))
	if _, err := f.executor.RunApproved(ctx, 0); err != nil {
		t.Fatal(err)
	}
	return key
}

// The tool calls waiting on a confirmation settle in the transaction that reconciles it, inside its
// tenant; a confirmation that cannot be reconciled settles nothing, and a failed hook fails the
// reconciliation so its transaction rolls back.
func TestReconcileSettlesTheCallsWaitingOnTheConfirmation(t *testing.T) {
	ctx := context.Background()
	pending := newFixture()
	idle := &reconciledCalls{db: pending.db}
	pending.store.Calls = idle
	prepared, _, _ := pending.store.Prepare(ctx, draft(domain.MCPConfirmationInbox))
	if _, err := pending.executor.Reconcile(ctx, domain.MCPConfirmationKey{TenantID: 7, ID: prepared.ID}, checker, domain.MCPConfirmationExecuted, "verified"); !errors.Is(err, domain.ErrConflict) || len(idle.keys) != 0 {
		t.Fatalf("a pending confirmation = %v, settled %v", err, idle.keys)
	}

	failing := newFixture()
	failing.store.Calls = &reconciledCalls{db: failing.db, err: errors.New("call outcome store down")}
	if _, err := failing.executor.Reconcile(ctx, unknownConfirmation(t, failing), checker, domain.MCPConfirmationFailed, "never published"); err == nil {
		t.Fatal("a failed hook must fail the reconciliation, rolling its transaction back")
	}

	f := newFixture()
	calls := &reconciledCalls{db: f.db}
	f.store.Calls = calls
	key := unknownConfirmation(t, f)
	if _, err := f.executor.Reconcile(ctx, key, checker, domain.MCPConfirmationFailed, "never published"); err != nil {
		t.Fatal(err)
	}
	if len(calls.keys) != 1 || calls.keys[0] != key || calls.outcomes[0] != domain.MCPConfirmationFailed || !calls.inTx[0] {
		t.Fatalf("hook saw %+v %v in transaction %v", calls.keys, calls.outcomes, calls.inTx)
	}
}
