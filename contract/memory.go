package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// MemoryStore keeps a tenant's living memory: every run reads it, and runs append to it.
type MemoryStore interface {
	// Sections returns the tenant's non-empty sections in display order.
	Sections(ctx context.Context, tenantID int64) ([]domain.MemorySection, error)
	// Append adds one fact with its provenance; a tool may write only agent-writable sections.
	Append(ctx context.Context, fact domain.MemoryFact) error
}
