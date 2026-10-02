package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// RestrictionLayerReader returns the platform and tenant layers in force now. An
// unreadable or corrupt layer is an error, which every consumer treats as a denial.
type RestrictionLayerReader interface {
	Layers(ctx context.Context, tenantID int64) (domain.RestrictionLayers, error)
}

// RestrictionLayerWriter replaces a layer by compare-and-swap on its digest; an
// empty expectedDigest means no layer has been written. Writing the value already
// in force succeeds unchanged. A mismatch is ErrConflict with the current digest.
// Only a service principal may write, and every change is audited.
type RestrictionLayerWriter interface {
	ReplacePlatform(ctx context.Context, actor domain.Principal, layer domain.RestrictionLayer, expectedDigest string) (string, error)
	ReplaceTenant(ctx context.Context, actor domain.Principal, tenantID int64, layer domain.RestrictionLayer, expectedDigest string) (string, error)
}
