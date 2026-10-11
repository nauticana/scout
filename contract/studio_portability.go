package contract

import (
	"context"
	"encoding/json"

	"github.com/nauticana/scout/domain"
)

// AgentDocumentCodec renders a portable agent as a Charter AgentDefinition document and reads one back.
type AgentDocumentCodec interface {
	// Encode returns the canonical document and its "sha256:<hex>" digest.
	Encode(ctx context.Context, tenantID int64, agent domain.PortableAgent) (document json.RawMessage, digest string, err error)
	// Decode refuses anything Encode could not have written and a digest that does not match.
	Decode(ctx context.Context, document json.RawMessage) (domain.PortableAgent, error)
}

// StudioPortabilityBackend exports an agent as a document and imports one into the caller's tenant.
type StudioPortabilityBackend interface {
	Export(ctx context.Context, actor domain.StudioActor, agentID string) (json.RawMessage, error)
	Import(ctx context.Context, actor domain.StudioActor, request domain.AgentImportRequest) (domain.AgentSummary, error)
}
