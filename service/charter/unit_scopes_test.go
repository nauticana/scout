package charter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/charter/sdk/organization"

	"github.com/nauticana/scout/domain"
)

// unitScopeRows is config_scope_unit keyed by unit object id; a transaction works on a copy until it commits.
type unitScopeRows struct {
	keelport.DatabaseRepository
	committed map[int64]string
	working   map[int64]string
}

func (rows *unitScopeRows) table() map[int64]string {
	if rows.working != nil {
		return rows.working
	}
	return rows.committed
}

func (rows *unitScopeRows) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	table := rows.table()
	switch name {
	case qUnitScopePut:
		table[args[1].(int64)] = args[2].(string)
	case qUnitScopeDelete:
		delete(table, args[1].(int64))
	case qUnitScopeList:
		result := &keelmodel.QueryResult{}
		for unitID, scopeID := range table {
			result.Rows = append(result.Rows, []any{unitID, scopeID})
		}
		return result, nil
	}
	return &keelmodel.QueryResult{}, nil
}

func (*unitScopeRows) GenID() int64 { return 0 }
func (rows *unitScopeRows) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return rows
}
func (rows *unitScopeRows) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	rows.working = make(map[int64]string, len(rows.committed))
	for key, scopeID := range rows.committed {
		rows.working[key] = scopeID
	}
	return rows, nil
}
func (rows *unitScopeRows) Commit(context.Context) error {
	rows.committed, rows.working = rows.working, nil
	return nil
}
func (rows *unitScopeRows) Rollback(context.Context) error {
	rows.working = nil
	return nil
}

// scopeTree is a ScopeRepository over parent links: tenant > plant > planning, and tenant > sales.
type scopeTree map[string]string

func (tree scopeTree) Chain(_ context.Context, tenantID int64, scopeID string) (domain.ScopeChain, error) {
	var chain domain.ScopeChain
	for id := scopeID; id != ""; id = tree[id] {
		if _, known := tree[id]; !known {
			return nil, fmt.Errorf("%w: scope %s", domain.ErrNotFound, id)
		}
		chain = append(domain.ScopeChain{{TenantID: tenantID, ScopeID: id, ParentScopeID: tree[id]}}, chain...)
	}
	return chain, nil
}

func (scopeTree) Bindings(context.Context, int64, []string, time.Time) ([]domain.ScopedBinding, error) {
	return nil, nil
}

// The store holds production (101), production planning (102), sales operations (103), and a
// unit of another namespace (104).
func unitScopesUnder(t *testing.T) (*UnitScopes, *unitScopeRows) {
	t.Helper()
	rows := &unitScopeRows{committed: map[int64]string{}}
	unit := func(namespace, id string) model.ObjectRef {
		return model.ObjectRef{Kind: model.KindOrganizationUnit, Namespace: namespace, ID: id}
	}
	store := &objectStore{refs: []model.ObjectRef{unit(harborNS, "OU-PRODUCTION"), unit(harborNS, "OU-PRODUCTION-PLANNING"),
		unit(harborNS, "OU-SALES-OPERATIONS"), unit("elsewhere", "OU-PRODUCTION-PLANNING")}}
	return &UnitScopes{
		DB: rows, Organization: organization.NewBaseProvider(harbor(t)),
		Scopes:  scopeTree{"tenant": "", "plant": "tenant", "planning": "plant", "sales": "tenant"},
		Objects: &DocumentObjects{Store: store, Namespace: func(int64) string { return harborNS }},
	}, rows
}

func TestUnitScopesAttachKeepsTheOrganizationTreeInsideTheScopeTree(t *testing.T) {
	ctx := context.Background()
	scopes, rows := unitScopesUnder(t)
	if err := scopes.Attach(ctx, 7, model.Ref{ID: "OU-PRODUCTION"}, "plant"); err != nil {
		t.Fatal(err)
	}
	// Production planning sits under production, so its scope must sit under plant.
	if err := scopes.Attach(ctx, 7, model.Ref{ID: "OU-PRODUCTION-PLANNING"}, "sales"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a child scope under another branch = %v", err)
	}
	if err := scopes.Attach(ctx, 7, model.Ref{ID: "OU-PRODUCTION-PLANNING"}, "planning"); err != nil {
		t.Fatal(err)
	}
	// Re-attaching the parent outside its attached child's chain is refused too, and the refusal writes nothing.
	if err := scopes.Attach(ctx, 7, model.Ref{ID: "OU-PRODUCTION"}, "sales"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a parent scope outside its child's chain = %v", err)
	}
	if rows.committed[101] != "plant" || rows.working != nil {
		t.Fatalf("rows = %+v", rows.committed)
	}
	for name, attach := range map[string]func() error{
		"unknown unit":  func() error { return scopes.Attach(ctx, 7, model.Ref{ID: "OU-NOWHERE"}, "plant") },
		"unknown scope": func() error { return scopes.Attach(ctx, 7, model.Ref{ID: "OU-SALES-OPERATIONS"}, "nowhere") },
	} {
		if err := attach(); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("%s = %v", name, err)
		}
	}
	if err := scopes.Attach(ctx, 7, model.Ref{ID: "OU-SALES-OPERATIONS"}, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank scope = %v", err)
	}
	// A unit the provider knows but the store does not hold has no row to reference.
	scopes.Objects.Store.(*objectStore).refs[2].ID = "OU-UNSTORED"
	if err := scopes.Attach(ctx, 7, model.Ref{ID: "OU-SALES-OPERATIONS"}, "sales"); !errors.Is(err, domain.ErrNotFound) || rows.working != nil {
		t.Fatalf("unstored unit = %v", err)
	}
	if _, attached := rows.committed[103]; attached {
		t.Fatalf("rows = %+v", rows.committed)
	}
	if err := (&UnitScopes{}).Attach(ctx, 7, model.Ref{ID: "OU-PRODUCTION"}, "plant"); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("zero value = %v", err)
	}
}

func TestUnitScopesResolveThroughPositionsAndAssignments(t *testing.T) {
	ctx := context.Background()
	scopes, rows := unitScopesUnder(t)
	position := model.ObjectRef{Kind: model.KindPosition, ID: "PP-PLN-01"}
	// Nothing attached: the tenant root.
	if scopeID, err := scopes.Resolve(ctx, 7, position); err != nil || scopeID != "" {
		t.Fatalf("unattached = %q, %v", scopeID, err)
	}
	// An attachment of a unit in another namespace never answers for this one.
	rows.committed[104] = "planning"
	if scopeID, err := scopes.Resolve(ctx, 7, position); err != nil || scopeID != "" {
		t.Fatalf("another namespace = %q, %v", scopeID, err)
	}
	if err := scopes.Attach(ctx, 7, model.Ref{ID: "OU-PRODUCTION"}, "plant"); err != nil {
		t.Fatal(err)
	}
	// The position's unit has no attachment of its own, so the nearest attached ancestor answers.
	if scopeID, err := scopes.Resolve(ctx, 7, position); err != nil || scopeID != "plant" {
		t.Fatalf("through the ancestor = %q, %v", scopeID, err)
	}
	if err := scopes.Attach(ctx, 7, model.Ref{ID: "OU-PRODUCTION-PLANNING"}, "planning"); err != nil {
		t.Fatal(err)
	}
	if scopeID, err := scopes.Resolve(ctx, 7, position); err != nil || scopeID != "planning" {
		t.Fatalf("through the unit = %q, %v", scopeID, err)
	}
	assignment := model.ObjectRef{Kind: model.KindAssignment, ID: "ASGN-DQA-PP-PLN-01"}
	if scopeID, err := scopes.Resolve(ctx, 7, assignment); err != nil || scopeID != "planning" {
		t.Fatalf("through the assignment = %q, %v", scopeID, err)
	}
	if scopeID, err := scopes.Resolve(ctx, 7, model.ObjectRef{Kind: model.KindPosition, ID: "POS-SALES-OPERATIONS-MANAGER"}); err != nil || scopeID != "" {
		t.Fatalf("another branch = %q, %v", scopeID, err)
	}
	if err := scopes.Detach(ctx, 7, model.Ref{ID: "OU-PRODUCTION-PLANNING"}); err != nil {
		t.Fatal(err)
	}
	if scopeID, err := scopes.Resolve(ctx, 7, position); err != nil || scopeID != "plant" {
		t.Fatalf("after detach = %q, %v", scopeID, err)
	}
	for name, subject := range map[string]model.ObjectRef{
		"responsibility assignment": {Kind: model.KindAssignment, ID: "ASGN-ALEX-CREDIT-APPROVAL"},
		"human":                     {Kind: model.KindHumanIdentity, ID: "HUMAN-JORDAN-KIM"},
	} {
		if _, err := scopes.Resolve(ctx, 7, subject); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v", name, err)
		}
	}
	if _, err := scopes.Resolve(ctx, 7, model.ObjectRef{Kind: model.KindPosition, ID: "POS-NOWHERE"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown position = %v", err)
	}
}
