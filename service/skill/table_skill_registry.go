package skill

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/jsonschema"
)

const (
	qSkillProfileEnsure  = "scout_skill_profile_ensure"
	qSkillProfileLock    = "scout_skill_profile_lock"
	qSkillToolProfileGet = "scout_skill_tool_profile_get"
	qSkillUseToolSchema  = "scout_skill_use_tool_schema"
	qSkillVersionGet     = "scout_skill_version_get"
	qSkillVersionInsert  = "scout_skill_version_insert"
	qSkillToolList       = "scout_skill_tool_list"
	qSkillToolInsert     = "scout_skill_tool_insert"
	qSkillExampleList    = "scout_skill_example_list"
	qSkillExampleInsert  = "scout_skill_example_insert"
	qSkillBoundList      = "scout_skill_bound_list"
	qSkillBoundTools     = "scout_skill_bound_tools"
	qSkillBoundExamples  = "scout_skill_bound_examples"
	qSkillBindingGet     = "scout_skill_binding_get"
	qSkillBindingInsert  = "scout_skill_binding_insert"

	skillVersionColumns = `v.skill_id, v.skill_version, p.display_name, v.summary, v.instructions, v.input_schema, v.golden_set_id, v.golden_set_version`

	maxSkillIdentifier  = 80
	maxSkillDisplayName = 200
	maxSkillSummary     = 400
)

var skillRegistryQueries = map[string]string{
	qSkillProfileEnsure: `
INSERT INTO skill_profile (tenant_id, skill_id, display_name)
VALUES (?, ?, ?)
ON CONFLICT (tenant_id, skill_id) DO NOTHING`,
	qSkillProfileLock: `
SELECT display_name
  FROM skill_profile
 WHERE tenant_id = ? AND skill_id = ?
   FOR UPDATE`,
	qSkillToolProfileGet: `
SELECT tool_id
  FROM tool_profile
 WHERE tenant_id = ? AND tool_id = ?`,
	qSkillUseToolSchema: `
SELECT input_schema
  FROM tool_version
 WHERE tenant_id = ? AND tool_id = ? AND tool_version = ?`,
	qSkillVersionGet: `
SELECT ` + skillVersionColumns + `
  FROM skill_version v
  JOIN skill_profile p ON p.tenant_id = v.tenant_id AND p.skill_id = v.skill_id
 WHERE v.tenant_id = ? AND v.skill_id = ? AND v.skill_version = ?`,
	qSkillVersionInsert: `
INSERT INTO skill_version (tenant_id, skill_id, skill_version, summary, instructions, input_schema, golden_set_id, golden_set_version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, skill_id, skill_version) DO NOTHING`,
	qSkillToolList: `
SELECT tool_id
  FROM skill_tool
 WHERE tenant_id = ? AND skill_id = ? AND skill_version = ?
 ORDER BY tool_id`,
	qSkillToolInsert: `
INSERT INTO skill_tool (tenant_id, skill_id, skill_version, tool_id)
VALUES (?, ?, ?, ?)
ON CONFLICT (tenant_id, skill_id, skill_version, tool_id) DO NOTHING`,
	qSkillExampleList: `
SELECT request
  FROM skill_example
 WHERE tenant_id = ? AND skill_id = ? AND skill_version = ?
 ORDER BY example_no`,
	qSkillExampleInsert: `
INSERT INTO skill_example (tenant_id, skill_id, skill_version, example_no, request)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, skill_id, skill_version, example_no) DO NOTHING`,
	qSkillBoundList: `
SELECT ` + skillVersionColumns + `
  FROM agent_skill_binding b
  JOIN skill_version v ON v.tenant_id = b.tenant_id AND v.skill_id = b.skill_id AND v.skill_version = b.skill_version
  JOIN skill_profile p ON p.tenant_id = v.tenant_id AND p.skill_id = v.skill_id
 WHERE b.tenant_id = ? AND b.agent_id = ? AND b.agent_version = ?
 ORDER BY v.skill_id`,
	qSkillBoundTools: `
SELECT t.skill_id, t.tool_id
  FROM agent_skill_binding b
  JOIN skill_tool t ON t.tenant_id = b.tenant_id AND t.skill_id = b.skill_id AND t.skill_version = b.skill_version
 WHERE b.tenant_id = ? AND b.agent_id = ? AND b.agent_version = ?
 ORDER BY t.skill_id, t.tool_id`,
	qSkillBoundExamples: `
SELECT e.skill_id, e.request
  FROM agent_skill_binding b
  JOIN skill_example e ON e.tenant_id = b.tenant_id AND e.skill_id = b.skill_id AND e.skill_version = b.skill_version
 WHERE b.tenant_id = ? AND b.agent_id = ? AND b.agent_version = ?
 ORDER BY e.skill_id, e.example_no`,
	qSkillBindingGet: `
SELECT skill_version
  FROM agent_skill_binding
 WHERE tenant_id = ? AND agent_id = ? AND agent_version = ? AND skill_id = ?`,
	qSkillBindingInsert: `
INSERT INTO agent_skill_binding (tenant_id, agent_id, agent_version, skill_id, skill_version)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, agent_id, agent_version, skill_id) DO NOTHING`,
}

const skillRegistryCatalogID = "scout.skill.registry"

// TableSkillRegistry is the SkillRegistry over skill_profile, skill_version, skill_tool,
// skill_example, and agent_skill_binding, and the release writer that binds a published
// definition's skills. Versions and bindings are immutable: repeating identical content
// is a no-op, different content under the same key is ErrConflict.
type TableSkillRegistry struct {
	DB port.DatabaseRepository

	once sync.Once
	qs   port.QueryService
}

var _ contract.SkillRegistry = (*TableSkillRegistry)(nil)

func (registry *TableSkillRegistry) init(ctx context.Context) error {
	if registry.DB == nil {
		return fmt.Errorf("skill registry: database is required")
	}
	registry.once.Do(func() { registry.qs = registry.DB.GetQueryService(ctx, skillRegistryQueries) })
	if registry.qs == nil {
		return fmt.Errorf("skill registry: query service is required")
	}
	return nil
}

// Register publishes one immutable version, creating the skill profile with it. The
// profile row is locked because a version spans three tables.
func (registry *TableSkillRegistry) Register(ctx context.Context, tenantID int64, skill domain.SkillDefinition) error {
	if err := registry.init(ctx); err != nil {
		return err
	}
	skill, err := normalize(tenantID, skill)
	if err != nil {
		return err
	}
	tx, err := registry.DB.BeginTx(ctx, skillRegistryQueries)
	if err != nil {
		return fmt.Errorf("begin skill registration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if _, err = tx.Query(ctx, qSkillProfileEnsure, tenantID, skill.SkillID, skill.DisplayName); err != nil {
		return fmt.Errorf("ensure skill profile %q: %w", skill.SkillID, err)
	}
	profile, err := tx.Query(ctx, qSkillProfileLock, tenantID, skill.SkillID)
	if err != nil {
		return fmt.Errorf("read skill profile %q: %w", skill.SkillID, err)
	}
	if len(profile.Rows) != 1 || common.AsString(profile.Rows[0][0]) != skill.DisplayName {
		return fmt.Errorf("%w: skill %s is already registered with a different display name", domain.ErrConflict, skill.SkillID)
	}
	existing, found, err := readVersion(ctx, tx, tenantID, skill.SkillID, skill.Version)
	if err != nil {
		return err
	}
	if found {
		if !sameSkill(existing, skill) {
			return fmt.Errorf("%w: skill %s@%s is already registered with different content", domain.ErrConflict, skill.SkillID, skill.Version)
		}
		return nil
	}
	if err = insertVersion(ctx, tx, tenantID, skill); err != nil {
		return err
	}
	stored, found, err := readVersion(ctx, tx, tenantID, skill.SkillID, skill.Version)
	if err != nil {
		return err
	}
	if !found || !sameSkill(stored, skill) {
		return fmt.Errorf("%w: skill %s@%s was concurrently registered with different content", domain.ErrConflict, skill.SkillID, skill.Version)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit skill registration: %w", err)
	}
	committed = true
	return nil
}

func insertVersion(ctx context.Context, tx port.QueryService, tenantID int64, skill domain.SkillDefinition) error {
	var goldenSetID, goldenSetVersion any
	if skill.EvalSet != nil {
		goldenSetID, goldenSetVersion = skill.EvalSet.GoldenSetID, skill.EvalSet.SetVersion
	}
	var inputSchema any
	if len(skill.InputSchema) > 0 {
		inputSchema = string(skill.InputSchema)
	}
	if _, err := tx.Query(ctx, qSkillVersionInsert, tenantID, skill.SkillID, skill.Version, skill.Summary, skill.Procedure,
		inputSchema, goldenSetID, goldenSetVersion); err != nil {
		return fmt.Errorf("insert skill version %s@%s: %w", skill.SkillID, skill.Version, err)
	}
	for _, toolID := range skill.Tools {
		profile, err := tx.Query(ctx, qSkillToolProfileGet, tenantID, toolID)
		if err != nil {
			return fmt.Errorf("read tool profile %q: %w", toolID, err)
		}
		if len(profile.Rows) == 0 {
			return fmt.Errorf("%w: skill %s@%s uses unregistered tool %q", domain.ErrNotFound, skill.SkillID, skill.Version, toolID)
		}
		if _, err = tx.Query(ctx, qSkillToolInsert, tenantID, skill.SkillID, skill.Version, toolID); err != nil {
			return fmt.Errorf("insert tool %q of skill %s@%s: %w", toolID, skill.SkillID, skill.Version, err)
		}
	}
	for i, request := range skill.Examples {
		if _, err := tx.Query(ctx, qSkillExampleInsert, tenantID, skill.SkillID, skill.Version, i+1, request); err != nil {
			return fmt.Errorf("insert example %d of skill %s@%s: %w", i+1, skill.SkillID, skill.Version, err)
		}
	}
	return nil
}

// Get returns one version owned by the tenant; another tenant's skill is ErrNotFound.
func (registry *TableSkillRegistry) Get(ctx context.Context, tenantID int64, skillID, version string) (domain.SkillDefinition, error) {
	if err := registry.init(ctx); err != nil {
		return domain.SkillDefinition{}, err
	}
	if tenantID <= 0 || strings.TrimSpace(skillID) == "" || strings.TrimSpace(version) == "" {
		return domain.SkillDefinition{}, fmt.Errorf("%w: tenant, skill, and version are required", domain.ErrValidation)
	}
	skill, found, err := readVersion(ctx, registry.qs, tenantID, skillID, version)
	if err != nil {
		return domain.SkillDefinition{}, err
	}
	if !found {
		return domain.SkillDefinition{}, fmt.Errorf("%w: skill %s@%s", domain.ErrNotFound, skillID, version)
	}
	return skill, nil
}

// List returns only the versions bound to the pinned agent version.
func (registry *TableSkillRegistry) List(ctx context.Context, tenantID int64, agentID, agentVersion string) ([]domain.SkillDefinition, error) {
	if err := registry.init(ctx); err != nil {
		return nil, err
	}
	if tenantID <= 0 || strings.TrimSpace(agentID) == "" || strings.TrimSpace(agentVersion) == "" {
		return nil, fmt.Errorf("%w: tenant, agent, and agent version are required", domain.ErrValidation)
	}
	result, err := registry.qs.Query(ctx, qSkillBoundList, tenantID, agentID, agentVersion)
	if err != nil {
		return nil, fmt.Errorf("list skills of %s@%s: %w", agentID, agentVersion, err)
	}
	skills := make([]domain.SkillDefinition, 0, len(result.Rows))
	index := make(map[string]int, len(result.Rows))
	for _, row := range result.Rows {
		skill := scanSkillVersion(row)
		index[skill.SkillID] = len(skills)
		skills = append(skills, skill)
	}
	for query, assign := range map[string]func(*domain.SkillDefinition, string){
		qSkillBoundTools:    func(skill *domain.SkillDefinition, toolID string) { skill.Tools = append(skill.Tools, toolID) },
		qSkillBoundExamples: func(skill *domain.SkillDefinition, request string) { skill.Examples = append(skill.Examples, request) },
	} {
		rows, err := registry.qs.Query(ctx, query, tenantID, agentID, agentVersion)
		if err != nil {
			return nil, fmt.Errorf("list skill details of %s@%s: %w", agentID, agentVersion, err)
		}
		for _, row := range rows.Rows {
			if at, ok := index[common.AsString(row[0])]; ok {
				assign(&skills[at], common.AsString(row[1]))
			}
		}
	}
	return skills, nil
}

// WriteRelease binds the skills a definition names inside the caller's publish
// transaction. Every tool a bound skill uses must be bound to the same release,
// and so must use_skill, or the agent could load a procedure it cannot follow.
func (registry *TableSkillRegistry) WriteRelease(ctx context.Context, tx port.TxQueryService, tenantID int64, definition domain.AgentDefinition) error {
	if len(definition.Skills) == 0 {
		return nil
	}
	catalog, ok := tx.(port.TxQueryCatalog)
	if !ok {
		return fmt.Errorf("%w: the publish transaction cannot bind another query catalog", domain.ErrNotReady)
	}
	qs := catalog.QueryService(skillRegistryCatalogID, skillRegistryQueries)
	boundTools := make(map[string]string, len(definition.Tools))
	for _, tool := range definition.Tools {
		boundTools[tool.ToolID] = tool.Version
	}
	useSkillVersion, ok := boundTools[domain.UseSkillToolID]
	if !ok {
		return fmt.Errorf("%w: %s@%s binds skills but not the %s tool", domain.ErrValidation, definition.AgentID, definition.Version, domain.UseSkillToolID)
	}
	listed, err := useSkillEnum(ctx, qs, tenantID, useSkillVersion)
	if err != nil {
		return err
	}
	for _, reference := range definition.Skills {
		if listed != nil && !slices.Contains(listed, reference.SkillID) {
			return fmt.Errorf("%w: %s@%s does not enumerate skill %s, which %s@%s binds", domain.ErrValidation,
				domain.UseSkillToolID, useSkillVersion, reference.SkillID, definition.AgentID, definition.Version)
		}
		skill, found, err := readVersion(ctx, qs, tenantID, reference.SkillID, reference.Version)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: skill %s@%s", domain.ErrNotFound, reference.SkillID, reference.Version)
		}
		for _, toolID := range skill.Tools {
			if _, ok := boundTools[toolID]; !ok {
				return fmt.Errorf("%w: skill %s@%s uses tool %q, which %s@%s does not bind", domain.ErrValidation,
					reference.SkillID, reference.Version, toolID, definition.AgentID, definition.Version)
			}
		}
		if err = bindSkill(ctx, qs, tenantID, definition.AgentID, definition.Version, reference); err != nil {
			return err
		}
	}
	return nil
}

// useSkillEnum reads the skill ids the bound use_skill version enumerates; nil for an open contract.
func useSkillEnum(ctx context.Context, qs port.QueryService, tenantID int64, version string) ([]string, error) {
	res, err := qs.Query(ctx, qSkillUseToolSchema, tenantID, domain.UseSkillToolID, version)
	if err != nil {
		return nil, fmt.Errorf("read %s@%s: %w", domain.UseSkillToolID, version, err)
	}
	if len(res.Rows) == 0 {
		return nil, fmt.Errorf("%w: tool %s@%s", domain.ErrNotFound, domain.UseSkillToolID, version)
	}
	return enumeratedSkills([]byte(common.AsString(res.Rows[0][0])))
}

func bindSkill(ctx context.Context, tx port.QueryService, tenantID int64, agentID, agentVersion string, skill domain.SkillReference) error {
	bound, err := tx.Query(ctx, qSkillBindingGet, tenantID, agentID, agentVersion, skill.SkillID)
	if err != nil {
		return fmt.Errorf("read binding of skill %s: %w", skill.SkillID, err)
	}
	if len(bound.Rows) > 0 {
		if pinned := common.AsString(bound.Rows[0][0]); pinned != skill.Version {
			return fmt.Errorf("%w: %s@%s already binds skill %s at %s", domain.ErrConflict, agentID, agentVersion, skill.SkillID, pinned)
		}
		return nil
	}
	if _, err = tx.Query(ctx, qSkillBindingInsert, tenantID, agentID, agentVersion, skill.SkillID, skill.Version); err != nil {
		return fmt.Errorf("bind skill %s@%s: %w", skill.SkillID, skill.Version, err)
	}
	bound, err = tx.Query(ctx, qSkillBindingGet, tenantID, agentID, agentVersion, skill.SkillID)
	if err != nil {
		return fmt.Errorf("read inserted binding of skill %s: %w", skill.SkillID, err)
	}
	if len(bound.Rows) != 1 || common.AsString(bound.Rows[0][0]) != skill.Version {
		return fmt.Errorf("%w: %s@%s concurrently bound skill %s at another version", domain.ErrConflict, agentID, agentVersion, skill.SkillID)
	}
	return nil
}

func readVersion(ctx context.Context, qs port.QueryService, tenantID int64, skillID, version string) (domain.SkillDefinition, bool, error) {
	result, err := qs.Query(ctx, qSkillVersionGet, tenantID, skillID, version)
	if err != nil {
		return domain.SkillDefinition{}, false, fmt.Errorf("read skill version %s@%s: %w", skillID, version, err)
	}
	if len(result.Rows) == 0 {
		return domain.SkillDefinition{}, false, nil
	}
	skill, err := completeVersion(ctx, qs, tenantID, scanSkillVersion(result.Rows[0]))
	return skill, err == nil, err
}

// completeVersion adds the tool and example rows to a scanned version row.
func completeVersion(ctx context.Context, qs port.QueryService, tenantID int64, skill domain.SkillDefinition) (domain.SkillDefinition, error) {
	tools, err := qs.Query(ctx, qSkillToolList, tenantID, skill.SkillID, skill.Version)
	if err != nil {
		return skill, fmt.Errorf("list tools of skill %s@%s: %w", skill.SkillID, skill.Version, err)
	}
	for _, row := range tools.Rows {
		skill.Tools = append(skill.Tools, common.AsString(row[0]))
	}
	examples, err := qs.Query(ctx, qSkillExampleList, tenantID, skill.SkillID, skill.Version)
	if err != nil {
		return skill, fmt.Errorf("list examples of skill %s@%s: %w", skill.SkillID, skill.Version, err)
	}
	for _, row := range examples.Rows {
		skill.Examples = append(skill.Examples, common.AsString(row[0]))
	}
	return skill, nil
}

func scanSkillVersion(row []any) domain.SkillDefinition {
	skill := domain.SkillDefinition{
		SkillID: common.AsString(row[0]), Version: common.AsString(row[1]), DisplayName: common.AsString(row[2]),
		Summary: common.AsString(row[3]), Procedure: common.AsString(row[4]),
	}
	if inputSchema := common.AsString(row[5]); inputSchema != "" {
		skill.InputSchema = []byte(inputSchema)
	}
	if goldenSetID := common.AsString(row[6]); goldenSetID != "" {
		skill.EvalSet = &domain.GoldenSetReference{GoldenSetID: goldenSetID, SetVersion: common.AsInt64(row[7])}
	}
	return skill
}

// normalize validates the definition and canonicalizes it, so equal skills
// compare equal however they were formatted.
func normalize(tenantID int64, skill domain.SkillDefinition) (domain.SkillDefinition, error) {
	skill.SkillID, skill.Version = strings.TrimSpace(skill.SkillID), strings.TrimSpace(skill.Version)
	skill.DisplayName, skill.Summary = strings.TrimSpace(skill.DisplayName), strings.TrimSpace(skill.Summary)
	skill.Procedure = strings.TrimSpace(skill.Procedure)
	if skill.DisplayName == "" {
		skill.DisplayName = skill.SkillID
	}
	switch {
	case tenantID <= 0 || skill.SkillID == "" || skill.Version == "" || skill.Summary == "" || skill.Procedure == "":
		return skill, fmt.Errorf("%w: tenant, skill id, version, summary, and procedure are required", domain.ErrValidation)
	case len([]rune(skill.SkillID)) > maxSkillIdentifier || len([]rune(skill.Version)) > maxSkillIdentifier:
		return skill, fmt.Errorf("%w: skill id and version are limited to %d characters", domain.ErrValidation, maxSkillIdentifier)
	case len([]rune(skill.DisplayName)) > maxSkillDisplayName:
		return skill, fmt.Errorf("%w: skill display name is limited to %d characters", domain.ErrValidation, maxSkillDisplayName)
	case len([]rune(skill.Summary)) > maxSkillSummary || strings.ContainsAny(skill.Summary, "\r\n"):
		return skill, fmt.Errorf("%w: a skill summary is one line of at most %d characters", domain.ErrValidation, maxSkillSummary)
	case skill.EvalSet != nil && (strings.TrimSpace(skill.EvalSet.GoldenSetID) == "" || skill.EvalSet.SetVersion <= 0):
		return skill, fmt.Errorf("%w: a skill's eval set needs a golden set id and a positive version", domain.ErrValidation)
	}
	tools := make([]string, 0, len(skill.Tools))
	for _, toolID := range skill.Tools {
		toolID = strings.TrimSpace(toolID)
		if toolID == "" || toolID == domain.UseSkillToolID {
			return skill, fmt.Errorf("%w: skill %s lists an empty tool id or %s", domain.ErrValidation, skill.SkillID, domain.UseSkillToolID)
		}
		tools = append(tools, toolID)
	}
	slices.Sort(tools)
	skill.Tools = slices.Compact(tools)
	examples := make([]string, 0, len(skill.Examples))
	for _, request := range skill.Examples {
		if request = strings.TrimSpace(request); request == "" {
			return skill, fmt.Errorf("%w: skill %s has an empty example request", domain.ErrValidation, skill.SkillID)
		}
		examples = append(examples, request)
	}
	skill.Examples = examples
	if len(skill.InputSchema) > 0 {
		if _, err := jsonschema.Compile(skill.InputSchema); err != nil {
			return skill, fmt.Errorf("%w: input schema of skill %s@%s: %w", domain.ErrValidation, skill.SkillID, skill.Version, err)
		}
		canonical, err := jsonschema.Canonical(skill.InputSchema)
		if err != nil {
			return skill, fmt.Errorf("%w: input schema of skill %s@%s: %w", domain.ErrValidation, skill.SkillID, skill.Version, err)
		}
		skill.InputSchema = canonical
	}
	return skill, nil
}

func sameSkill(stored, offered domain.SkillDefinition) bool {
	return stored.DisplayName == offered.DisplayName && stored.Summary == offered.Summary && stored.Procedure == offered.Procedure &&
		bytes.Equal(stored.InputSchema, offered.InputSchema) && slices.Equal(stored.Tools, offered.Tools) &&
		slices.Equal(stored.Examples, offered.Examples) && sameEvalSet(stored.EvalSet, offered.EvalSet)
}

func sameEvalSet(stored, offered *domain.GoldenSetReference) bool {
	if stored == nil || offered == nil {
		return stored == offered
	}
	return *stored == *offered
}
