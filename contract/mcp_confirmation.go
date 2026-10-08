package contract

import (
	"context"
	"encoding/json"

	"github.com/nauticana/scout/domain"
)

// MCPConfirmationChecker re-authorizes a confirmation against current state.
// The product owns who may make and decide an action; any error refuses.
type MCPConfirmationChecker interface {
	// Maker reports whether the maker may still run the action.
	Maker(ctx context.Context, confirmation domain.MCPConfirmation) error
	// Decider reports whether decider may decide the action now.
	Decider(ctx context.Context, confirmation domain.MCPConfirmation, decider domain.PrincipalRef) error
}

// MCPConfirmedRunner runs a confirmed action's stored payload through the
// product's tool backend. An error wrapping domain.ErrEffectUnknown records the
// outcome as unknown; any other error records it as failed.
type MCPConfirmedRunner interface {
	RunConfirmed(ctx context.Context, confirmation domain.MCPConfirmation) (json.RawMessage, error)
}
