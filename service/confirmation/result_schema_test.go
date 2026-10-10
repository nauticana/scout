package confirmation

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/jsonschema"
)

// Every result ToolResult returns conforms to ResultSchema.
func TestToolResultConformsToResultSchema(t *testing.T) {
	preview := json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"}},"required":["action"]}`)
	result := json.RawMessage(`{"type":"object","properties":{"page_id":{"type":"integer"}}}`)
	raw, err := ResultSchema(preview, result)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := jsonschema.Compile(raw)
	if err != nil {
		t.Fatalf("result schema does not compile: %v", err)
	}
	for _, status := range Statuses {
		for _, expired := range []bool{false, true} {
			confirmation := domain.MCPConfirmation{ID: 5, ApprovalID: 9, Maker: maker, Tool: "publish_page", Status: status,
				Expired: expired, ExpiresAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), Error: "provider timeout",
				Preview: json.RawMessage(`{"action":"Publish"}`), Result: json.RawMessage(`{"page_id":4}`)}
			encoded, err := json.Marshal(ToolResult(confirmation, maker, "", "", "https://app.example/approvals/9").Data)
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.ValidateJSON(encoded); err != nil {
				t.Errorf("%s (expired %v): %s: %v", status, expired, encoded, err)
			}
		}
	}
}

func TestResultSchemaRefusesSchemasTheValidatorCannotEnforce(t *testing.T) {
	if _, err := ResultSchema(json.RawMessage(`{"type":"object","allOf":[]}`), nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unsupported preview schema error = %v", err)
	}
	if _, err := ResultSchema(nil, json.RawMessage(`{"type":`)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("malformed result schema error = %v", err)
	}
	if string(StatusSchema) != `{"enum":["pending","approved","executing","executed","failed","unknown","declined","withdrawn","expired"],"type":"string"}` {
		t.Fatalf("status schema = %s", StatusSchema)
	}
}
