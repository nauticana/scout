package isolation

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type budgetQueryFake struct {
	rows  map[string][][]any
	args  map[string][]any
	names []string
}

func (query *budgetQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if query.args == nil {
		query.args = make(map[string][]any)
	}
	query.args[name] = append([]any(nil), args...)
	query.names = append(query.names, name)
	return &keelmodel.QueryResult{Rows: query.rows[name]}, nil
}

func (*budgetQueryFake) GenID() int64                   { return 0 }
func (*budgetQueryFake) Commit(context.Context) error   { return nil }
func (*budgetQueryFake) Rollback(context.Context) error { return nil }

func (query *budgetQueryFake) ran(name string) bool {
	_, called := query.args[name]
	return called
}

type budgetDBFake struct {
	keelport.DatabaseRepository
	query *budgetQueryFake
}

func (db budgetDBFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.query
}

func (db budgetDBFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.query, nil
}

// dbNow is the database clock every fake turn-state row reports.
var dbNow = time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC)

// turnRows answers the pin of a nonterminal turn of agent "writer" at release "3".
func turnRows(rows map[string][][]any) map[string][][]any {
	if rows == nil {
		rows = map[string][][]any{}
	}
	if _, set := rows[qBudgetTurnPin]; !set {
		rows[qBudgetTurnPin] = [][]any{{"writer", "3"}}
	}
	if _, set := rows[qGetTurnState]; !set {
		rows[qGetTurnState] = [][]any{{false, dbNow}}
	}
	return rows
}

// usdCatalog holds only USD at two decimals.
func usdCatalog() *fake.CurrencyCatalog {
	return &fake.CurrencyCatalog{Exponents: map[string]int{"USD": 2}}
}

func newBudgetLedger(query *budgetQueryFake) *BudgetLedger {
	query.rows = turnRows(query.rows)
	return &BudgetLedger{DB: budgetDBFake{query: query}, Policy: usdBudget, Releases: &fake.PrincipalLimits{}, Currencies: usdCatalog()}
}

func withBudget(ledger *BudgetLedger, budget domain.ReleaseBudget, read *domain.Principal) *BudgetLedger {
	ledger.Releases = &fake.PrincipalLimits{BudgetFunc: func(_ context.Context, principal domain.Principal) (domain.ReleaseBudget, error) {
		if read != nil {
			*read = principal
		}
		return budget, nil
	}}
	return ledger
}

func storedBudget(id, request, status string, tokens, cost, attempt int64, expires any, settledTokens, settledCost any, expired bool) []any {
	return []any{id, request, status, tokens, cost, "USD", expires, settledTokens, settledCost, attempt, expired}
}

type budgetPolicyFunc func(context.Context, int64) (domain.BudgetLimits, error)

func (f budgetPolicyFunc) BudgetFor(ctx context.Context, tenantID int64) (domain.BudgetLimits, error) {
	return f(ctx, tenantID)
}

var usdBudget = budgetPolicyFunc(func(context.Context, int64) (domain.BudgetLimits, error) {
	return domain.BudgetLimits{WindowTokens: 10_000, WindowCostMinorUnits: 500, Currency: "USD", Window: time.Hour}, nil
})

func usd(tokens, cost int64) domain.BudgetRequest {
	return domain.BudgetRequest{TenantID: 8, RequestID: "req", Tokens: tokens, CostMinorUnits: cost, Currency: "USD"}
}

func two() *int { value := 2; return &value }

func dayUSD(tokens, cost int64) domain.ReleaseBudget {
	return domain.ReleaseBudget{Tokens: tokens, CostMinorUnits: cost, Currency: "USD", Exponent: two(), Source: domain.BudgetSourceRelease,
		Period: domain.BudgetPeriod{Kind: domain.BudgetDaily, Timezone: "Europe/Berlin"}}
}

func TestBudgetLedgerReserveGrantsAndDenies(t *testing.T) {
	expires := time.Now().Add(time.Minute)
	query := &budgetQueryFake{rows: map[string][][]any{qReserveBudget: {{"any", expires}}}}
	ledger := newBudgetLedger(query)

	reservation, err := ledger.Reserve(context.Background(), domain.BudgetRequest{TenantID: 8, RequestID: "req-1", Tokens: 1_000, CostMinorUnits: 50, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if reservation.TenantID != 8 || reservation.RequestID != "req-1" || reservation.Attempt != 1 || reservation.GrantedTokens != 1_000 || reservation.ReservationID != "any" || !reservation.ExpiresAt.Equal(expires) {
		t.Fatalf("reservation = %+v", reservation)
	}
	// An empty principal is charged to the turn's conversation agent.
	args := query.args[qReserveBudget]
	if len(args) != 19 || args[0] != int64(8) || args[3] != "agent" || args[4] != "writer" || args[5] != int64(1) || args[13] != int64(10_000) || args[18] != int64(500) {
		t.Fatalf("reserve args = %v", args)
	}
	if pin := query.args[qBudgetTurnPin]; pin[0] != int64(8) || pin[1] != "req-1" {
		t.Fatalf("pin args = %v", pin)
	}
	if query.ran(qPrincipalPeriodUse) || len(ledger.Currencies.(*fake.CurrencyCatalog).Reads) != 0 {
		t.Fatalf("an unbounded release budget read %v", query.names)
	}

	// No returned row means the tenant window denied the hold.
	denied := newBudgetLedger(&budgetQueryFake{})
	if _, err = denied.Reserve(context.Background(), usd(1_000, 50)); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("denied = %v", err)
	}
}

func TestBudgetLedgerChargesOnlyTheTurnsAgent(t *testing.T) {
	var read domain.Principal
	query := &budgetQueryFake{rows: map[string][][]any{qReserveBudget: {{"r", time.Now().Add(time.Minute)}}}}
	ledger := withBudget(newBudgetLedger(query), domain.ReleaseBudget{}, &read)
	named := usd(10, 2)
	named.Principal = domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer"}
	if _, err := ledger.Reserve(context.Background(), named); err != nil {
		t.Fatal(err)
	}
	if read.Kind != domain.PrincipalAgent || read.ID != "writer" || read.TenantID != 8 || read.Release != "3" {
		t.Fatalf("budget read for %+v, want the conversation's pinned release", read)
	}
	for name, principal := range map[string]domain.PrincipalRef{
		"another agent": {Kind: domain.PrincipalAgent, ID: "spender"},
		"a person":      {Kind: domain.PrincipalHuman, ID: "writer"},
	} {
		query := &budgetQueryFake{rows: map[string][][]any{qReserveBudget: {{"r", time.Now()}}}}
		request := usd(10, 2)
		request.Principal = principal
		if _, err := newBudgetLedger(query).Reserve(context.Background(), request); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("%s: error = %v, want forbidden", name, err)
		}
		if query.ran(qReserveBudget) || query.ran(qLockBudget) {
			t.Errorf("%s: a foreign principal reached %v", name, query.names)
		}
	}
	// Another tenant's request does not pin.
	foreign := &budgetQueryFake{rows: map[string][][]any{qBudgetTurnPin: nil}}
	if _, err := newBudgetLedger(foreign).Reserve(context.Background(), usd(10, 2)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign request = %v, want not found", err)
	}
}

func TestBudgetLedgerRequiresReleaseLimits(t *testing.T) {
	ledger := newBudgetLedger(&budgetQueryFake{})
	ledger.Releases = nil
	_, err := ledger.Reserve(context.Background(), usd(10, 2))
	if err == nil || errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing release limits = %v, want a configuration error", err)
	}
}

func TestBudgetLedgerCountsTheCurrentPeriodByTheDatabaseClock(t *testing.T) {
	query := &budgetQueryFake{rows: map[string][][]any{
		qReserveBudget:      {{"r", time.Now().Add(time.Minute)}},
		qPrincipalPeriodUse: {{int64(100), int64(10), int64(50), int64(5)}},
	}}
	ledger := withBudget(newBudgetLedger(query), dayUSD(1_000, 100), nil)
	if _, err := ledger.Reserve(context.Background(), usd(100, 10)); err != nil {
		t.Fatal(err)
	}
	args := query.args[qPrincipalPeriodUse]
	berlinDay := time.Date(2026, 3, 28, 23, 0, 0, 0, time.UTC)
	if len(args) != 6 || args[0] != "USD" || args[1] != "USD" || args[2] != int64(8) || args[3] != "agent" || args[4] != "writer" ||
		!args[5].(time.Time).Equal(berlinDay) || args[5].(time.Time).Location() != time.UTC {
		t.Fatalf("period use args = %v", args)
	}
	if reads := ledger.Currencies.(*fake.CurrencyCatalog).Reads; len(reads) != 1 || reads[0] != "USD" {
		t.Fatalf("unit check read %v", reads)
	}

	rolling := domain.ReleaseBudget{Tokens: 1_000, Source: domain.BudgetSourceRelease, Period: domain.BudgetPeriod{Kind: domain.BudgetRolling, Window: time.Hour}}
	query.rows[qPrincipalPeriodUse] = [][]any{{int64(0), int64(0), int64(0), int64(0)}}
	if _, err := withBudget(newBudgetLedger(query), rolling, nil).Reserve(context.Background(), usd(100, 10)); err != nil {
		t.Fatal(err)
	}
	if start := query.args[qPrincipalPeriodUse][5].(time.Time); !start.Equal(dbNow.Add(-time.Hour)) {
		t.Fatalf("rolling start = %s", start)
	}
}

func TestBudgetLedgerRefusesAnExhaustedPeriodWithRetryAfter(t *testing.T) {
	query := &budgetQueryFake{rows: map[string][][]any{
		qReserveBudget:      {{"r", time.Now().Add(time.Minute)}},
		qPrincipalPeriodUse: {{int64(900), int64(60), int64(50), int64(20)}},
	}}
	_, err := withBudget(newBudgetLedger(query), dayUSD(1_000, 100), nil).Reserve(context.Background(), usd(100, 10))
	var limit interface{ RetryAfter() time.Duration }
	if !errors.Is(err, domain.ErrBudgetExceeded) || !errors.As(err, &limit) || limit.RetryAfter() != 10*time.Hour {
		t.Fatalf("exhausted day = %v, want budget exceeded until the Berlin midnight", err)
	}
	if query.ran(qReserveBudget) {
		t.Fatal("an exhausted period inserted a hold")
	}
	rolling := domain.ReleaseBudget{CostMinorUnits: 80, Currency: "USD", Exponent: two(), Source: domain.BudgetSourceRelease,
		Period: domain.BudgetPeriod{Kind: domain.BudgetRolling, Window: time.Hour}}
	_, err = withBudget(newBudgetLedger(query), rolling, nil).Reserve(context.Background(), usd(100, 1))
	if !errors.Is(err, domain.ErrBudgetExceeded) || !errors.As(err, &limit) || limit.RetryAfter() != 0 {
		t.Fatalf("exhausted rolling window = %v, want no retry hint", err)
	}
	// Sums near the int64 limit refuse instead of wrapping.
	query.rows[qPrincipalPeriodUse] = [][]any{{int64(1<<63 - 1), int64(0), int64(1<<63 - 1), int64(0)}}
	huge := domain.ReleaseBudget{Tokens: 1<<63 - 1, Source: domain.BudgetSourceRelease, Period: domain.BudgetPeriod{Kind: domain.BudgetRolling, Window: time.Hour}}
	if _, err = withBudget(newBudgetLedger(query), huge, nil).Reserve(context.Background(), usd(1, 0)); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("overflowing sums = %v, want budget exceeded", err)
	}
}

func TestBudgetLedgerFailsClosedOnAnUnenforceableBudget(t *testing.T) {
	jpy := dayUSD(0, 100)
	jpy.Currency = "JPY"
	noCurrency := dayUSD(0, 100)
	noCurrency.Currency = ""
	cases := map[domain.BudgetProblem]struct {
		budget  domain.ReleaseBudget
		catalog map[string]int
	}{
		domain.BudgetInvalidPeriod:    {budget: domain.ReleaseBudget{Tokens: 10, Source: domain.BudgetSourceFallback, Period: domain.BudgetPeriod{Kind: domain.BudgetDaily, Timezone: "Mars/Olympus"}}},
		domain.BudgetNoCurrency:       {budget: noCurrency},
		domain.BudgetCurrencyMismatch: {budget: jpy},
		domain.BudgetUnknownCurrency:  {budget: dayUSD(0, 100), catalog: map[string]int{}},
		domain.BudgetExponentChanged:  {budget: dayUSD(0, 100), catalog: map[string]int{"USD": 3}},
	}
	for problem, tc := range cases {
		query := &budgetQueryFake{rows: map[string][][]any{qReserveBudget: {{"r", time.Now()}}}}
		ledger := withBudget(newBudgetLedger(query), tc.budget, nil)
		if tc.catalog != nil {
			ledger.Currencies = &fake.CurrencyCatalog{Exponents: tc.catalog}
		}
		_, err := ledger.Reserve(context.Background(), usd(10, 2))
		var unenforceable *domain.BudgetUnenforceableError
		if !errors.As(err, &unenforceable) || unenforceable.Problem != problem || !errors.Is(err, domain.ErrDegraded) {
			t.Errorf("%s: error = %v", problem, err)
		}
		if query.ran(qReserveBudget) {
			t.Errorf("%s: an unenforceable budget inserted a hold", problem)
		}
	}
}

func TestBudgetLedgerClampsAGovernedTurnToItsPerTurnCeiling(t *testing.T) {
	perTurn := domain.ReleaseBudget{Tokens: 500, CostMinorUnits: 20, Currency: "USD", Exponent: two(), Source: domain.BudgetSourceRelease,
		Period: domain.BudgetPeriod{Kind: domain.BudgetPerTurn}}
	query := &budgetQueryFake{rows: map[string][][]any{qReserveBudget: {{"r", time.Now().Add(time.Minute)}}}}
	governed := usd(1_000, 50)
	governed.TurnGoverned = true
	reservation, err := withBudget(newBudgetLedger(query), perTurn, nil).Reserve(context.Background(), governed)
	if err != nil || reservation.GrantedTokens != 500 || reservation.GrantedCostMinorUnits != 20 {
		t.Fatalf("governed = %+v, %v", reservation, err)
	}
	if args := query.args[qReserveBudget]; args[6] != int64(500) || args[7] != int64(20) || query.ran(qPrincipalPeriodUse) {
		t.Fatalf("reserve args = %v, queries = %v", args, query.names)
	}
	// The ingress hold and the runtime's replay ask the same governed request.
	replay := &budgetQueryFake{rows: map[string][][]any{
		qFindBudget: {storedBudget("r", "req", string(domain.ReservationHeld), 500, 20, 1, time.Now().Add(time.Minute), nil, nil, false)},
	}}
	if again, err := withBudget(newBudgetLedger(replay), perTurn, nil).Reserve(context.Background(), governed); err != nil || again.ReservationID != "r" || replay.ran(qReserveBudget) {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	ungoverned := &budgetQueryFake{rows: map[string][][]any{qReserveBudget: {{"r", time.Now()}}}}
	if _, err = withBudget(newBudgetLedger(ungoverned), perTurn, nil).Reserve(context.Background(), usd(1_000, 5)); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("ungoverned = %v, want budget exceeded", err)
	}
	if ungoverned.ran(qReserveBudget) {
		t.Fatal("an estimate above the per-turn ceiling was held")
	}
}

func TestBudgetLedgerReplaysALiveHoldButRechecksAnExpiredAttempt(t *testing.T) {
	exhausted := [][]any{{int64(1_000), int64(0), int64(0), int64(0)}}
	live := &budgetQueryFake{rows: map[string][][]any{
		qFindBudget:         {storedBudget("r", "req", string(domain.ReservationHeld), 10, 2, 1, time.Now().Add(time.Minute), nil, nil, false)},
		qPrincipalPeriodUse: exhausted,
	}}
	replayed := withBudget(newBudgetLedger(live), dayUSD(1_000, 0), nil)
	if reservation, err := replayed.Reserve(context.Background(), usd(10, 2)); err != nil || reservation.ReservationID != "r" {
		t.Fatalf("live replay = %+v, %v", reservation, err)
	}
	expired := &budgetQueryFake{rows: map[string][][]any{
		qFindBudget:         {storedBudget("old", "req", string(domain.ReservationHeld), 10, 2, 1, time.Now().Add(-time.Minute), nil, nil, true)},
		qExpireOneBudget:    {{"old"}},
		qPrincipalPeriodUse: exhausted,
		qReserveBudget:      {{"new", time.Now().Add(time.Minute)}},
	}}
	if _, err := withBudget(newBudgetLedger(expired), dayUSD(1_000, 0), nil).Reserve(context.Background(), usd(10, 2)); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("renewed attempt in an exhausted day = %v", err)
	}
	if live.ran(qPrincipalPeriodUse) || len(replayed.Currencies.(*fake.CurrencyCatalog).Reads) != 0 {
		t.Fatalf("a live replay re-ran admission: %v", live.names)
	}
}

// Abandon and cancellation recover a live hold to release it, even once its budget cannot admit
// a new attempt; only a new attempt is refused.
func TestBudgetLedgerRecoversALiveHoldOfAnUnenforceableBudget(t *testing.T) {
	invalidPeriod := domain.ReleaseBudget{Tokens: 1_000, Source: domain.BudgetSourceRelease, Period: domain.BudgetPeriod{Kind: domain.BudgetRolling}}
	for name, tc := range map[string]struct {
		budget  domain.ReleaseBudget
		catalog map[string]int
	}{
		"exponent drift": {budget: dayUSD(0, 100), catalog: map[string]int{"USD": 3}},
		"unknown":        {budget: dayUSD(0, 100), catalog: map[string]int{}},
		"invalid period": {budget: invalidPeriod},
	} {
		live := &budgetQueryFake{rows: map[string][][]any{
			qFindBudget: {storedBudget("r", "req", string(domain.ReservationHeld), 10, 2, 1, time.Now().Add(time.Minute), nil, nil, false)},
		}}
		ledger := withBudget(newBudgetLedger(live), tc.budget, nil)
		ledger.Currencies = &fake.CurrencyCatalog{Exponents: tc.catalog}
		if reservation, err := ledger.Reserve(context.Background(), usd(10, 2)); err != nil || reservation.ReservationID != "r" {
			t.Errorf("%s: live replay = %+v, %v", name, reservation, err)
		}
		renewed := &budgetQueryFake{rows: map[string][][]any{
			qFindBudget:    {storedBudget("old", "req", string(domain.ReservationExpired), 10, 2, 1, time.Now().Add(-time.Minute), int64(0), int64(0), false)},
			qReserveBudget: {{"new", time.Now().Add(time.Minute)}},
		}}
		ledger = withBudget(newBudgetLedger(renewed), tc.budget, nil)
		ledger.Currencies = &fake.CurrencyCatalog{Exponents: tc.catalog}
		if _, err := ledger.Reserve(context.Background(), usd(10, 2)); !errors.Is(err, domain.ErrDegraded) {
			t.Errorf("%s: new attempt = %v, want degraded", name, err)
		}
		if renewed.ran(qReserveBudget) {
			t.Errorf("%s: an unenforceable budget inserted a new attempt", name)
		}
	}
}

// A zero cost hold against a period cost ceiling would let any number of concurrent turns
// spend past it, so it is refused even with room left.
func TestBudgetLedgerRefusesAZeroCostHoldAgainstAPeriodCostCeiling(t *testing.T) {
	query := &budgetQueryFake{rows: map[string][][]any{
		qReserveBudget:      {{"r", time.Now().Add(time.Minute)}},
		qPrincipalPeriodUse: {{int64(0), int64(0), int64(0), int64(0)}},
	}}
	_, err := withBudget(newBudgetLedger(query), dayUSD(0, 500), nil).Reserve(context.Background(), usd(100, 0))
	var unenforceable *domain.BudgetUnenforceableError
	if !errors.As(err, &unenforceable) || unenforceable.Problem != domain.BudgetNoCostQuote || !errors.Is(err, domain.ErrDegraded) {
		t.Fatalf("zero cost hold = %v, want no_cost_quote", err)
	}
	if query.ran(qReserveBudget) {
		t.Fatal("a zero cost hold was inserted")
	}
	// A tokens-only budget still admits a zero cost hold.
	if _, err = withBudget(newBudgetLedger(query), dayUSD(1_000, 0), nil).Reserve(context.Background(), usd(100, 0)); err != nil {
		t.Fatalf("tokens-only day = %v", err)
	}
}

func TestBudgetLedgerSpentAxisAdmitsNothing(t *testing.T) {
	for _, tc := range []struct {
		used, grant, ceiling int64
		want                 bool
	}{
		{used: 500, grant: 0, ceiling: 500, want: false},
		{used: 501, grant: 0, ceiling: 500, want: false},
		{used: 499, grant: 1, ceiling: 500, want: true},
		{used: 499, grant: 2, ceiling: 500, want: false},
		{used: 0, grant: 500, ceiling: 500, want: true},
		{used: 900, grant: 900, ceiling: 0, want: true},
	} {
		if got := fits(tc.used, tc.grant, tc.ceiling); got != tc.want {
			t.Errorf("fits(%d, %d, %d) = %v, want %v", tc.used, tc.grant, tc.ceiling, got, tc.want)
		}
	}
}

// A hold larger than the whole period ceiling never fits, so it carries no Retry-After.
func TestBudgetLedgerRefusesAHoldLargerThanThePeriodWithoutRetryAfter(t *testing.T) {
	for name, tc := range map[string]struct {
		budget       domain.ReleaseBudget
		tokens, cost int64
	}{
		"tokens": {budget: dayUSD(10_000, 0), tokens: 20_000, cost: 1},
		"cost":   {budget: dayUSD(0, 100), tokens: 10, cost: 101},
	} {
		query := &budgetQueryFake{rows: map[string][][]any{
			qReserveBudget:      {{"r", time.Now().Add(time.Minute)}},
			qPrincipalPeriodUse: {{int64(0), int64(0), int64(0), int64(0)}},
		}}
		_, err := withBudget(newBudgetLedger(query), tc.budget, nil).Reserve(context.Background(), usd(tc.tokens, tc.cost))
		var limit interface{ RetryAfter() time.Duration }
		if !errors.Is(err, domain.ErrBudgetExceeded) || errors.As(err, &limit) {
			t.Errorf("%s: oversized hold = %v, want budget exceeded without a retry hint", name, err)
		}
		if query.ran(qReserveBudget) || query.ran(qPrincipalPeriodUse) {
			t.Errorf("%s: an oversized hold reached %v", name, query.names)
		}
	}
}

func TestBudgetLedgerSpendReportsTheDatabaseClockPeriod(t *testing.T) {
	query := &budgetQueryFake{rows: map[string][][]any{
		qBudgetClock:        {{dbNow}},
		qPrincipalPeriodUse: {{int64(100), int64(10), int64(50), int64(5)}},
	}}
	ledger := newBudgetLedger(query)
	agent := domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer"}
	spend, err := ledger.Spend(context.Background(), 8, agent, dayUSD(1_000, 100))
	if err != nil {
		t.Fatal(err)
	}
	want := domain.BudgetSpend{
		PeriodStart: time.Date(2026, 3, 28, 23, 0, 0, 0, time.UTC), PeriodEnd: time.Date(2026, 3, 29, 22, 0, 0, 0, time.UTC),
		Tokens: 100, CostMinorUnits: 10, HeldTokens: 50, HeldCostMinorUnits: 5, Currency: "USD", Exponent: 2,
	}
	if spend != want {
		t.Fatalf("spend = %+v, want %+v", spend, want)
	}
	rolling := domain.ReleaseBudget{Tokens: 10, Source: domain.BudgetSourceRelease, Period: domain.BudgetPeriod{Kind: domain.BudgetRolling, Window: time.Hour}}
	if spend, err = ledger.Spend(context.Background(), 8, agent, rolling); err != nil || !spend.PeriodEnd.Equal(dbNow) || !spend.PeriodStart.Equal(dbNow.Add(-time.Hour)) {
		t.Fatalf("rolling spend = %+v, %v", spend, err)
	}
	if _, err = ledger.Spend(context.Background(), 8, agent, domain.ReleaseBudget{Tokens: 1, Period: domain.BudgetPeriod{Kind: domain.BudgetPerTurn}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("turn spend = %v", err)
	}
	if _, err = ledger.Spend(context.Background(), 0, agent, rolling); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no tenant = %v", err)
	}
}

func TestBudgetLedgerReserveValidation(t *testing.T) {
	ledger := newBudgetLedger(&budgetQueryFake{})
	if _, err := ledger.Reserve(context.Background(), domain.BudgetRequest{TenantID: 0, RequestID: "", Tokens: 0, CostMinorUnits: -1, Currency: "x"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid = %v", err)
	}
	if _, err := ledger.Reserve(context.Background(), domain.BudgetRequest{TenantID: 8, RequestID: "req", Tokens: 10, CostMinorUnits: 1, Currency: "EUR"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("currency mismatch = %v", err)
	}
}

func TestBudgetLedgerReserveIsIdempotentByRequest(t *testing.T) {
	expires := time.Now().Add(time.Minute)
	existing := storedBudget("reservation", "req", string(domain.ReservationHeld), 10, 2, 1, expires.Format(time.RFC3339), nil, nil, false)
	query := &budgetQueryFake{rows: map[string][][]any{qFindBudget: {existing}}}
	reservation, err := newBudgetLedger(query).Reserve(context.Background(), usd(10, 2))
	if err != nil || reservation.ReservationID != "reservation" || reservation.Attempt != 1 || !reservation.ExpiresAt.Equal(expires.Truncate(time.Second)) || !reservation.Replayed {
		t.Fatalf("replay = %+v, %v", reservation, err)
	}
	if query.ran(qReserveBudget) {
		t.Fatal("replay inserted a second reservation")
	}
}

func TestBudgetLedgerReserveClassifiesSettledReplay(t *testing.T) {
	existing := storedBudget("reservation", "req", string(domain.ReservationSettled), 10, 2, 1, time.Now(), int64(8), int64(1), false)
	query := &budgetQueryFake{rows: map[string][][]any{qFindBudget: {existing}}}
	if _, err := newBudgetLedger(query).Reserve(context.Background(), usd(10, 2)); !errors.Is(err, domain.ErrBudgetSettled) {
		t.Fatalf("settled replay = %v", err)
	}
}

func TestBudgetLedgerRenewsExpiredReservation(t *testing.T) {
	expires := time.Now().Add(-time.Minute)
	existing := storedBudget("old", "req", string(domain.ReservationHeld), 10, 2, 1, expires, nil, nil, true)
	newExpiry := time.Now().Add(time.Minute)
	query := &budgetQueryFake{rows: map[string][][]any{
		qFindBudget:      {existing},
		qExpireOneBudget: {{"old"}},
		qReserveBudget:   {{"new", newExpiry}},
	}}
	reservation, err := newBudgetLedger(query).Reserve(context.Background(), usd(10, 2))
	if err != nil || reservation.ReservationID != "new" || reservation.Attempt != 2 || reservation.Replayed {
		t.Fatalf("renewed = %+v, %v", reservation, err)
	}
	if args := query.args[qReserveBudget]; len(args) != 19 || args[5] != int64(2) {
		t.Fatalf("reserve args = %v", args)
	}
}

func TestBudgetLedgerRenewsAttemptExpiredBySweeper(t *testing.T) {
	expires := time.Now().Add(-time.Minute)
	query := &budgetQueryFake{rows: map[string][][]any{
		qFindBudget:    {storedBudget("old", "req", string(domain.ReservationHeld), 10, 2, 1, expires, nil, nil, true)},
		qGetBudget:     {storedBudget("old", "req", string(domain.ReservationExpired), 10, 2, 1, expires, int64(0), int64(0), false)},
		qReserveBudget: {{"new", time.Now().Add(time.Minute)}},
	}}
	reservation, err := newBudgetLedger(query).Reserve(context.Background(), usd(10, 2))
	if err != nil || reservation.Attempt != 2 {
		t.Fatalf("renewed after sweep = %+v, %v", reservation, err)
	}
}

func TestBudgetLedgerDoesNotRenewTerminalTurn(t *testing.T) {
	query := &budgetQueryFake{rows: map[string][][]any{qGetTurnState: {{true, dbNow}}}}
	if _, err := newBudgetLedger(query).Reserve(context.Background(), usd(10, 2)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("terminal turn = %v", err)
	}
}

func TestBudgetLedgerCommitAndRelease(t *testing.T) {
	current := storedBudget("r", "req", string(domain.ReservationHeld), 1_000, 50, 1, time.Now().Add(time.Minute), nil, nil, false)
	query := &budgetQueryFake{rows: map[string][][]any{qGetBudget: {current}, qSettleBudget: {{"r"}}, qReleaseBudget: {{"r"}}}}
	ledger := newBudgetLedger(query)
	reservation := domain.BudgetReservation{TenantID: 8, ReservationID: "r", Currency: "USD", GrantedTokens: 1_000, GrantedCostMinorUnits: 50}

	usage := domain.Usage{InputTokens: 100, OutputTokens: 50, CostMinorUnits: 30, Currency: "USD"}
	if err := ledger.Commit(context.Background(), reservation, usage); err != nil {
		t.Fatal(err)
	}
	args := query.args[qSettleBudget]
	if len(args) != 4 || args[0] != int64(150) || args[1] != int64(30) || args[2] != int64(8) || args[3] != "r" {
		t.Fatalf("settle args = %v", args)
	}
	if err := ledger.Release(context.Background(), reservation); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Commit(context.Background(), reservation, domain.Usage{InputTokens: 2_000, CostMinorUnits: 100, Currency: "USD"}); err != nil {
		t.Fatalf("overage = %v", err)
	}
	if args := query.args[qSettleBudget]; len(args) != 4 || args[0] != int64(2_000) || args[1] != int64(100) {
		t.Fatalf("overage settle args = %v", args)
	}

	// Repeating the same terminal transition is idempotent.
	settled := storedBudget("r", "req", string(domain.ReservationSettled), 1_000, 50, 1, time.Now(), int64(150), int64(30), false)
	replayed := newBudgetLedger(&budgetQueryFake{rows: map[string][][]any{qGetBudget: {settled}}})
	if err := replayed.Commit(context.Background(), reservation, usage); err != nil {
		t.Fatalf("double settle = %v", err)
	}
	if err := replayed.Commit(context.Background(), reservation, domain.Usage{Currency: "EUR"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("currency = %v", err)
	}
}

// catalogTx is a turn's ending transaction that can bind the ledger's queries, recording which catalog it bound.
type catalogTx struct {
	*budgetQueryFake
	bound []string
}

func (tx *catalogTx) QueryService(catalogID string, _ map[string]string) keelport.QueryService {
	tx.bound = append(tx.bound, catalogID)
	return tx.budgetQueryFake
}

// Decision 18: a turn's settlement joins the transaction that ends the turn, after its guarded
// touch; it takes the tenant lock and moves the reservation, and never begins or commits itself.
func TestBudgetLedgerSettlesATurnInsideTheTransactionThatEndsIt(t *testing.T) {
	current := storedBudget("r", "req", string(domain.ReservationHeld), 1_000, 50, 1, time.Now().Add(time.Minute), nil, nil, false)
	reservation := domain.BudgetReservation{TenantID: 8, ReservationID: "r", RequestID: "req", Currency: "USD"}
	usage := domain.Usage{InputTokens: 10, OutputTokens: 5, CostMinorUnits: 7, Currency: "USD"}
	ledger := newBudgetLedger(&budgetQueryFake{})

	tx := &catalogTx{budgetQueryFake: &budgetQueryFake{rows: map[string][][]any{qGetBudget: {current}, qSettleBudget: {{"r"}}}}}
	if err := ledger.SettleTurnTx(context.Background(), tx, reservation, usage, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tx.names, []string{qLockBudget, qGetBudget, qSettleBudget}) || !slices.Equal(tx.bound, []string{budgetCatalogID}) {
		t.Fatalf("settlement ran %v in catalogs %v", tx.names, tx.bound)
	}
	if args := tx.args[qSettleBudget]; args[0] != int64(15) || args[1] != int64(7) {
		t.Fatalf("settled at %v", args)
	}

	refund := &catalogTx{budgetQueryFake: &budgetQueryFake{rows: map[string][][]any{qGetBudget: {current}, qReleaseBudget: {{"r"}}}}}
	if err := ledger.SettleTurnTx(context.Background(), refund, reservation, domain.Usage{}, true); err != nil || !refund.ran(qReleaseBudget) || refund.ran(qSettleBudget) {
		t.Fatalf("refund = %v, ran %v", err, refund.names)
	}

	expired := storedBudget("r", "req", string(domain.ReservationHeld), 1_000, 50, 1, time.Now().Add(-time.Minute), nil, nil, true)
	lapsed := &catalogTx{budgetQueryFake: &budgetQueryFake{rows: map[string][][]any{qGetBudget: {expired}, qExpireOneBudget: {{"r"}}}}}
	if err := ledger.SettleTurnTx(context.Background(), lapsed, reservation, usage, false); !errors.Is(err, domain.ErrConflict) || lapsed.ran(qSettleBudget) {
		t.Fatalf("a lapsed hold = %v, ran %v", err, lapsed.names)
	}
	settled := storedBudget("r", "req", string(domain.ReservationSettled), 1_000, 50, 1, time.Now(), int64(15), int64(7), false)
	replay := &catalogTx{budgetQueryFake: &budgetQueryFake{rows: map[string][][]any{qGetBudget: {settled}}}}
	if err := ledger.SettleTurnTx(context.Background(), replay, reservation, usage, false); err != nil || replay.ran(qSettleBudget) {
		t.Fatalf("a repeated settlement = %v, ran %v", err, replay.names)
	}

	if err := ledger.SettleTurnTx(context.Background(), &budgetQueryFake{}, reservation, usage, false); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a transaction without a query catalog = %v, want ErrNotReady", err)
	}
	if err := ledger.SettleTurnTx(context.Background(), tx, domain.BudgetReservation{}, usage, false); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no reservation = %v", err)
	}
	if err := ledger.SettleTurnTx(context.Background(), tx, reservation, domain.Usage{Currency: "EUR"}, false); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("another currency = %v", err)
	}
	if strings.Contains(strings.Join(slices.Collect(maps.Values(budgetQueries)), "\n"), "conversation_turn SET fence") {
		t.Fatal("the ledger must leave the guarded touch to the transaction that ends the turn")
	}
}

func TestBudgetLedgerRejectsInvalidExpiryEncoding(t *testing.T) {
	query := &budgetQueryFake{rows: map[string][][]any{qReserveBudget: {{"r", "not-a-time"}}}}
	if _, err := newBudgetLedger(query).Reserve(context.Background(), usd(10, 2)); err == nil {
		t.Fatal("invalid expiry must fail")
	}
}

func TestBudgetLedgerFencesExpiredSettlement(t *testing.T) {
	current := storedBudget("r", "req", string(domain.ReservationHeld), 100, 10, 1, time.Now().Add(-time.Minute), nil, nil, true)
	query := &budgetQueryFake{rows: map[string][][]any{qGetBudget: {current}, qExpireOneBudget: {{"r"}}}}
	reservation := domain.BudgetReservation{TenantID: 8, ReservationID: "r", Currency: "USD"}
	err := newBudgetLedger(query).Commit(context.Background(), reservation, domain.Usage{CostMinorUnits: 1, Currency: "USD"})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired commit = %v", err)
	}
	if query.ran(qSettleBudget) {
		t.Fatal("expired reservation settled")
	}
}

func TestBudgetLedgerExpireIsBounded(t *testing.T) {
	query := &budgetQueryFake{rows: map[string][][]any{qExpireBudget: {{"a"}, {"b"}}}}
	ledger := newBudgetLedger(query)
	expired, err := ledger.Expire(context.Background(), 100)
	if err != nil || expired != 2 {
		t.Fatalf("expired = %d, %v", expired, err)
	}
	if args := query.args[qExpireBudget]; len(args) != 1 || args[0] != 100 {
		t.Fatalf("expire args = %v", args)
	}
	if _, err := ledger.Expire(context.Background(), 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("limit = %v", err)
	}
}
