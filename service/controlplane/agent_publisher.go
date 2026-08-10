package controlplane

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// AgentPublisher validates, compiles, and atomically stores immutable agent versions.
type AgentPublisher struct {
	Compiler contract.AgentCompiler
	Store    contract.AgentPublicationStore
}

// Publish compiles and persists one immutable definition without activating traffic.
func (publisher *AgentPublisher) Publish(ctx context.Context, tenantID int64, definition domain.AgentDefinition) error {
	if tenantID <= 0 || strings.TrimSpace(definition.AgentID) == "" || strings.TrimSpace(definition.Version) == "" {
		return fmt.Errorf("%w: tenant, agent, and version are required", domain.ErrValidation)
	}
	if !validSHA256(definition.DefinitionDigest) {
		return fmt.Errorf("%w: definition digest must contain 64 hexadecimal characters", domain.ErrValidation)
	}
	if publisher.Compiler == nil || publisher.Store == nil {
		return fmt.Errorf("agent publisher: compiler and publication store are required")
	}
	graph, err := publisher.Compiler.Compile(ctx, definition)
	if err != nil {
		return fmt.Errorf("compile agent %q version %q: %w", definition.AgentID, definition.Version, err)
	}
	if graph.AgentID != definition.AgentID || graph.Version != definition.Version || graph.EntryStepID == "" || !validSHA256(graph.Digest) {
		return fmt.Errorf("%w: compiler returned an inconsistent execution graph", domain.ErrValidation)
	}
	if err := publisher.Store.Publish(ctx, tenantID, definition, graph); err != nil {
		return fmt.Errorf("publish agent %q version %q: %w", definition.AgentID, definition.Version, err)
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

var _ contract.AgentPublisher = (*AgentPublisher)(nil)
