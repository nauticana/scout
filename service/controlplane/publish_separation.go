package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// BasePublishSeparation requires two-person publish for every tenant with All, or for the
// tenants listed, without Charter. It admits no exception: relieving a person of the
// separation takes an explicit, bounded, revocable Charter SodException.
type BasePublishSeparation struct {
	All     bool
	Tenants map[int64]bool
}

var _ contract.PublishSeparation = BasePublishSeparation{}

func (s BasePublishSeparation) Required(_ context.Context, tenantID int64, _ string, _ time.Time) (bool, error) {
	return s.All || s.Tenants[tenantID], nil
}

func (BasePublishSeparation) Excuse(_ context.Context, _ int64, _, exceptionID string, _ domain.PrincipalRef, _ time.Time) error {
	return fmt.Errorf("%w: separation exception %q is not recognized; exceptions are Charter SodExceptions", domain.ErrForbidden, exceptionID)
}
