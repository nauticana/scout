package contract

import "context"

// HealthProbe reports whether a dependency is safe to receive traffic.
type HealthProbe interface {
	// Check reports whether a dependency can safely receive traffic.
	Check(ctx context.Context) error
}
