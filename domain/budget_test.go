package domain

import (
	"errors"
	"testing"
	"time"
)

func utc(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func day(zone string) BudgetPeriod   { return BudgetPeriod{Kind: BudgetDaily, Timezone: zone} }
func month(zone string) BudgetPeriod { return BudgetPeriod{Kind: BudgetMonthly, Timezone: zone} }

func mustBounds(t *testing.T, period BudgetPeriod, now time.Time) (time.Time, time.Time) {
	t.Helper()
	start, end, err := period.Bounds(now)
	if err != nil {
		t.Fatalf("%+v at %s: %v", period, now, err)
	}
	return start, end
}

func TestBudgetDayBoundsFollowTheZoneAcrossTransitions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		period     BudgetPeriod
		now        string
		start, end string
	}{
		{"berlin spring day is 23h", day("Europe/Berlin"), "2026-03-29T12:00:00Z", "2026-03-28T23:00:00Z", "2026-03-29T22:00:00Z"},
		{"berlin autumn day is 25h", day("Europe/Berlin"), "2026-10-25T12:00:00Z", "2026-10-24T22:00:00Z", "2026-10-25T23:00:00Z"},
		{"berlin last second of the short day", day("Europe/Berlin"), "2026-03-29T21:59:59Z", "2026-03-28T23:00:00Z", "2026-03-29T22:00:00Z"},
		{"berlin first second of the next day", day("Europe/Berlin"), "2026-03-29T22:00:00Z", "2026-03-29T22:00:00Z", "2026-03-30T22:00:00Z"},
		{"havana midnight in the gap", day("America/Havana"), "2026-03-08T12:00:00Z", "2026-03-08T05:00:00Z", "2026-03-09T04:00:00Z"},
		{"havana repeated midnight keeps the first", day("America/Havana"), "2026-11-01T12:00:00Z", "2026-11-01T04:00:00Z", "2026-11-02T05:00:00Z"},
		{"santiago midnight in the gap", day("America/Santiago"), "2026-09-06T12:00:00Z", "2026-09-06T04:00:00Z", "2026-09-07T03:00:00Z"},
		{"beirut midnight in the gap", day("Asia/Beirut"), "2026-03-29T12:00:00Z", "2026-03-28T22:00:00Z", "2026-03-29T21:00:00Z"},
		{"sao paulo midnight in the gap", day("America/Sao_Paulo"), "2018-11-04T12:00:00Z", "2018-11-04T03:00:00Z", "2018-11-05T02:00:00Z"},
		{"apia day before the skipped day", day("Pacific/Apia"), "2011-12-29T20:00:00Z", "2011-12-29T10:00:00Z", "2011-12-30T10:00:00Z"},
		{"apia day after the skipped day", day("Pacific/Apia"), "2011-12-30T10:00:00Z", "2011-12-30T10:00:00Z", "2011-12-31T10:00:00Z"},
		{"kolkata half hour", day("Asia/Kolkata"), "2026-01-15T00:00:00Z", "2026-01-14T18:30:00Z", "2026-01-15T18:30:00Z"},
		{"chatham quarter hour", day("Pacific/Chatham"), "2026-06-15T00:00:00Z", "2026-06-14T11:15:00Z", "2026-06-15T11:15:00Z"},
		{"utc", day("UTC"), "2026-06-15T23:59:59Z", "2026-06-15T00:00:00Z", "2026-06-16T00:00:00Z"},
		{"berlin march is 743h", month("Europe/Berlin"), "2026-03-15T00:00:00Z", "2026-02-28T23:00:00Z", "2026-03-31T22:00:00Z"},
		{"december to january", month("Europe/Istanbul"), "2026-12-31T20:59:59Z", "2026-11-30T21:00:00Z", "2026-12-31T21:00:00Z"},
		{"january after december", month("Europe/Istanbul"), "2026-12-31T21:00:00Z", "2026-12-31T21:00:00Z", "2027-01-31T21:00:00Z"},
		{"leap february", month("UTC"), "2028-02-29T12:00:00Z", "2028-02-01T00:00:00Z", "2028-03-01T00:00:00Z"},
	} {
		start, end := mustBounds(t, tc.period, utc(tc.now))
		if !start.Equal(utc(tc.start)) || !end.Equal(utc(tc.end)) || start.Location() != time.UTC || end.Location() != time.UTC {
			t.Errorf("%s: [%s, %s), want [%s, %s)", tc.name, start, end, tc.start, tc.end)
		}
	}
	start, end := mustBounds(t, month("Europe/Berlin"), utc("2026-03-15T00:00:00Z"))
	if end.Sub(start) != 743*time.Hour {
		t.Fatalf("march = %s", end.Sub(start))
	}
}

func TestBudgetBoundsEdgesAreHalfOpen(t *testing.T) {
	period := day("Europe/Berlin")
	start, end := mustBounds(t, period, utc("2026-10-25T12:00:00Z"))
	for _, now := range []time.Time{start, end.Add(-time.Nanosecond)} {
		if s, e := mustBounds(t, period, now); !s.Equal(start) || !e.Equal(end) {
			t.Errorf("%s: [%s, %s), want the same day", now, s, e)
		}
	}
	if next, _ := mustBounds(t, period, end); !next.Equal(end) {
		t.Fatalf("now == end starts the next day at %s, want %s", next, end)
	}
}

func TestBudgetBoundsHoldForEveryHourOfTheYear(t *testing.T) {
	zones := []string{"Europe/Berlin", "America/Havana", "America/Santiago", "Pacific/Apia", "Asia/Kolkata", "Pacific/Chatham"}
	for _, zone := range zones {
		location, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		date := func(at time.Time) string { return at.In(location).Format(time.DateOnly) }
		for _, period := range []BudgetPeriod{day(zone), month(zone)} {
			for now := utc("2026-01-01T00:00:00Z"); now.Year() == 2026; now = now.Add(time.Hour) {
				start, end := mustBounds(t, period, now)
				if now.Before(start) || !now.Before(end) {
					t.Fatalf("%s %s at %s: [%s, %s) does not hold now", zone, period.Kind, now, start, end)
				}
				if next, _ := mustBounds(t, period, end); !next.Equal(end) {
					t.Fatalf("%s %s at %s: the next period starts at %s, not %s", zone, period.Kind, now, next, end)
				}
				if before := start.Add(-time.Nanosecond); date(before) >= date(start) {
					t.Fatalf("%s %s at %s: %s is not the first instant of %s", zone, period.Kind, now, start, date(start))
				}
				if date(start) > date(now) {
					t.Fatalf("%s %s at %s: start date %s is after %s", zone, period.Kind, now, date(start), date(now))
				}
			}
		}
	}
}

func TestBudgetRollingAndTurnBounds(t *testing.T) {
	now := utc("2026-06-15T12:00:00Z")
	start, end := mustBounds(t, BudgetPeriod{Kind: BudgetRolling, Window: time.Hour}, now)
	if !start.Equal(now.Add(-time.Hour)) || !end.Equal(now) {
		t.Fatalf("rolling = [%s, %s)", start, end)
	}
	if _, _, err := (BudgetPeriod{Kind: BudgetPerTurn}).Bounds(now); !errors.Is(err, ErrValidation) {
		t.Fatalf("turn bounds = %v, want validation", err)
	}
}

func TestBudgetPeriodValidateRefusesWhatTheKindDoesNotTake(t *testing.T) {
	for name, period := range map[string]BudgetPeriod{
		"unknown kind":         {Kind: "week"},
		"local zone":           day("Local"),
		"empty zone":           day(""),
		"unknown zone":         day("Mars/Base"),
		"long zone":            day("Europe/Berlin/" + string(make([]byte, 60))),
		"zone on rolling":      {Kind: BudgetRolling, Window: time.Hour, Timezone: "UTC"},
		"zone on turn":         {Kind: BudgetPerTurn, Timezone: "UTC"},
		"window on turn":       {Kind: BudgetPerTurn, Window: time.Hour},
		"window on day":        {Kind: BudgetDaily, Window: time.Hour, Timezone: "UTC"},
		"59 second window":     {Kind: BudgetRolling, Window: 59 * time.Second},
		"367 day window":       {Kind: BudgetRolling, Window: 367 * 24 * time.Hour},
		"fractional window":    {Kind: BudgetRolling, Window: 90*time.Second + time.Millisecond},
		"rolling, no window":   {Kind: BudgetRolling},
		"month without a zone": {Kind: BudgetMonthly},
	} {
		if err := period.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
	for name, period := range map[string]BudgetPeriod{
		"turn":       {Kind: BudgetPerTurn},
		"one minute": {Kind: BudgetRolling, Window: time.Minute},
		"366 days":   {Kind: BudgetRolling, Window: MaxBudgetWindow},
		"day":        day("Europe/Istanbul"),
		"month":      month("UTC"),
	} {
		if err := period.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestBudgetUnenforceableErrorIsDegraded(t *testing.T) {
	err := error(&BudgetUnenforceableError{Problem: BudgetExponentChanged, Detail: "frozen 2, catalog 3"})
	if !errors.Is(err, ErrDegraded) || err.Error() != "budget unenforceable (exponent_changed): frozen 2, catalog 3" {
		t.Fatalf("error = %v", err)
	}
	if (ReleaseBudget{}).Bounded() || !(ReleaseBudget{CostMinorUnits: 1}).Bounded() {
		t.Fatal("bounded")
	}
}
