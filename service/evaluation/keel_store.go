package evaluation

import (
	"context"
	"fmt"
	"sync"

	keelport "github.com/nauticana/keel/port"
)

// keelStore is the shared query-service bootstrap every keel-backed store embeds.
type keelStore struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

func (store *keelStore) queries(ctx context.Context, name string, queries map[string]string) (keelport.QueryService, error) {
	if store.DB == nil {
		return nil, fmt.Errorf("%s: database is required", name)
	}
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, queries) })
	if store.qs == nil {
		return nil, fmt.Errorf("%s: query service is required", name)
	}
	return store.qs, nil
}

func (store *keelStore) begin(ctx context.Context, name string, queries map[string]string) (keelport.TxQueryService, error) {
	if store.DB == nil {
		return nil, fmt.Errorf("%s: database is required", name)
	}
	tx, err := store.DB.BeginTx(ctx, queries)
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", name, err)
	}
	return tx, nil
}
