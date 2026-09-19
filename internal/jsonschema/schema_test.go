package jsonschema

import (
	"strings"
	"testing"
)

func TestCompileRejectsConstraintsTheBoundaryCannotEnforce(t *testing.T) {
	for _, keyword := range []string{"pattern", "format", "oneOf", "$ref"} {
		raw := []byte(`{"type":"string","` + keyword + `":"ignored"}`)
		if _, err := Compile(raw); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("%s: want an unsupported-keyword error, got %v", keyword, err)
		}
	}
}

func TestValidateJSONUsesUnicodeLengthAndRejectsTrailingValues(t *testing.T) {
	schema, err := Compile([]byte(`{"type":"string","minLength":1,"maxLength":1}`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if err := schema.ValidateJSON([]byte(`"é"`)); err != nil {
		t.Fatalf("one Unicode code point must have length one: %v", err)
	}
	if err := schema.ValidateJSON([]byte(`"a" "b"`)); err == nil {
		t.Fatal("multiple JSON values must be rejected")
	}
}

func TestCompileRejectsContradictoryBounds(t *testing.T) {
	if _, err := Compile([]byte(`{"type":"string","minLength":3,"maxLength":2}`)); err == nil {
		t.Fatal("contradictory bounds must be rejected")
	}
}

func TestEnumMatchesNumbersHoweverTheDocumentSpellsThem(t *testing.T) {
	schema, err := Compile([]byte(`{"type":"object","properties":{"n":{"type":"number","enum":[1,100]}}}`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	for _, document := range []string{`{"n":1}`, `{"n":1.0}`, `{"n":1e2}`} {
		if err := schema.ValidateJSON([]byte(document)); err != nil {
			t.Fatalf("%s: %v", document, err)
		}
	}
	if err := schema.ValidateJSON([]byte(`{"n":2}`)); err == nil {
		t.Fatal("a value outside the enum must be rejected")
	}
}
