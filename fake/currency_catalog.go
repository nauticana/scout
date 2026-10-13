package fake

import (
	"context"
	"fmt"

	"github.com/nauticana/keel/geo"

	"github.com/nauticana/scout/service/scope"
)

// CurrencyCatalog is a scope.CurrencyCatalog over a map of code to exponent that records the
// codes it was asked for.
type CurrencyCatalog struct {
	Exponents map[string]int
	Reads     []string
}

// Exponent answers from Exponents; a missing code is geo.ErrUnknownCurrency.
func (c *CurrencyCatalog) Exponent(_ context.Context, code string) (int, error) {
	c.Reads = append(c.Reads, code)
	exponent, ok := c.Exponents[code]
	if !ok {
		return 0, fmt.Errorf("%w %q", geo.ErrUnknownCurrency, code)
	}
	return exponent, nil
}

var _ scope.CurrencyCatalog = (*CurrencyCatalog)(nil)
