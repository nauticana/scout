package skill

import (
	"context"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"
)

// skillLoadDB records each named query and answers it with the rows set for its name.
type skillLoadDB struct {
	keelport.DatabaseRepository
	answers map[string][][]any
	calls   map[string][][]any
	err     error
}

func (db *skillLoadDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db
}

func (db *skillLoadDB) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if db.calls == nil {
		db.calls = map[string][][]any{}
	}
	db.calls[name] = append(db.calls[name], args)
	return &keelmodel.QueryResult{Rows: db.answers[name]}, db.err
}

func (*skillLoadDB) GenID() int64 { return 0 }

func (db *skillLoadDB) QueryService(string, map[string]string) keelport.QueryService { return db }
