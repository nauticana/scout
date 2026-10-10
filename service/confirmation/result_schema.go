package confirmation

import (
	"encoding/json"
	"fmt"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/jsonschema"
)

// Statuses are every status ToolResult reports, in lifecycle order.
var Statuses = []domain.MCPConfirmationStatus{
	domain.MCPConfirmationPending, domain.MCPConfirmationApproved, domain.MCPConfirmationExecuting,
	domain.MCPConfirmationExecuted, domain.MCPConfirmationFailed, domain.MCPConfirmationUnknown,
	domain.MCPConfirmationDeclined, domain.MCPConfirmationWithdrawn, domain.MCPConfirmationExpired,
}

// StatusSchema is the JSON Schema of the status ToolResult reports.
var StatusSchema = mustStatusSchema()

// Instructions explains a confirmation result once, for a server's instructions.
const Instructions = "A tool that must be confirmed by a person returns confirmation_id and status instead of acting: " +
	"pending waits for a decision (at approval_url when given) until expires_at; approved and executing are running; " +
	"executed carries the result; failed and unknown carry reason, and unknown means the effect may have happened; " +
	"declined, withdrawn and expired did nothing. preview is the complete action the person approves. " +
	"Never repeat a call to execute a confirmation; read its status instead."

func mustStatusSchema() json.RawMessage {
	encoded, err := json.Marshal(map[string]any{"type": "string", "enum": Statuses})
	if err != nil {
		panic(err)
	}
	return encoded
}

// ResultSchema is the JSON Schema of the data ToolResult returns. preview and
// result are the schemas of the product's preview and executed result; nil
// admits any value. Both must be schemas Scout's output validator enforces.
func ResultSchema(preview, result json.RawMessage) (json.RawMessage, error) {
	properties := map[string]json.RawMessage{
		"confirmation_id": json.RawMessage(`{"type":"integer","minimum":1}`),
		"status":          StatusSchema,
		"tool":            json.RawMessage(`{"type":"string"}`),
		"expires_at":      json.RawMessage(`{"type":"string"}`),
		"approval_id":     json.RawMessage(`{"type":"integer","minimum":1}`),
		"reason":          json.RawMessage(`{"type":"string"}`),
		"approval_url":    json.RawMessage(`{"type":"string"}`),
	}
	for name, schema := range map[string]json.RawMessage{"preview": preview, "result": result} {
		if schema == nil {
			schema = json.RawMessage(`{}`)
		}
		if _, err := jsonschema.Compile(schema); err != nil {
			return nil, fmt.Errorf("%w: confirmation %s schema: %w", domain.ErrValidation, name, err)
		}
		properties[name] = schema
	}
	return json.Marshal(map[string]any{
		"type": "object", "additionalProperties": false, "properties": properties,
		"required": []string{"confirmation_id", "status", "tool", "expires_at"},
	})
}
