package isolation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keeldata "github.com/nauticana/keel/data"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

const (
	qLockBudget      = "scout_isolation_lock_budget"
	qGetTurnState    = "scout_isolation_get_turn_state"
	qFindBudget      = "scout_isolation_find_budget"
	qGetBudget       = "scout_isolation_get_budget"
	qReserveBudget   = "scout_isolation_reserve_budget"
	qSettleBudget    = "scout_isolation_settle_budget"
	qReleaseBudget   = "scout_isolation_release_budget"
	qExpireOneBudget = "scout_isolation_expire_one_budget"
	qExpireBudget    = "scout_isolation_expire_budget"
	qGuardTurnOwner  = "scout_isolation_guard_turn_owner"
)

// Window accounting: held reservations count at their grant until they expire,
// settled reservations count at actual usage for one budget window. The turn's agent is
// counted against its release budget's period the same way; both must hold.
var budgetQueries = common.MergeMaps(scope.CurrencyQueries, agentBudgetQueries, map[string]string{
	qLockBudget: "SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
	// The guarded touch of the settling owner's turn row: it matches only while the owner's fence
	// holds, and its row lock orders the settlement before or after a hold of the turn.
	qGuardTurnOwner: `
UPDATE conversation_turn SET fence = fence
 WHERE partner_id = ? AND request_id = ? AND fence = ? AND status_code IN (` + sqlTurnOwned + `)
RETURNING turn_no`,
	qGetTurnState: `
SELECT status.is_terminal, CURRENT_TIMESTAMP
  FROM conversation_turn turn_row
  JOIN turn_status status ON status.code = turn_row.status_code
 WHERE turn_row.partner_id = ? AND turn_row.request_id = ?`,
	qFindBudget: `
SELECT reservation_id, request_id, status_code, granted_tokens,
       granted_cost_minor_units, currency_code, expires_at,
       settled_tokens, settled_cost_minor_units, attempt_no,
       status_code = ` + sqlReservationHeld + ` AND expires_at <= CURRENT_TIMESTAMP
  FROM budget_reservation
 WHERE partner_id = ? AND request_id = ?
 ORDER BY attempt_no DESC
 LIMIT 1`,
	qGetBudget: `
SELECT reservation_id, request_id, status_code, granted_tokens,
       granted_cost_minor_units, currency_code, expires_at,
       settled_tokens, settled_cost_minor_units, attempt_no,
       status_code = ` + sqlReservationHeld + ` AND expires_at <= CURRENT_TIMESTAMP
  FROM budget_reservation
 WHERE partner_id = ? AND reservation_id = ?`,
	qReserveBudget: `
INSERT INTO budget_reservation (partner_id, reservation_id, request_id, principal_kind, principal_id, attempt_no, status_code,
                                granted_tokens, granted_cost_minor_units, currency_code, expires_at)
SELECT ?, ?, ?, ?, ?, ?, ` + sqlReservationHeld + `, ?, ?, ?, CURRENT_TIMESTAMP + make_interval(secs => ?)
 WHERE (SELECT COALESCE(SUM(CASE WHEN status_code = ` + sqlReservationHeld + ` THEN granted_tokens ELSE settled_tokens END), 0)
          FROM budget_reservation
         WHERE partner_id = ?
           AND (status_code = ` + sqlReservationHeld + ` AND expires_at > CURRENT_TIMESTAMP
             OR status_code = ` + sqlReservationSettled + ` AND settled_at > CURRENT_TIMESTAMP - make_interval(secs => ?))) + ? <= ?
   AND (SELECT COALESCE(SUM(CASE WHEN status_code = ` + sqlReservationHeld + ` THEN granted_cost_minor_units ELSE settled_cost_minor_units END), 0)
          FROM budget_reservation
         WHERE partner_id = ?
           AND currency_code = ?
           AND (status_code = ` + sqlReservationHeld + ` AND expires_at > CURRENT_TIMESTAMP
             OR status_code = ` + sqlReservationSettled + ` AND settled_at > CURRENT_TIMESTAMP - make_interval(secs => ?))) + ? <= ?
RETURNING reservation_id, expires_at`,
	qSettleBudget: `
UPDATE budget_reservation
   SET status_code = ` + sqlReservationSettled + `, settled_at = CURRENT_TIMESTAMP, settled_tokens = ?, settled_cost_minor_units = ?
 WHERE partner_id = ? AND reservation_id = ? AND status_code = ` + sqlReservationHeld + `
   AND expires_at > CURRENT_TIMESTAMP
RETURNING reservation_id`,
	qReleaseBudget: `
UPDATE budget_reservation
   SET status_code = ` + sqlReservationReleased + `, settled_at = CURRENT_TIMESTAMP, settled_tokens = 0, settled_cost_minor_units = 0
 WHERE partner_id = ? AND reservation_id = ? AND status_code = ` + sqlReservationHeld + `
RETURNING reservation_id`,
	qExpireOneBudget: `
UPDATE budget_reservation
   SET status_code = ` + sqlReservationExpired + `, settled_at = CURRENT_TIMESTAMP, settled_tokens = 0, settled_cost_minor_units = 0
 WHERE partner_id = ? AND reservation_id = ? AND status_code = ` + sqlReservationHeld + ` AND expires_at <= CURRENT_TIMESTAMP
RETURNING reservation_id`,
	qExpireBudget: `
UPDATE budget_reservation
   SET status_code = ` + sqlReservationExpired + `, settled_at = CURRENT_TIMESTAMP, settled_tokens = 0, settled_cost_minor_units = 0
 WHERE status_code = ` + sqlReservationHeld + `
   AND (partner_id, reservation_id) IN (SELECT partner_id, reservation_id
                                         FROM budget_reservation
                                        WHERE status_code = ` + sqlReservationHeld + ` AND expires_at <= CURRENT_TIMESTAMP
                                        ORDER BY expires_at
                                        LIMIT ?
                                          FOR UPDATE SKIP LOCKED)
RETURNING reservation_id`,
})

// BudgetLedger is a durable TenantBudgetManager over budget_reservation:
// reserve-then-settle with a lease, so a dead worker's hold expires instead of
// throttling its tenant forever. Every hold is charged to its turn's conversation agent
// and enforces that agent's release budget inside the tenant envelope.
type BudgetLedger struct {
	DB     keelport.DatabaseRepository
	Policy contract.TenantBudgetPolicy
	// Releases reads each agent's frozen budget; required. Pass the data plane's PrincipalLimits.
	Releases contract.PrincipalLimits
	// ReservationTTL bounds how long a hold counts before Expire reclaims it; default 15m.
	ReservationTTL time.Duration

	once sync.Once
	qs   keelport.QueryService
}

var (
	_ contract.TenantBudgetManager = (*BudgetLedger)(nil)
	_ contract.TurnBudgetManager   = (*BudgetLedger)(nil)
)

func (ledger *BudgetLedger) init(ctx context.Context) error {
	if ledger.DB == nil {
		return fmt.Errorf("budget ledger: database is required")
	}
	ledger.once.Do(func() { ledger.qs = ledger.DB.GetQueryService(ctx, budgetQueries) })
	if ledger.qs == nil {
		return fmt.Errorf("budget ledger: query service is required")
	}
	return nil
}

func (ledger *BudgetLedger) ttl() time.Duration {
	if ledger.ReservationTTL == 0 {
		return 15 * time.Minute
	}
	return ledger.ReservationTTL
}

// Reserve atomically holds tokens and cost against the tenant's window budget and the release
// budget of the turn's agent: its per-turn ceiling, or its current period by the database clock.
func (ledger *BudgetLedger) Reserve(ctx context.Context, request domain.BudgetRequest) (domain.BudgetReservation, error) {
	tenantID, tokens, costMinorUnits, currency := request.TenantID, request.Tokens, request.CostMinorUnits, request.Currency
	requestID := strings.TrimSpace(request.RequestID)
	if tenantID <= 0 || requestID == "" || tokens <= 0 || costMinorUnits < 0 || len(currency) != 3 {
		return domain.BudgetReservation{}, fmt.Errorf("%w: tenant, request, positive tokens, cost, and currency are required", domain.ErrValidation)
	}
	if ledger.Policy == nil {
		return domain.BudgetReservation{}, fmt.Errorf("budget ledger: policy is required")
	}
	if ledger.Releases == nil {
		return domain.BudgetReservation{}, fmt.Errorf("budget ledger: release limits are required")
	}
	if err := ledger.init(ctx); err != nil {
		return domain.BudgetReservation{}, err
	}
	limits, err := ledger.Policy.BudgetFor(ctx, tenantID)
	if err != nil {
		return domain.BudgetReservation{}, fmt.Errorf("budget policy for tenant %d: %w", tenantID, err)
	}
	if limits.Window < time.Second || limits.WindowTokens <= 0 || limits.WindowCostMinorUnits < 0 {
		return domain.BudgetReservation{}, fmt.Errorf("%w: tenant budget limits are invalid", domain.ErrValidation)
	}
	if currency != limits.Currency {
		return domain.BudgetReservation{}, fmt.Errorf("%w: currency %s does not match budget currency %s", domain.ErrValidation, currency, limits.Currency)
	}
	if ledger.ttl() < time.Second {
		return domain.BudgetReservation{}, fmt.Errorf("%w: reservation TTL must be at least one second", domain.ErrValidation)
	}
	agent, err := ledger.turnAgent(ctx, tenantID, requestID, request.Principal)
	if err != nil {
		return domain.BudgetReservation{}, err
	}
	budget, err := ledger.Releases.Budget(ctx, agent)
	if err != nil {
		return domain.BudgetReservation{}, fmt.Errorf("release budget of agent %q: %w", agent.ID, err)
	}
	if tokens, costMinorUnits, err = turnGrant(budget, request.TurnGoverned, tokens, costMinorUnits); err != nil {
		return domain.BudgetReservation{}, err
	}
	windowSeconds := durationSeconds(limits.Window)
	tx, err := ledger.DB.BeginTx(ctx, budgetQueries)
	if err != nil {
		return domain.BudgetReservation{}, fmt.Errorf("reserve budget: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = keeldata.RollbackDetached(tx)
		}
	}()
	if _, err = tx.Query(ctx, qLockBudget, strconv.FormatInt(tenantID, 10)); err != nil {
		return domain.BudgetReservation{}, fmt.Errorf("reserve budget: lock tenant: %w", err)
	}
	turnState, err := tx.Query(ctx, qGetTurnState, tenantID, requestID)
	if err != nil {
		return domain.BudgetReservation{}, fmt.Errorf("reserve budget: find turn: %w", err)
	}
	if len(turnState.Rows) == 0 {
		return domain.BudgetReservation{}, fmt.Errorf("%w: turn request %q does not exist", domain.ErrNotFound, requestID)
	}
	if common.AsBool(turnState.Rows[0][0]) {
		return domain.BudgetReservation{}, fmt.Errorf("%w: turn request %q is terminal", domain.ErrConflict, requestID)
	}
	now, ok := common.AsTimeOK(turnState.Rows[0][1])
	if !ok {
		return domain.BudgetReservation{}, fmt.Errorf("reserve budget: invalid database clock %q", common.AsString(turnState.Rows[0][1]))
	}
	existing, err := tx.Query(ctx, qFindBudget, tenantID, requestID)
	if err != nil {
		return domain.BudgetReservation{}, fmt.Errorf("reserve budget: find request: %w", err)
	}
	attempt := int64(1)
	if len(existing.Rows) > 0 {
		row, err := decodeBudgetRow(tenantID, existing.Rows[0])
		if err != nil {
			return domain.BudgetReservation{}, fmt.Errorf("reserve budget: decode existing reservation: %w", err)
		}
		reservation := row.reservation
		if reservation.GrantedTokens != tokens || reservation.GrantedCostMinorUnits != costMinorUnits || reservation.Currency != currency {
			return domain.BudgetReservation{}, fmt.Errorf("%w: request %q has a different budget reservation", domain.ErrConflict, requestID)
		}
		if row.status == domain.ReservationHeld && !row.expired {
			if err = tx.Commit(ctx); err != nil {
				return domain.BudgetReservation{}, fmt.Errorf("reserve budget: commit replay: %w", err)
			}
			committed = true
			reservation.Replayed = true
			return reservation, nil
		}
		if row.status == domain.ReservationSettled {
			return domain.BudgetReservation{}, fmt.Errorf("%w: request %q budget reservation", domain.ErrBudgetSettled, requestID)
		}
		if row.status != domain.ReservationExpired && !(row.status == domain.ReservationHeld && row.expired) {
			return domain.BudgetReservation{}, fmt.Errorf("%w: request %q budget reservation is terminal", domain.ErrConflict, requestID)
		}
		if row.status == domain.ReservationHeld {
			expired, err := tx.Query(ctx, qExpireOneBudget, tenantID, reservation.ReservationID)
			if err != nil {
				return domain.BudgetReservation{}, fmt.Errorf("reserve budget: expire prior attempt: %w", err)
			}
			if len(expired.Rows) == 0 {
				current, err := tx.Query(ctx, qGetBudget, tenantID, reservation.ReservationID)
				if err != nil {
					return domain.BudgetReservation{}, fmt.Errorf("reserve budget: verify prior attempt: %w", err)
				}
				if len(current.Rows) == 0 {
					return domain.BudgetReservation{}, fmt.Errorf("%w: reservation %q disappeared", domain.ErrConflict, reservation.ReservationID)
				}
				fenced, err := decodeBudgetRow(tenantID, current.Rows[0])
				if err != nil {
					return domain.BudgetReservation{}, fmt.Errorf("reserve budget: decode prior attempt: %w", err)
				}
				if fenced.status != domain.ReservationExpired {
					return domain.BudgetReservation{}, fmt.Errorf("%w: reservation %q became %s", domain.ErrConflict, reservation.ReservationID, fenced.status)
				}
			}
		}
		if reservation.Attempt == int64(^uint64(0)>>1) {
			return domain.BudgetReservation{}, fmt.Errorf("%w: reservation attempt overflow", domain.ErrConflict)
		}
		attempt = reservation.Attempt + 1
	}
	// Admission runs only for a new attempt: a live hold was admitted already, and Abandon and
	// cancellation recover it to release it even after the budget became unenforceable.
	if err = enforceable(budget, currency); err != nil {
		return domain.BudgetReservation{}, err
	}
	if _, err = scope.CheckBudgetUnit(ctx, tx, budget); err != nil {
		return domain.BudgetReservation{}, err
	}
	if err = ledger.checkPeriod(ctx, tx, tenantID, agent.ID, budget, now, tokens, costMinorUnits, currency); err != nil {
		return domain.BudgetReservation{}, err
	}
	reservationID, err := newReservationID()
	if err != nil {
		return domain.BudgetReservation{}, err
	}
	result, err := tx.Query(ctx, qReserveBudget,
		tenantID, reservationID, requestID, string(domain.PrincipalAgent), agent.ID, attempt, tokens, costMinorUnits, currency, durationSeconds(ledger.ttl()),
		tenantID, windowSeconds, tokens, limits.WindowTokens,
		tenantID, currency, windowSeconds, costMinorUnits, limits.WindowCostMinorUnits,
	)
	if err != nil {
		return domain.BudgetReservation{}, fmt.Errorf("reserve budget: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.BudgetReservation{}, fmt.Errorf("%w: the budget window of tenant %d is exhausted", domain.ErrBudgetExceeded, tenantID)
	}
	expiresAt, ok := common.AsTimeOK(result.Rows[0][1])
	if !ok {
		return domain.BudgetReservation{}, fmt.Errorf("reserve budget: invalid expires_at %q", common.AsString(result.Rows[0][1]))
	}
	reservation := domain.BudgetReservation{
		TenantID:              tenantID,
		ReservationID:         common.AsString(result.Rows[0][0]),
		RequestID:             requestID,
		Attempt:               attempt,
		GrantedTokens:         tokens,
		GrantedCostMinorUnits: costMinorUnits,
		Currency:              currency,
		ExpiresAt:             expiresAt,
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.BudgetReservation{}, fmt.Errorf("reserve budget: commit: %w", err)
	}
	committed = true
	return reservation, nil
}

// Commit settles actual usage, including an overrun, into the budget window.
func (ledger *BudgetLedger) Commit(ctx context.Context, reservation domain.BudgetReservation, usage domain.Usage) error {
	return ledger.commit(ctx, nil, reservation, usage)
}

// CommitTurn settles the owner's turn reservation; a stale owner settles nothing.
func (ledger *BudgetLedger) CommitTurn(ctx context.Context, owner domain.TurnOwner, reservation domain.BudgetReservation, usage domain.Usage) error {
	return ledger.commit(ctx, &owner, reservation, usage)
}

func (ledger *BudgetLedger) commit(ctx context.Context, owner *domain.TurnOwner, reservation domain.BudgetReservation, usage domain.Usage) error {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CostMinorUnits < 0 {
		return fmt.Errorf("%w: usage cannot be negative", domain.ErrValidation)
	}
	if usage.Currency != "" && usage.Currency != reservation.Currency {
		return fmt.Errorf("%w: usage currency does not match reservation", domain.ErrValidation)
	}
	settledTokens := usage.InputTokens + usage.OutputTokens
	if settledTokens < usage.InputTokens {
		return fmt.Errorf("%w: token usage overflow", domain.ErrValidation)
	}
	return ledger.transition(ctx, owner, qSettleBudget, reservation, settledTokens, usage.CostMinorUnits)
}

// Release returns an unused hold to the tenant budget.
func (ledger *BudgetLedger) Release(ctx context.Context, reservation domain.BudgetReservation) error {
	return ledger.transition(ctx, nil, qReleaseBudget, reservation)
}

// ReleaseTurn returns the owner's unused turn reservation; a stale owner releases nothing.
func (ledger *BudgetLedger) ReleaseTurn(ctx context.Context, owner domain.TurnOwner, reservation domain.BudgetReservation) error {
	return ledger.transition(ctx, &owner, qReleaseBudget, reservation)
}

// transition settles or releases one reservation; with an owner, the guarded touch of the
// owner's turn is the transaction's first statement.
func (ledger *BudgetLedger) transition(ctx context.Context, owner *domain.TurnOwner, query string, reservation domain.BudgetReservation, extra ...any) error {
	if reservation.TenantID <= 0 || strings.TrimSpace(reservation.ReservationID) == "" {
		return fmt.Errorf("%w: reservation identity is required", domain.ErrValidation)
	}
	if owner != nil {
		if err := owner.Check(); err != nil {
			return err
		}
		if !owner.Owns(reservation.TenantID, reservation.RequestID) {
			return fmt.Errorf("%w: the owner of turn %q cannot settle the reservation of %q", domain.ErrFenceLost, owner.RequestID, reservation.RequestID)
		}
	}
	if ledger.DB == nil {
		return fmt.Errorf("budget ledger: database is required")
	}
	tx, err := ledger.DB.BeginTx(ctx, budgetQueries)
	if err != nil {
		return fmt.Errorf("budget reservation transition: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = keeldata.RollbackDetached(tx)
		}
	}()
	if owner != nil {
		touched, err := tx.Query(ctx, qGuardTurnOwner, owner.TenantID, owner.RequestID, owner.Fence)
		if err != nil {
			return fmt.Errorf("budget reservation transition: guard turn owner: %w", err)
		}
		if len(touched.Rows) == 0 {
			return fmt.Errorf("%w: turn %q moved past fence %d", domain.ErrFenceLost, owner.RequestID, owner.Fence)
		}
	}
	if _, err = tx.Query(ctx, qLockBudget, strconv.FormatInt(reservation.TenantID, 10)); err != nil {
		return fmt.Errorf("budget reservation transition: lock tenant: %w", err)
	}
	current, err := tx.Query(ctx, qGetBudget, reservation.TenantID, reservation.ReservationID)
	if err != nil {
		return fmt.Errorf("budget reservation transition: find: %w", err)
	}
	if len(current.Rows) == 0 {
		return fmt.Errorf("%w: reservation %q does not exist", domain.ErrNotFound, reservation.ReservationID)
	}
	row, err := decodeBudgetRow(reservation.TenantID, current.Rows[0])
	if err != nil {
		return fmt.Errorf("budget reservation transition: decode: %w", err)
	}
	if transitionAlreadyApplied(query, row, extra) {
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("budget reservation transition: commit replay: %w", err)
		}
		committed = true
		return nil
	}
	if row.status == domain.ReservationHeld && row.expired {
		expired, err := tx.Query(ctx, qExpireOneBudget, reservation.TenantID, reservation.ReservationID)
		if err != nil {
			return fmt.Errorf("budget reservation transition: expire lease: %w", err)
		}
		if len(expired.Rows) == 0 {
			return fmt.Errorf("%w: reservation %q lease changed", domain.ErrConflict, reservation.ReservationID)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("budget reservation transition: commit expiry: %w", err)
		}
		committed = true
		return fmt.Errorf("%w: reservation %q is expired", domain.ErrConflict, reservation.ReservationID)
	}
	if row.status != domain.ReservationHeld {
		return fmt.Errorf("%w: reservation %q is %s", domain.ErrConflict, reservation.ReservationID, row.status)
	}
	args := append(append([]any(nil), extra...), reservation.TenantID, reservation.ReservationID)
	result, err := tx.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("budget reservation transition: %w", err)
	}
	if len(result.Rows) == 0 {
		return fmt.Errorf("%w: reservation %q is not held", domain.ErrConflict, reservation.ReservationID)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("budget reservation transition: commit: %w", err)
	}
	committed = true
	return nil
}

type budgetRow struct {
	reservation   domain.BudgetReservation
	status        domain.ReservationStatus
	settledTokens int64
	settledCost   int64
	expired       bool
}

func decodeBudgetRow(tenantID int64, row []any) (budgetRow, error) {
	if len(row) < 11 {
		return budgetRow{}, fmt.Errorf("expected 11 columns, got %d", len(row))
	}
	expiresAt, ok := common.AsTimeOK(row[6])
	if !ok {
		return budgetRow{}, fmt.Errorf("invalid expires_at %q", common.AsString(row[6]))
	}
	attempt := common.AsInt64(row[9])
	if attempt <= 0 {
		return budgetRow{}, fmt.Errorf("invalid attempt %d", attempt)
	}
	return budgetRow{
		reservation: domain.BudgetReservation{
			TenantID: tenantID, ReservationID: common.AsString(row[0]), RequestID: common.AsString(row[1]), Attempt: attempt,
			GrantedTokens: common.AsInt64(row[3]), GrantedCostMinorUnits: common.AsInt64(row[4]),
			Currency: common.AsString(row[5]), ExpiresAt: expiresAt,
		},
		status:        domain.ReservationStatus(common.AsString(row[2])),
		settledTokens: common.AsInt64(row[7]),
		settledCost:   common.AsInt64(row[8]),
		expired:       common.AsBool(row[10]),
	}, nil
}

func transitionAlreadyApplied(query string, row budgetRow, extra []any) bool {
	if query == qReleaseBudget {
		return row.status == domain.ReservationReleased
	}
	return query == qSettleBudget && row.status == domain.ReservationSettled && len(extra) == 2 &&
		row.settledTokens == extra[0].(int64) && row.settledCost == extra[1].(int64)
}

func durationSeconds(duration time.Duration) int64 {
	return int64((duration-1)/time.Second + 1)
}

// Expire reclaims lapsed holds in bounded batches; run it from a periodic worker.
func (ledger *BudgetLedger) Expire(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("%w: limit must be positive", domain.ErrValidation)
	}
	if err := ledger.init(ctx); err != nil {
		return 0, err
	}
	result, err := ledger.qs.Query(ctx, qExpireBudget, limit)
	if err != nil {
		return 0, fmt.Errorf("expire budget reservations: %w", err)
	}
	return int64(len(result.Rows)), nil
}

func newReservationID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate reservation id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
