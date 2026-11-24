// Package txn names a keel transaction in the errors of its begin and commit.
package txn

import (
	"context"
	"fmt"

	"github.com/nauticana/keel/data"
	keelport "github.com/nauticana/keel/port"
)

// Transaction is one atomic unit over a query catalog.
type Transaction struct {
	db      keelport.DatabaseRepository
	queries map[string]string
	name    string
}

func New(db keelport.DatabaseRepository, queries map[string]string, name string) Transaction {
	return Transaction{db: db, queries: queries, name: name}
}

// Run runs fn under data.InTx: committed when fn returns nil, rolled back otherwise. fn's error is returned
// unnamed, joined with a failed rollback; a failed begin or commit carries the name.
func (t Transaction) Run(ctx context.Context, fn func(tx keelport.TxQueryService) error) error {
	if t.db == nil {
		return fmt.Errorf("%s: database is required", t.name)
	}
	var fnErr error
	err := data.InTx(ctx, t.db, t.queries, func(tx keelport.TxQueryService) error {
		fnErr = fn(tx)
		return fnErr
	})
	if err != nil && fnErr == nil {
		return fmt.Errorf("%s: %w", t.name, err)
	}
	return err
}

// Value is Run for an fn with a result, which is the zero value unless the transaction committed.
func Value[T any](ctx context.Context, t Transaction, fn func(tx keelport.TxQueryService) (T, error)) (T, error) {
	var value T
	err := t.Run(ctx, func(tx keelport.TxQueryService) error {
		var err error
		value, err = fn(tx)
		return err
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return value, nil
}
