package txn

import (
	"context"
	"errors"
	"strings"
	"testing"

	keelport "github.com/nauticana/keel/port"
)

func TestRunNamesBeginAndCommitFailuresOnly(t *testing.T) {
	down := errors.New("down")
	ok := func(keelport.TxQueryService) error { return nil }
	if err := New(nil, nil, "op").Run(context.Background(), ok); err == nil || err.Error() != "op: database is required" {
		t.Fatalf("nil database = %v", err)
	}
	if err := New(fakeDB{beginErr: down}, nil, "op").Run(context.Background(), ok); !errors.Is(err, down) || err.Error() != "op: down" {
		t.Fatalf("begin = %v", err)
	}
	tx := &fakeTx{commitErr: down}
	if err := New(fakeDB{tx: tx}, nil, "op").Run(context.Background(), ok); err == nil || err.Error() != "op: down" || tx.rollbacks != 0 {
		t.Fatalf("commit = %v, rollbacks = %d", err, tx.rollbacks)
	}
	tx = &fakeTx{}
	if err := New(fakeDB{tx: tx}, nil, "op").Run(context.Background(), func(keelport.TxQueryService) error { return down }); err != down || tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("fn = %v, commits = %d, rollbacks = %d", err, tx.commits, tx.rollbacks)
	}
}

func TestRunSurfacesAFailedRollbackAndOutlivesCancellation(t *testing.T) {
	refused, lost := errors.New("refused"), errors.New("connection lost")
	ctx, cancel := context.WithCancel(context.Background())
	tx := &fakeTx{rollbackErr: lost}
	err := New(fakeDB{tx: tx}, nil, "op").Run(ctx, func(keelport.TxQueryService) error { cancel(); return refused })
	if !errors.Is(err, refused) || !errors.Is(err, lost) || tx.rollbackCtxErr != nil {
		t.Fatalf("err = %v, rollback ctx = %v", err, tx.rollbackCtxErr)
	}
}

func TestValueIsZeroUnlessCommitted(t *testing.T) {
	seven := func(keelport.TxQueryService) (int, error) { return 7, nil }
	if value, err := Value(context.Background(), New(fakeDB{tx: &fakeTx{}}, nil, "op"), seven); err != nil || value != 7 {
		t.Fatalf("committed = %d, %v", value, err)
	}
	value, err := Value(context.Background(), New(fakeDB{tx: &fakeTx{commitErr: errors.New("down")}}, nil, "op"), seven)
	if value != 0 || err == nil || !strings.HasPrefix(err.Error(), "op: ") {
		t.Fatalf("uncommitted = %d, %v", value, err)
	}
}
