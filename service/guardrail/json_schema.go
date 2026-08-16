package guardrail

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// jsonSchema is the tiny deterministic subset of JSON Schema supported by the json_schema rule:
// type, properties, required, additionalProperties (bool), items, enum, min/max length, minimum/maximum, maxItems, maxProperties.
type jsonSchema struct {
	Type                 string                 `json:"type"`
	Properties           map[string]*jsonSchema `json:"properties"`
	Required             []string               `json:"required"`
	AdditionalProperties *bool                  `json:"additionalProperties"`
	Items                *jsonSchema            `json:"items"`
	Enum                 []json.RawMessage      `json:"enum"`
	MinLength            *int                   `json:"minLength"`
	MaxLength            *int                   `json:"maxLength"`
	Minimum              *float64               `json:"minimum"`
	Maximum              *float64               `json:"maximum"`
	MaxItems             *int                   `json:"maxItems"`
	MaxProperties        *int                   `json:"maxProperties"`
}

const maxSchemaDepth = 16

func (schema *jsonSchema) validate(depth int) error {
	if schema == nil {
		return nil
	}
	if depth > maxSchemaDepth {
		return fmt.Errorf("schema nesting exceeds %d levels", maxSchemaDepth)
	}
	switch schema.Type {
	case "", "object", "array", "string", "number", "integer", "boolean", "null":
	default:
		return fmt.Errorf("unsupported schema type %q", schema.Type)
	}
	for name, property := range schema.Properties {
		if err := property.validate(depth + 1); err != nil {
			return fmt.Errorf("property %q: %w", name, err)
		}
	}
	if err := schema.Items.validate(depth + 1); err != nil {
		return fmt.Errorf("items: %w", err)
	}
	return nil
}

func (schema *jsonSchema) check(value any, path string) error {
	if schema == nil {
		return nil
	}
	if schema.Type != "" && !typeMatches(schema.Type, value) {
		return fmt.Errorf("%s: expected %s", path, schema.Type)
	}
	if len(schema.Enum) > 0 && !enumContains(schema.Enum, value) {
		return fmt.Errorf("%s: value not in enum", path)
	}
	switch typed := value.(type) {
	case string:
		if schema.MinLength != nil && len(typed) < *schema.MinLength || schema.MaxLength != nil && len(typed) > *schema.MaxLength {
			return fmt.Errorf("%s: string length out of range", path)
		}
	case float64:
		if schema.Minimum != nil && typed < *schema.Minimum || schema.Maximum != nil && typed > *schema.Maximum {
			return fmt.Errorf("%s: number out of range", path)
		}
	case []any:
		if schema.MaxItems != nil && len(typed) > *schema.MaxItems {
			return fmt.Errorf("%s: too many items", path)
		}
		for i, item := range typed {
			if err := schema.Items.check(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case map[string]any:
		if schema.MaxProperties != nil && len(typed) > *schema.MaxProperties {
			return fmt.Errorf("%s: too many properties", path)
		}
		for _, name := range schema.Required {
			if _, ok := typed[name]; !ok {
				return fmt.Errorf("%s: missing required %q", path, name)
			}
		}
		for name, item := range typed {
			property, known := schema.Properties[name]
			if !known {
				if schema.AdditionalProperties != nil && !*schema.AdditionalProperties {
					return fmt.Errorf("%s: unexpected property %q", path, name)
				}
				continue
			}
			if err := property.check(item, path+"."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

func typeMatches(kind string, value any) bool {
	switch kind {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		number, ok := value.(float64)
		return ok && number == math.Trunc(number)
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	}
	return false
}

func enumContains(enum []json.RawMessage, value any) bool {
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	for _, candidate := range enum {
		var decoded any
		if json.Unmarshal(candidate, &decoded) != nil {
			continue
		}
		normalized, err := json.Marshal(decoded)
		if err == nil && strings.TrimSpace(string(normalized)) == string(encoded) {
			return true
		}
	}
	return false
}
