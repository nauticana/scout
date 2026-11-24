package txn

import (
	"context"

	keelport "github.com/nauticana/keel/port"
)

type fakeTx struct {
	keelport.TxQueryService
	commits, rollbacks     int
	commitErr, rollbackErr error
	rollbackCtxErr         error
}

func (tx *fakeTx) Commit(context.Context) error { tx.commits++; return tx.commitErr }

func (tx *fakeTx) Rollback(ctx context.Context) error {
	tx.rollbacks++
	tx.rollbackCtxErr = ctx.Err()
	return tx.rollbackErr
}
