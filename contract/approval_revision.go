package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// EvidenceArchive keeps payloads where evidence lives, content-addressed; rows keep only the reference.
type EvidenceArchive interface {
	Dehydrate(ctx context.Context, name string, payload []byte) (domain.ObjectRef, error)
	// Hydrate loads the referenced payload and fails closed when it no longer matches its digest.
	Hydrate(ctx context.Context, ref domain.ObjectRef) ([]byte, error)
}

// ProposalRevisions reads the edit a decider made to a parked tool call.
type ProposalRevisions interface {
	// Revision returns the revised arguments when call's proposal was edited, and false otherwise.
	Revision(ctx context.Context, call domain.ToolCall) (domain.CallRevision, bool, error)
}
