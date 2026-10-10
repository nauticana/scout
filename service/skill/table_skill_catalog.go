package skill

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qCatalogProfileEnsure = "scout_skill_catalog_profile_ensure"
	qCatalogProfileLock   = "scout_skill_catalog_profile_lock"
	qCatalogVersionGet    = "scout_skill_catalog_version_get"
	qCatalogVersionInsert = "scout_skill_catalog_version_insert"
	qCatalogToolList      = "scout_skill_catalog_tool_list"
	qCatalogToolInsert    = "scout_skill_catalog_tool_insert"
	qCatalogExampleList   = "scout_skill_catalog_example_list"
	qCatalogExampleInsert = "scout_skill_catalog_example_insert"
	qCatalogDerived       = "scout_skill_catalog_derived"
)

var skillCatalogQueries = map[string]string{
	qCatalogProfileEnsure: `
INSERT INTO skill_catalog_profile (skill_id, display_name)
VALUES (?, ?)
ON CONFLICT (skill_id) DO NOTHING`,
	qCatalogProfileLock: `
SELECT display_name
  FROM skill_catalog_profile
 WHERE skill_id = ?
   FOR UPDATE`,
	qCatalogVersionGet: `
SELECT v.skill_id, v.skill_version, p.display_name, v.summary, v.instructions, v.input_schema
  FROM skill_catalog_version v
  JOIN skill_catalog_profile p ON p.skill_id = v.skill_id
 WHERE v.skill_id = ? AND v.skill_version = ?`,
	qCatalogVersionInsert: `
INSERT INTO skill_catalog_version (skill_id, skill_version, summary, instructions, input_schema)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (skill_id, skill_version) DO NOTHING`,
	qCatalogToolList: `
SELECT tool_id
  FROM skill_catalog_tool
 WHERE skill_id = ? AND skill_version = ?
 ORDER BY tool_id`,
	qCatalogToolInsert: `
INSERT INTO skill_catalog_tool (skill_id, skill_version, tool_id)
VALUES (?, ?, ?)
ON CONFLICT (skill_id, skill_version, tool_id) DO NOTHING`,
	qCatalogExampleList: `
SELECT request
  FROM skill_catalog_example
 WHERE skill_id = ? AND skill_version = ?
 ORDER BY example_no`,
	qCatalogExampleInsert: `
INSERT INTO skill_catalog_example (skill_id, skill_version, example_no, request)
VALUES (?, ?, ?, ?)
ON CONFLICT (skill_id, skill_version, example_no) DO NOTHING`,
	qCatalogDerived: `
SELECT partner_id, skill_id, skill_version
  FROM skill_version
 WHERE origin_skill_id = ? AND origin_skill_version = ?
 ORDER BY partner_id, skill_id, skill_version`,
}

// TableSkillCatalog is the SkillCatalog over the skill_catalog tables. It has no
// tenant: callers restrict its writers to a platform principal. Versions are
// immutable under the same rules as TableSkillRegistry.
type TableSkillCatalog struct {
	DB port.DatabaseRepository

	once sync.Once
	qs   port.QueryService
}

var _ contract.SkillCatalog = (*TableSkillCatalog)(nil)

func (catalog *TableSkillCatalog) init(ctx context.Context) error {
	if catalog.DB == nil {
		return fmt.Errorf("skill catalog: database is required")
	}
	catalog.once.Do(func() { catalog.qs = catalog.DB.GetQueryService(ctx, skillCatalogQueries) })
	if catalog.qs == nil {
		return fmt.Errorf("skill catalog: query service is required")
	}
	return nil
}

// Register publishes one immutable catalog version, creating its profile with it.
// Tool ids are stored as given; each tenant copy must name tools that tenant registered.
func (catalog *TableSkillCatalog) Register(ctx context.Context, skill domain.SkillDefinition) error {
	if err := catalog.init(ctx); err != nil {
		return err
	}
	if skill.EvalSet != nil || skill.DerivedFrom != nil || len(skill.Requires) > 0 {
		return fmt.Errorf("%w: a catalog skill names no eval set, origin, or requirement; its tenant copies do", domain.ErrValidation)
	}
	skill, err := normalize(skill)
	if err != nil {
		return err
	}
	tx, err := catalog.DB.BeginTx(ctx, skillCatalogQueries)
	if err != nil {
		return fmt.Errorf("begin catalog skill registration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if _, err = tx.Query(ctx, qCatalogProfileEnsure, skill.SkillID, skill.DisplayName); err != nil {
		return fmt.Errorf("ensure catalog skill profile %q: %w", skill.SkillID, err)
	}
	profile, err := tx.Query(ctx, qCatalogProfileLock, skill.SkillID)
	if err != nil {
		return fmt.Errorf("read catalog skill profile %q: %w", skill.SkillID, err)
	}
	if len(profile.Rows) != 1 || common.AsString(profile.Rows[0][0]) != skill.DisplayName {
		return fmt.Errorf("%w: catalog skill %s is already registered with a different display name", domain.ErrConflict, skill.SkillID)
	}
	existing, found, err := readCatalogVersion(ctx, tx, skill.SkillID, skill.Version)
	if err != nil {
		return err
	}
	if !found {
		if err = insertCatalogVersion(ctx, tx, skill); err != nil {
			return err
		}
		if existing, found, err = readCatalogVersion(ctx, tx, skill.SkillID, skill.Version); err != nil {
			return err
		}
	}
	if !found || !sameSkill(existing, skill) {
		return fmt.Errorf("%w: catalog skill %s@%s is already registered with different content", domain.ErrConflict, skill.SkillID, skill.Version)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit catalog skill registration: %w", err)
	}
	committed = true
	return nil
}

func insertCatalogVersion(ctx context.Context, tx port.QueryService, skill domain.SkillDefinition) error {
	if _, err := tx.Query(ctx, qCatalogVersionInsert, skill.SkillID, skill.Version, skill.Summary, skill.Procedure,
		inputSchemaValue(skill.InputSchema)); err != nil {
		return fmt.Errorf("insert catalog skill version %s@%s: %w", skill.SkillID, skill.Version, err)
	}
	for _, toolID := range skill.Tools {
		if _, err := tx.Query(ctx, qCatalogToolInsert, skill.SkillID, skill.Version, toolID); err != nil {
			return fmt.Errorf("insert tool %q of catalog skill %s@%s: %w", toolID, skill.SkillID, skill.Version, err)
		}
	}
	for i, request := range skill.Examples {
		if _, err := tx.Query(ctx, qCatalogExampleInsert, skill.SkillID, skill.Version, i+1, request); err != nil {
			return fmt.Errorf("insert example %d of catalog skill %s@%s: %w", i+1, skill.SkillID, skill.Version, err)
		}
	}
	return nil
}

// Get returns one catalog version.
func (catalog *TableSkillCatalog) Get(ctx context.Context, skillID, version string) (domain.SkillDefinition, error) {
	if err := catalog.init(ctx); err != nil {
		return domain.SkillDefinition{}, err
	}
	if strings.TrimSpace(skillID) == "" || strings.TrimSpace(version) == "" {
		return domain.SkillDefinition{}, fmt.Errorf("%w: skill and version are required", domain.ErrValidation)
	}
	skill, found, err := readCatalogVersion(ctx, catalog.qs, skillID, version)
	if err != nil {
		return domain.SkillDefinition{}, err
	}
	if !found {
		return domain.SkillDefinition{}, fmt.Errorf("%w: catalog skill %s@%s", domain.ErrNotFound, skillID, version)
	}
	return skill, nil
}

// Derived lists every tenant version copied from the catalog version.
func (catalog *TableSkillCatalog) Derived(ctx context.Context, origin domain.SkillReference) ([]domain.TenantSkillReference, error) {
	if err := catalog.init(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(origin.SkillID) == "" || strings.TrimSpace(origin.Version) == "" {
		return nil, fmt.Errorf("%w: skill and version are required", domain.ErrValidation)
	}
	result, err := catalog.qs.Query(ctx, qCatalogDerived, origin.SkillID, origin.Version)
	if err != nil {
		return nil, fmt.Errorf("list copies of catalog skill %s@%s: %w", origin.SkillID, origin.Version, err)
	}
	derived := make([]domain.TenantSkillReference, len(result.Rows))
	for i, row := range result.Rows {
		derived[i] = domain.TenantSkillReference{TenantID: common.AsInt64(row[0]), SkillID: common.AsString(row[1]), Version: common.AsString(row[2])}
	}
	return derived, nil
}

func readCatalogVersion(ctx context.Context, qs port.QueryService, skillID, version string) (domain.SkillDefinition, bool, error) {
	result, err := qs.Query(ctx, qCatalogVersionGet, skillID, version)
	if err != nil {
		return domain.SkillDefinition{}, false, fmt.Errorf("read catalog skill version %s@%s: %w", skillID, version, err)
	}
	if len(result.Rows) == 0 {
		return domain.SkillDefinition{}, false, nil
	}
	row := result.Rows[0]
	skill := domain.SkillDefinition{
		SkillID: common.AsString(row[0]), Version: common.AsString(row[1]), DisplayName: common.AsString(row[2]),
		Summary: common.AsString(row[3]), Procedure: common.AsString(row[4]),
	}
	if inputSchema := common.AsString(row[5]); inputSchema != "" {
		skill.InputSchema = []byte(inputSchema)
	}
	tools, err := qs.Query(ctx, qCatalogToolList, skillID, version)
	if err != nil {
		return skill, false, fmt.Errorf("list tools of catalog skill %s@%s: %w", skillID, version, err)
	}
	for _, row := range tools.Rows {
		skill.Tools = append(skill.Tools, common.AsString(row[0]))
	}
	examples, err := qs.Query(ctx, qCatalogExampleList, skillID, version)
	if err != nil {
		return skill, false, fmt.Errorf("list examples of catalog skill %s@%s: %w", skillID, version, err)
	}
	for _, row := range examples.Rows {
		skill.Examples = append(skill.Examples, common.AsString(row[0]))
	}
	return skill, true, nil
}
