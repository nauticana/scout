package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// Labeler fills the captions of the supplied refs; which caption names a person is the product's choice.
type Labeler interface {
	Label(ctx context.Context, labels domain.Labels) error
}
