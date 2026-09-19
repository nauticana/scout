// Package jsonschema is the small deterministic JSON Schema subset Scout validates
// at its own boundaries: guardrail rules, tool contracts, tool-call arguments, and
// constrained model output.
package jsonschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

// Schema supports type, properties, required, additionalProperties (bool), items, enum,
// min/max length, minimum/maximum, maxItems, and maxProperties. Annotation keywords
// are retained; unsupported validation keywords are rejected rather than ignored.
type Schema struct {
	Schema               string             `json:"$schema"`
	ID                   string             `json:"$id"`
	Title                string             `json:"title"`
	Description          string             `json:"description"`
	Comment              string             `json:"$comment"`
	Default              json.RawMessage    `json:"default"`
	Examples             []json.RawMessage  `json:"examples"`
	Type                 string             `json:"type"`
	Properties           map[string]*Schema `json:"properties"`
	Required             []string           `json:"required"`
	AdditionalProperties *bool              `json:"additionalProperties"`
	Items                *Schema            `json:"items"`
	Enum                 []json.RawMessage  `json:"enum"`
	MinLength            *int               `json:"minLength"`
	MaxLength            *int               `json:"maxLength"`
	Minimum              *float64           `json:"minimum"`
	Maximum              *float64           `json:"maximum"`
	MaxItems             *int               `json:"maxItems"`
	MaxProperties        *int               `json:"maxProperties"`
}

const maxSchemaDepth = 16

// Compile parses a schema and rejects one this subset cannot enforce.
func Compile(raw []byte) (*Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("schema is empty")
	}
	schema := &Schema{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(schema); err != nil {
		return nil, fmt.Errorf("decode schema: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode schema: %w", err)
	}
	if err := schema.Check(); err != nil {
		return nil, err
	}
	return schema, nil
}

// Canonical re-encodes a JSON document with sorted keys and no insignificant
// whitespace, so equal contracts compare and digest equal.
func Canonical(raw []byte) ([]byte, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

// Check reports whether the schema is well formed.
func (schema *Schema) Check() error { return schema.validate(0) }

// ValidateJSON decodes a document and validates it.
func (schema *Schema) ValidateJSON(document []byte) error {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("decode document: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return fmt.Errorf("decode document: %w", err)
	}
	return schema.Validate(value)
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

// Validate checks a decoded JSON value.
func (schema *Schema) Validate(value any) error { return schema.check(value, "$") }

func (schema *Schema) validate(depth int) error {
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
	if schema.MinLength != nil && *schema.MinLength < 0 || schema.MaxLength != nil && *schema.MaxLength < 0 ||
		schema.MaxItems != nil && *schema.MaxItems < 0 || schema.MaxProperties != nil && *schema.MaxProperties < 0 {
		return fmt.Errorf("schema bounds cannot be negative")
	}
	if schema.MinLength != nil && schema.MaxLength != nil && *schema.MinLength > *schema.MaxLength {
		return fmt.Errorf("minLength exceeds maxLength")
	}
	if schema.Minimum != nil && schema.Maximum != nil && *schema.Minimum > *schema.Maximum {
		return fmt.Errorf("minimum exceeds maximum")
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

func (schema *Schema) check(value any, path string) error {
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
		length := utf8.RuneCountInString(typed)
		if schema.MinLength != nil && length < *schema.MinLength || schema.MaxLength != nil && length > *schema.MaxLength {
			return fmt.Errorf("%s: string length out of range", path)
		}
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return fmt.Errorf("%s: invalid number", path)
		}
		if schema.Minimum != nil && number < *schema.Minimum || schema.Maximum != nil && number > *schema.Maximum {
			return fmt.Errorf("%s: number out of range", path)
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
		switch value.(type) {
		case json.Number, float64:
			return true
		}
		return false
	case "integer":
		switch number := value.(type) {
		case json.Number:
			parsed, err := number.Float64()
			return err == nil && !math.IsInf(parsed, 0) && parsed == math.Trunc(parsed)
		case float64:
			return !math.IsInf(number, 0) && number == math.Trunc(number)
		}
		return false
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	}
	return false
}

func enumContains(enum []json.RawMessage, value any) bool {
	raw, err := json.Marshal(value)
	if err != nil {
		return false
	}
	// Both sides go through Canonical so a json.Number spelled 1.0 or 1e2 equals the enum's 1 or 100.
	encoded, err := Canonical(raw)
	if err != nil {
		return false
	}
	for _, candidate := range enum {
		normalized, err := Canonical(candidate)
		if err == nil && bytes.Equal(normalized, encoded) {
			return true
		}
	}
	return false
}
