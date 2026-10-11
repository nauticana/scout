package domain

import (
	"fmt"
	"time"
)

// BudgetPeriodKind names the span a release budget's ceilings apply to.
type BudgetPeriodKind string

const (
	BudgetPerTurn BudgetPeriodKind = "turn"
	BudgetRolling BudgetPeriodKind = "rolling"
	BudgetDaily   BudgetPeriodKind = "day"
	BudgetMonthly BudgetPeriodKind = "month"
)

const (
	MinBudgetWindow = time.Minute
	MaxBudgetWindow = 366 * 24 * time.Hour
	// MaxBudgetAmount bounds a period budget's tokens and minor units.
	MaxBudgetAmount int64 = 1_000_000_000_000_000
	// MaxCurrencyExponent mirrors currency_exponent_ck.
	MaxCurrencyExponent = 6
	MaxTimezoneLength   = 64
)

// BudgetPeriod is the span a budget resets over: one turn, a trailing window, or a calendar
// day or month in an IANA timezone.
type BudgetPeriod struct {
	Kind BudgetPeriodKind
	// Window is the trailing span of a rolling budget only.
	Window time.Duration
	// Timezone places a day or month budget's boundaries only.
	Timezone string
}

// Validate refuses an unknown kind, a field the kind does not take, a rolling window outside
// [MinBudgetWindow, MaxBudgetWindow] or not in whole seconds, and an unloadable timezone.
func (p BudgetPeriod) Validate() error {
	_, err := p.location()
	return err
}

func (p BudgetPeriod) location() (*time.Location, error) {
	switch p.Kind {
	case BudgetPerTurn, BudgetRolling:
		if p.Timezone != "" {
			return nil, fmt.Errorf("%w: a %s budget takes no timezone", ErrValidation, p.Kind)
		}
		if p.Kind == BudgetPerTurn && p.Window != 0 {
			return nil, fmt.Errorf("%w: a turn budget takes no window", ErrValidation)
		}
		if p.Kind == BudgetRolling && (p.Window < MinBudgetWindow || p.Window > MaxBudgetWindow || p.Window%time.Second != 0) {
			return nil, fmt.Errorf("%w: a rolling budget window must be whole seconds from %s to %s", ErrValidation, MinBudgetWindow, MaxBudgetWindow)
		}
		return nil, nil
	case BudgetDaily, BudgetMonthly:
		if p.Window != 0 {
			return nil, fmt.Errorf("%w: a %s budget takes no window", ErrValidation, p.Kind)
		}
		if p.Timezone == "" || p.Timezone == "Local" || len(p.Timezone) > MaxTimezoneLength {
			return nil, fmt.Errorf("%w: a %s budget needs an IANA timezone", ErrValidation, p.Kind)
		}
		location, err := time.LoadLocation(p.Timezone)
		if err != nil {
			return nil, fmt.Errorf("%w: unknown timezone %q", ErrValidation, p.Timezone)
		}
		return location, nil
	default:
		return nil, fmt.Errorf("%w: unknown budget period %q", ErrValidation, p.Kind)
	}
}

// Bounds returns the period [start, end) that contains now, in UTC. A day starts at the first
// instant whose local date is that day, which is midnight or, when midnight falls in a
// transition gap, the transition; a month starts at its first day's start. A rolling window
// is [now-Window, now). A turn has no bounds.
func (p BudgetPeriod) Bounds(now time.Time) (time.Time, time.Time, error) {
	location, err := p.location()
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	switch p.Kind {
	case BudgetRolling:
		return now.Add(-p.Window).UTC(), now.UTC(), nil
	case BudgetDaily, BudgetMonthly:
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("%w: a %s budget has no period bounds", ErrValidation, p.Kind)
	}
	year, month, day := now.In(location).Date()
	if p.Kind == BudgetMonthly {
		day = 1
	}
	// A zone whose date steps backwards can name a local date whose period does not hold now.
	for adjust := 0; adjust <= 2; adjust++ {
		start, end, err := p.span(year, month, day, location)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		step := 0
		switch {
		case now.Before(start):
			step = -1
		case !now.Before(end):
			step = 1
		default:
			return start, end, nil
		}
		if p.Kind == BudgetMonthly {
			month += time.Month(step)
		} else {
			day += step
		}
	}
	return time.Time{}, time.Time{}, fmt.Errorf("budget period bounds of %s in %s did not converge", now.UTC().Format(time.RFC3339), p.Timezone)
}

func (p BudgetPeriod) span(year int, month time.Month, day int, location *time.Location) (time.Time, time.Time, error) {
	nextYear, nextMonth, nextDay := year, month, day+1
	if p.Kind == BudgetMonthly {
		nextMonth, nextDay = month+1, 1
	}
	start, err := dayStart(year, month, day, location)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := dayStart(nextYear, nextMonth, nextDay, location)
	return start, end, err
}

// dayStart is the first instant whose local date in location is on or after the date. It walks
// the zone's offset periods in time order rather than trusting time.Date, which may resolve a
// midnight inside a gap to an instant after the gap's first local instant of the day.
func dayStart(year int, month time.Month, day int, location *time.Location) (time.Time, error) {
	midnight := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	at := midnight.Add(-30 * time.Hour)
	for periods := 0; periods < 8; periods++ {
		local := at.In(location)
		_, offset := local.Zone()
		_, periodEnd := local.ZoneBounds()
		candidate := midnight.Add(-time.Duration(offset) * time.Second)
		if candidate.Before(at) {
			candidate = at
		}
		if periodEnd.IsZero() || candidate.Before(periodEnd) {
			return candidate.UTC(), nil
		}
		at = periodEnd
	}
	return time.Time{}, fmt.Errorf("the start of %s in %s crosses too many zone transitions", midnight.Format(time.DateOnly), location)
}

// BudgetSource says where a principal's budget comes from.
type BudgetSource string

const (
	// BudgetSourceRelease is a budget frozen into the release.
	BudgetSourceRelease BudgetSource = "release"
	// BudgetSourceFallback is ReleaseLimits.Fallback, for a release that binds no budget.
	BudgetSourceFallback BudgetSource = "fallback"
)

// ReleaseBudget is the token and cost ceiling frozen into a release and the period it applies
// to. Cost is in minor units of Currency, which has *Exponent decimal places; zero is not limited.
type ReleaseBudget struct {
	Tokens         int64
	CostMinorUnits int64
	Currency       string
	// Exponent is frozen with a release cost ceiling; a fallback may leave it to the catalog.
	Exponent *int
	Period   BudgetPeriod
	Source   BudgetSource
}

// Bounded reports whether the budget caps anything.
func (b ReleaseBudget) Bounded() bool { return b.Tokens > 0 || b.CostMinorUnits > 0 }

// StepSpend is what the next step of a turn that has used used may spend: the remainder of a
// per-turn budget, and whether usage must be priced. A spent per-turn budget is ErrBudgetExceeded.
func (b ReleaseBudget) StepSpend(used Usage) (StepSpend, error) {
	spend := StepSpend{Currency: b.Currency, PricingRequired: b.CostMinorUnits > 0}
	if b.Period.Kind != BudgetPerTurn {
		return spend, nil
	}
	if b.Tokens > 0 {
		if spend.Tokens = b.Tokens - used.InputTokens - used.OutputTokens; spend.Tokens <= 0 {
			return StepSpend{}, fmt.Errorf("%w: the turn's budget of %d tokens is spent", ErrBudgetExceeded, b.Tokens)
		}
	}
	if b.CostMinorUnits > 0 {
		if spend.CostMinorUnits = b.CostMinorUnits - used.CostMinorUnits; spend.CostMinorUnits <= 0 {
			return StepSpend{}, fmt.Errorf("%w: the turn's budget of %d minor units is spent", ErrBudgetExceeded, b.CostMinorUnits)
		}
	}
	return spend, nil
}

// BudgetProblem names why a bounded release budget cannot be enforced.
type BudgetProblem string

const (
	// BudgetInvalidPeriod is a period the ledger cannot place on its clock, such as an
	// unloadable timezone.
	BudgetInvalidPeriod    BudgetProblem = "invalid_period"
	BudgetNoCurrency       BudgetProblem = "no_currency"
	BudgetUnknownCurrency  BudgetProblem = "unknown_currency"
	BudgetExponentChanged  BudgetProblem = "exponent_changed"
	BudgetCurrencyMismatch BudgetProblem = "currency_mismatch"
	// BudgetNoCostQuote refuses a hold that quotes no cost against a period cost ceiling.
	BudgetNoCostQuote BudgetProblem = "no_cost_quote"
)

// StepSpend is what one step may still spend of its turn's per-turn budget.
type StepSpend struct {
	// Tokens and CostMinorUnits remain of a per-turn budget; zero is no per-turn cap on that axis.
	Tokens         int64
	CostMinorUnits int64
	Currency       string
	// PricingRequired is set when the release has a cost ceiling of any period.
	PricingRequired bool
}
