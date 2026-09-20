package toolgateway

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/jsonschema"
)

const (
	qToolProfileEnsure = "scout_tool_profile_ensure"
	qToolProfileGet    = "scout_tool_profile_get"
	qToolVersionGet    = "scout_tool_version_get"
	qToolVersionInsert = "scout_tool_version_insert"
	qToolBoundList     = "scout_tool_bound_list"
	qToolBindingGet    = "scout_tool_binding_get"
	qToolBindingInsert = "scout_tool_binding_insert"

	toolVersionColumns = `v.tool_id, v.tool_version, p.display_name, v.endpoint_uri, v.input_schema, v.output_schema, v.timeout_ms, v.max_attempts`

	maxToolIdentifier  = 80
	maxToolDisplayName = 200
)

var toolRegistryQueries = map[string]string{
	qToolProfileEnsure: `
INSERT INTO tool_profile (tenant_id, tool_id, display_name)
VALUES (?, ?, ?)
ON CONFLICT (tenant_id, tool_id) DO NOTHING`,
	qToolProfileGet: `
SELECT display_name
  FROM tool_profile
 WHERE tenant_id = ? AND tool_id = ?`,
	qToolVersionGet: `
SELECT ` + toolVersionColumns + `
  FROM tool_version v
  JOIN tool_profile p ON p.tenant_id = v.tenant_id AND p.tool_id = v.tool_id
 WHERE v.tenant_id = ? AND v.tool_id = ? AND v.tool_version = ?`,
	qToolVersionInsert: `
INSERT INTO tool_version (tenant_id, tool_id, tool_version, endpoint_uri, input_schema, output_schema, timeout_ms, max_attempts)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, tool_id, tool_version) DO NOTHING`,
	qToolBoundList: `
SELECT ` + toolVersionColumns + `
  FROM agent_tool_binding b
  JOIN tool_version v ON v.tenant_id = b.tenant_id AND v.tool_id = b.tool_id AND v.tool_version = b.tool_version
  JOIN tool_profile p ON p.tenant_id = v.tenant_id AND p.tool_id = v.tool_id
 WHERE b.tenant_id = ? AND b.agent_id = ? AND b.agent_version = ?
 ORDER BY v.tool_id`,
	qToolBindingGet: `
SELECT tool_version
  FROM agent_tool_binding
 WHERE tenant_id = ? AND agent_id = ? AND agent_version = ? AND tool_id = ?`,
	qToolBindingInsert: `
INSERT INTO agent_tool_binding (tenant_id, agent_id, agent_version, tool_id, tool_version)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, agent_id, agent_version, tool_id) DO NOTHING`,
}

// TableToolRegistry is the ToolRegistry over tool_profile, tool_version, and
// agent_tool_binding. Versions and bindings are immutable: repeating identical
// content is a no-op, different content under the same key is ErrConflict.
type TableToolRegistry struct {
	DB port.DatabaseRepository
	// DefaultTimeout and DefaultMaxAttempts fill a definition that leaves them zero.
	DefaultTimeout     time.Duration
	DefaultMaxAttempts int

	once sync.Once
	qs   port.QueryService
}

func (registry *TableToolRegistry) init(ctx context.Context) error {
	if registry.DB == nil {
		return fmt.Errorf("tool registry: database is required")
	}
	registry.once.Do(func() { registry.qs = registry.DB.GetQueryService(ctx, toolRegistryQueries) })
	if registry.qs == nil {
		return fmt.Errorf("tool registry: query service is required")
	}
	return nil
}

// Register publishes one immutable version, creating the tool profile with it.
func (registry *TableToolRegistry) Register(ctx context.Context, tenantID int64, tool domain.ToolDefinition) error {
	if err := registry.init(ctx); err != nil {
		return err
	}
	tool, err := registry.normalize(tenantID, tool)
	if err != nil {
		return err
	}
	tx, err := registry.DB.BeginTx(ctx, toolRegistryQueries)
	if err != nil {
		return fmt.Errorf("begin tool registration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if _, err = tx.Query(ctx, qToolProfileEnsure, tenantID, tool.ToolID, tool.DisplayName); err != nil {
		return fmt.Errorf("ensure tool profile %q: %w", tool.ToolID, err)
	}
	profile, err := tx.Query(ctx, qToolProfileGet, tenantID, tool.ToolID)
	if err != nil {
		return fmt.Errorf("read tool profile %q: %w", tool.ToolID, err)
	}
	if len(profile.Rows) != 1 || common.AsString(profile.Rows[0][0]) != tool.DisplayName {
		return fmt.Errorf("%w: tool %s is already registered with a different display name", domain.ErrConflict, tool.ToolID)
	}
	existing, err := tx.Query(ctx, qToolVersionGet, tenantID, tool.ToolID, tool.Version)
	if err != nil {
		return fmt.Errorf("read tool version %s@%s: %w", tool.ToolID, tool.Version, err)
	}
	if len(existing.Rows) > 0 {
		if !sameToolContract(scanToolDefinition(existing.Rows[0]), tool) {
			return fmt.Errorf("%w: tool %s@%s is already registered with different content", domain.ErrConflict, tool.ToolID, tool.Version)
		}
		return nil
	}
	if _, err = tx.Query(ctx, qToolVersionInsert, tenantID, tool.ToolID, tool.Version, tool.Endpoint,
		string(tool.InputSchema), string(tool.OutputSchema), tool.Timeout.Milliseconds(), tool.MaxAttempts); err != nil {
		return fmt.Errorf("insert tool version %s@%s: %w", tool.ToolID, tool.Version, err)
	}
	stored, err := tx.Query(ctx, qToolVersionGet, tenantID, tool.ToolID, tool.Version)
	if err != nil {
		return fmt.Errorf("read inserted tool version %s@%s: %w", tool.ToolID, tool.Version, err)
	}
	if len(stored.Rows) != 1 || !sameToolContract(scanToolDefinition(stored.Rows[0]), tool) {
		return fmt.Errorf("%w: tool %s@%s was concurrently registered with different content", domain.ErrConflict, tool.ToolID, tool.Version)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tool registration: %w", err)
	}
	committed = true
	return nil
}

// Get returns one version owned by the tenant; another tenant's tool is ErrNotFound.
func (registry *TableToolRegistry) Get(ctx context.Context, tenantID int64, toolID, version string) (domain.ToolDefinition, error) {
	if err := registry.init(ctx); err != nil {
		return domain.ToolDefinition{}, err
	}
	if tenantID <= 0 || strings.TrimSpace(toolID) == "" || strings.TrimSpace(version) == "" {
		return domain.ToolDefinition{}, fmt.Errorf("%w: tenant, tool, and version are required", domain.ErrValidation)
	}
	result, err := registry.qs.Query(ctx, qToolVersionGet, tenantID, toolID, version)
	if err != nil {
		return domain.ToolDefinition{}, fmt.Errorf("get tool %s@%s: %w", toolID, version, err)
	}
	if len(result.Rows) == 0 {
		return domain.ToolDefinition{}, fmt.Errorf("%w: tool %s@%s", domain.ErrNotFound, toolID, version)
	}
	return scanToolDefinition(result.Rows[0]), nil
}

// List returns only the versions bound to the pinned agent version.
func (registry *TableToolRegistry) List(ctx context.Context, tenantID int64, agentID, agentVersion string) ([]domain.ToolDefinition, error) {
	if err := registry.init(ctx); err != nil {
		return nil, err
	}
	if tenantID <= 0 || strings.TrimSpace(agentID) == "" || strings.TrimSpace(agentVersion) == "" {
		return nil, fmt.Errorf("%w: tenant, agent, and agent version are required", domain.ErrValidation)
	}
	result, err := registry.qs.Query(ctx, qToolBoundList, tenantID, agentID, agentVersion)
	if err != nil {
		return nil, fmt.Errorf("list tools of %s@%s: %w", agentID, agentVersion, err)
	}
	tools := make([]domain.ToolDefinition, 0, len(result.Rows))
	for _, row := range result.Rows {
		tools = append(tools, scanToolDefinition(row))
	}
	return tools, nil
}

// Bind pins registered tool versions to one agent version, all or nothing.
func (registry *TableToolRegistry) Bind(ctx context.Context, tenantID int64, agentID, agentVersion string, tools []domain.ToolReference) error {
	if err := registry.init(ctx); err != nil {
		return err
	}
	if tenantID <= 0 || strings.TrimSpace(agentID) == "" || strings.TrimSpace(agentVersion) == "" || len(tools) == 0 {
		return fmt.Errorf("%w: tenant, agent, agent version, and at least one tool are required", domain.ErrValidation)
	}
	tx, err := registry.DB.BeginTx(ctx, toolRegistryQueries)
	if err != nil {
		return fmt.Errorf("begin tool binding: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = bindTools(ctx, tx, tenantID, agentID, agentVersion, tools); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tool binding: %w", err)
	}
	committed = true
	return nil
}

const toolRegistryCatalogID = "scout.toolgateway.registry"

// WriteRelease binds the tools a definition names inside the caller's publish
// transaction, so a release and its bindings commit or roll back together.
func (registry *TableToolRegistry) WriteRelease(ctx context.Context, tx port.TxQueryService, tenantID int64, definition domain.AgentDefinition) error {
	if len(definition.Tools) == 0 {
		return nil
	}
	catalog, ok := tx.(port.TxQueryCatalog)
	if !ok {
		return fmt.Errorf("%w: the publish transaction cannot bind another query catalog", domain.ErrNotReady)
	}
	return bindTools(ctx, catalog.QueryService(toolRegistryCatalogID, toolRegistryQueries), tenantID, definition.AgentID, definition.Version, definition.Tools)
}

func bindTools(ctx context.Context, tx port.QueryService, tenantID int64, agentID, agentVersion string, tools []domain.ToolReference) error {
	for _, tool := range tools {
		version, err := tx.Query(ctx, qToolVersionGet, tenantID, tool.ToolID, tool.Version)
		if err != nil {
			return fmt.Errorf("read tool version %s@%s: %w", tool.ToolID, tool.Version, err)
		}
		if len(version.Rows) == 0 {
			return fmt.Errorf("%w: tool %s@%s", domain.ErrNotFound, tool.ToolID, tool.Version)
		}
		bound, err := tx.Query(ctx, qToolBindingGet, tenantID, agentID, agentVersion, tool.ToolID)
		if err != nil {
			return fmt.Errorf("read binding of %s: %w", tool.ToolID, err)
		}
		if len(bound.Rows) > 0 {
			if pinned := common.AsString(bound.Rows[0][0]); pinned != tool.Version {
				return fmt.Errorf("%w: %s@%s already binds tool %s at %s", domain.ErrConflict, agentID, agentVersion, tool.ToolID, pinned)
			}
			continue
		}
		if _, err = tx.Query(ctx, qToolBindingInsert, tenantID, agentID, agentVersion, tool.ToolID, tool.Version); err != nil {
			return fmt.Errorf("bind tool %s@%s: %w", tool.ToolID, tool.Version, err)
		}
		bound, err = tx.Query(ctx, qToolBindingGet, tenantID, agentID, agentVersion, tool.ToolID)
		if err != nil {
			return fmt.Errorf("read inserted binding of %s: %w", tool.ToolID, err)
		}
		if len(bound.Rows) != 1 || common.AsString(bound.Rows[0][0]) != tool.Version {
			return fmt.Errorf("%w: %s@%s concurrently bound tool %s at another version", domain.ErrConflict, agentID, agentVersion, tool.ToolID)
		}
	}
	return nil
}

// normalize validates the definition and canonicalizes both schemas, so equal
// contracts compare equal however they were formatted.
func (registry *TableToolRegistry) normalize(tenantID int64, tool domain.ToolDefinition) (domain.ToolDefinition, error) {
	tool.ToolID, tool.Version = strings.TrimSpace(tool.ToolID), strings.TrimSpace(tool.Version)
	tool.DisplayName, tool.Endpoint = strings.TrimSpace(tool.DisplayName), strings.TrimSpace(tool.Endpoint)
	if tool.DisplayName == "" {
		tool.DisplayName = tool.ToolID
	}
	if tool.Timeout == 0 {
		tool.Timeout = registry.DefaultTimeout
	}
	if tool.MaxAttempts == 0 {
		tool.MaxAttempts = registry.DefaultMaxAttempts
	}
	switch {
	case tenantID <= 0 || tool.ToolID == "" || tool.Version == "" || tool.Endpoint == "":
		return tool, fmt.Errorf("%w: tenant, tool id, version, and endpoint are required", domain.ErrValidation)
	case len([]rune(tool.ToolID)) > maxToolIdentifier || len([]rune(tool.Version)) > maxToolIdentifier:
		return tool, fmt.Errorf("%w: tool id and version are limited to %d characters", domain.ErrValidation, maxToolIdentifier)
	case len([]rune(tool.DisplayName)) > maxToolDisplayName:
		return tool, fmt.Errorf("%w: tool display name is limited to %d characters", domain.ErrValidation, maxToolDisplayName)
	case tool.Timeout < time.Millisecond:
		return tool, fmt.Errorf("%w: tool timeout must be at least one millisecond", domain.ErrValidation)
	case tool.MaxAttempts < 1 || tool.MaxAttempts > 10:
		return tool, fmt.Errorf("%w: tool max attempts must be between 1 and 10", domain.ErrValidation)
	}
	tool.Timeout = tool.Timeout.Truncate(time.Millisecond)
	var err error
	if tool.InputSchema, err = canonicalSchema(tool.InputSchema); err != nil {
		return tool, fmt.Errorf("%w: input schema of %s@%s: %w", domain.ErrValidation, tool.ToolID, tool.Version, err)
	}
	if tool.OutputSchema, err = canonicalSchema(tool.OutputSchema); err != nil {
		return tool, fmt.Errorf("%w: output schema of %s@%s: %w", domain.ErrValidation, tool.ToolID, tool.Version, err)
	}
	return tool, nil
}

func canonicalSchema(raw []byte) ([]byte, error) {
	if _, err := jsonschema.Compile(raw); err != nil {
		return nil, err
	}
	return jsonschema.Canonical(raw)
}

func sameToolContract(stored, offered domain.ToolDefinition) bool {
	return stored.DisplayName == offered.DisplayName && stored.Endpoint == offered.Endpoint && stored.Timeout == offered.Timeout && stored.MaxAttempts == offered.MaxAttempts &&
		bytes.Equal(stored.InputSchema, offered.InputSchema) && bytes.Equal(stored.OutputSchema, offered.OutputSchema)
}

func scanToolDefinition(row []any) domain.ToolDefinition {
	return domain.ToolDefinition{
		ToolID: common.AsString(row[0]), Version: common.AsString(row[1]), DisplayName: common.AsString(row[2]),
		Endpoint: common.AsString(row[3]), InputSchema: []byte(common.AsString(row[4])), OutputSchema: []byte(common.AsString(row[5])),
		Timeout: time.Duration(common.AsInt64(row[6])) * time.Millisecond, MaxAttempts: int(common.AsInt64(row[7])),
	}
}

var (
	_ contract.ToolRegistry = (*TableToolRegistry)(nil)
	_ contract.ToolBinder   = (*TableToolRegistry)(nil)
)
