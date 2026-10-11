package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// BasePublishSeparation requires two-person publish for every tenant with All, or for the
// tenants listed, without Charter; Restores extends the requirement to restoring a published
// version in the same tenants. It admits no exception: relieving a person of the separation
// takes an explicit, bounded, revocable Charter SodException.
type BasePublishSeparation struct {
	All      bool
	Tenants  map[int64]bool
	Restores bool
}

var _ contract.PublishSeparation = BasePublishSeparation{}

func (s BasePublishSeparation) Required(_ context.Context, tenantID int64, _ string, kind domain.PublishReviewKind, _ time.Time) (bool, error) {
	switch kind {
	case domain.PublishReviewOfPublish:
		return s.All || s.Tenants[tenantID], nil
	case domain.PublishReviewOfRestore:
		return s.Restores && (s.All || s.Tenants[tenantID]), nil
	}
	return false, fmt.Errorf("%w: unknown review kind %q", domain.ErrValidation, kind)
}

func (BasePublishSeparation) Excuse(_ context.Context, _ int64, _ string, _ domain.PublishReviewKind, exceptionID string, _ domain.PrincipalRef, _ time.Time) error {
	return fmt.Errorf("%w: separation exception %q is not recognized; exceptions are Charter SodExceptions", domain.ErrForbidden, exceptionID)
}
