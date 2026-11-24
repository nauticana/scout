package txn

import (
	"context"

	keelport "github.com/nauticana/keel/port"
)

type fakeDB struct {
	keelport.DatabaseRepository
	tx       *fakeTx
	beginErr error
}

func (db fakeDB) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	if db.beginErr != nil {
		return nil, db.beginErr
	}
	return db.tx, nil
}
