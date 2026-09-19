package provider

import (
	"encoding/json"
	"fmt"

	"github.com/nauticana/scout/domain"
)

// conversation is the request as one ordered message list: the prompt opens it
// and Messages continue it.
func conversation(request domain.ModelRequest) []domain.ModelMessage {
	messages := make([]domain.ModelMessage, 0, len(request.Messages)+1)
	if len(request.Prompt) > 0 {
		messages = append(messages, domain.ModelMessage{Role: domain.ModelRoleUser, Text: request.Prompt})
	}
	return append(messages, request.Messages...)
}

func schemaObject(raw []byte) (map[string]any, error) {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("%w: schema is not a JSON object: %w", domain.ErrValidation, err)
	}
	return schema, nil
}

// checkOutputMode refuses a mode the adapter cannot enforce natively, so a
// constrained request is never served as free text.
func checkOutputMode(provider string, output domain.OutputConstraint) error {
	switch output.Mode {
	case domain.OutputModeText, domain.OutputModeJSONSchema:
		return nil
	}
	return fmt.Errorf("%w: %s adapter cannot enforce output mode %q", domain.ErrCapabilityUnsupported, provider, output.Mode)
}

func schemaName(output domain.OutputConstraint) string {
	if output.SchemaName != "" {
		return output.SchemaName
	}
	return "output"
}

// finishReason normalizes a stop that carries tool calls; every other reason
// stays the provider's own.
func finishReason(native string, calls []domain.ModelToolCall) string {
	if len(calls) > 0 {
		return domain.FinishReasonToolCalls
	}
	return native
}

func emptyResult(provider string, text string, calls []domain.ModelToolCall) error {
	if text == "" && len(calls) == 0 {
		return fmt.Errorf("%s: response carried neither text nor tool calls", provider)
	}
	return nil
}
