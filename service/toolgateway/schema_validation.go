package toolgateway

import (
	"context"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/jsonschema"
)

// validateArguments holds a call to its registered input schema; a definition
// without one accepts any arguments.
func validateArguments(definition domain.ToolDefinition, arguments []byte) error {
	if len(definition.InputSchema) == 0 {
		return nil
	}
	schema, err := jsonschema.Compile(definition.InputSchema)
	if err != nil {
		return fmt.Errorf("%w: input schema of %s@%s: %w", domain.ErrConflict, definition.ToolID, definition.Version, err)
	}
	if err := schema.ValidateJSON(arguments); err != nil {
		return fmt.Errorf("%w: arguments of %s@%s: %w", domain.ErrValidation, definition.ToolID, definition.Version, err)
	}
	return nil
}

// SchemaResultValidator holds tool output to the registered output schema and
// fails closed on a definition that declares none.
type SchemaResultValidator struct{}

func (SchemaResultValidator) Validate(_ context.Context, definition domain.ToolDefinition, result domain.ToolResult) error {
	schema, err := jsonschema.Compile(definition.OutputSchema)
	if err != nil {
		return fmt.Errorf("output schema of %s@%s: %w", definition.ToolID, definition.Version, err)
	}
	return schema.ValidateJSON(result.Output)
}

var _ contract.ToolResultValidator = SchemaResultValidator{}
