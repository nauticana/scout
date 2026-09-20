package provider

import (
	"encoding/json"
	"fmt"
	"slices"

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

// Constraint keywords a vendor's structured-output mode rejects. The gateway
// still validates the full schema, so dropping them here loosens nothing.
var anthropicUnsupportedKeywords = []string{"minimum", "maximum", "minLength", "maxLength", "maxItems", "maxProperties"}

// Keywords whose value maps names to subschemas, and keywords whose value is a
// literal. Names and literals are data: neither is filtered as a keyword.
var (
	schemaNameMaps = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}
	schemaLiterals = []string{"enum", "const", "default", "examples"}
)

// projectSchema returns a copy of schema without the listed keywords at any depth.
func projectSchema(schema map[string]any, dropped []string) map[string]any {
	projected := make(map[string]any, len(schema))
	for key, value := range schema {
		if slices.Contains(dropped, key) || key == "minItems" && schemaInteger(value) > 1 {
			continue
		}
		named, isNameMap := value.(map[string]any)
		switch {
		case slices.Contains(schemaLiterals, key):
		case isNameMap && slices.Contains(schemaNameMaps, key):
			subschemas := make(map[string]any, len(named))
			for name, subschema := range named {
				subschemas[name] = projectSchemaValue(subschema, dropped)
			}
			value = subschemas
		default:
			value = projectSchemaValue(value, dropped)
		}
		projected[key] = value
	}
	return projected
}

func projectSchemaValue(value any, dropped []string) any {
	switch typed := value.(type) {
	case map[string]any:
		return projectSchema(typed, dropped)
	case []any:
		projected := make([]any, len(typed))
		for index, item := range typed {
			projected[index] = projectSchemaValue(item, dropped)
		}
		return projected
	default:
		return value
	}
}

func schemaInteger(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	default:
		return 0
	}
}

// strictCompatible reports whether every object in the schema closes its
// properties and requires all of them, which OpenAI strict mode demands.
func strictCompatible(schema map[string]any) bool {
	properties, _ := schema["properties"].(map[string]any)
	if schema["type"] == "object" || properties != nil {
		if allowsExtra, _ := schema["additionalProperties"].(bool); schema["additionalProperties"] == nil || allowsExtra {
			return false
		}
		required, _ := schema["required"].([]any)
		if len(required) != len(properties) {
			return false
		}
	}
	for _, property := range properties {
		if nested, ok := property.(map[string]any); ok && !strictCompatible(nested) {
			return false
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		return strictCompatible(items)
	}
	return true
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
