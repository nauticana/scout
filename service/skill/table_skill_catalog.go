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
	qCatalogOriginInsert  = "scout_skill_catalog_origin_insert"
	qCatalogImported      = "scout_skill_catalog_imported"
	qCatalogToolList      = "scout_skill_catalog_tool_list"
	qCatalogToolInsert    = "scout_skill_catalog_tool_insert"
	qCatalogExampleList   = "scout_skill_catalog_example_list"
	qCatalogExampleInsert = "scout_skill_catalog_example_insert"
	qCatalogRequireList   = "scout_skill_catalog_require_list"
	qCatalogRequireInsert = "scout_skill_catalog_require_insert"
	qCatalogDerived       = "scout_skill_catalog_derived"
	qCatalogDependents    = "scout_skill_catalog_dependents"
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
SELECT v.skill_id, v.skill_version, p.display_name, v.summary, v.instructions, v.input_schema,
       o.kind_code, o.locator, o.revision, o.digest, o.license
  FROM skill_catalog_version v
  JOIN skill_catalog_profile p ON p.skill_id = v.skill_id
  LEFT JOIN skill_catalog_origin o ON o.skill_id = v.skill_id AND o.skill_version = v.skill_version
 WHERE v.skill_id = ? AND v.skill_version = ?`,
	qCatalogVersionInsert: `
INSERT INTO skill_catalog_version (skill_id, skill_version, summary, instructions, input_schema)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (skill_id, skill_version) DO NOTHING`,
	qCatalogOriginInsert: `
INSERT INTO skill_catalog_origin (skill_id, skill_version, kind_code, locator, revision, digest, license)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (skill_id, skill_version) DO NOTHING`,
	qCatalogImported: `
SELECT skill_id, skill_version
  FROM skill_catalog_origin
 WHERE locator = ? OR digest = ?
 ORDER BY skill_id, skill_version`,
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
	qCatalogRequireList: `
SELECT required_skill_id, required_skill_version
  FROM skill_catalog_requirement
 WHERE skill_id = ? AND skill_version = ?
 ORDER BY required_skill_id`,
	qCatalogRequireInsert: `
INSERT INTO skill_catalog_requirement (skill_id, skill_version, required_skill_id, required_skill_version)
VALUES (?, ?, ?, ?)
ON CONFLICT (skill_id, skill_version, required_skill_id) DO NOTHING`,
	qCatalogDerived: `
SELECT partner_id, skill_id, skill_version
  FROM skill_version
 WHERE origin_skill_id = ? AND origin_skill_version = ?
 ORDER BY partner_id, skill_id, skill_version`,
	qCatalogDependents: `
SELECT skill_id, skill_version
  FROM skill_catalog_requirement
 WHERE required_skill_id = ? AND required_skill_version = ?
 ORDER BY skill_id, skill_version`,
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
// A required catalog version must already exist, so requirements cannot cycle.
func (catalog *TableSkillCatalog) Register(ctx context.Context, skill domain.SkillDefinition) error {
	if err := catalog.init(ctx); err != nil {
		return err
	}
	if skill.EvalSet != nil || skill.DerivedFrom != nil {
		return fmt.Errorf("%w: a catalog skill names no eval set or origin; its tenant copies do", domain.ErrValidation)
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
	if err = ensureProfile(ctx, tx, qCatalogProfileEnsure, qCatalogProfileLock, skill.DisplayName, skill.SkillID); err != nil {
		return err
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

// EnsureProfile creates a catalog skill profile before its first version, refusing an
// existing profile with a different display name as Register does.
func (catalog *TableSkillCatalog) EnsureProfile(ctx context.Context, skillID, displayName string) error {
	if err := catalog.init(ctx); err != nil {
		return err
	}
	skillID, displayName, err := normalizeProfile(skillID, displayName)
	if err != nil {
		return err
	}
	return ensureProfile(ctx, catalog.qs, qCatalogProfileEnsure, qCatalogProfileLock, displayName, skillID)
}

func insertCatalogVersion(ctx context.Context, tx port.QueryService, skill domain.SkillDefinition) error {
	for _, required := range skill.Requires {
		if _, found, err := readCatalogVersion(ctx, tx, required.SkillID, required.Version); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("%w: catalog skill %s@%s requires unregistered catalog skill %s@%s", domain.ErrNotFound,
				skill.SkillID, skill.Version, required.SkillID, required.Version)
		}
	}
	if _, err := tx.Query(ctx, qCatalogVersionInsert, skill.SkillID, skill.Version, skill.Summary, skill.Procedure,
		nullIfBlank(string(skill.InputSchema))); err != nil {
		return fmt.Errorf("insert catalog skill version %s@%s: %w", skill.SkillID, skill.Version, err)
	}
	if origin := skill.Origin; origin != nil {
		if _, err := tx.Query(ctx, qCatalogOriginInsert, skill.SkillID, skill.Version, string(origin.Kind), origin.Locator,
			nullIfBlank(origin.Revision), nullIfBlank(origin.Digest), nullIfBlank(origin.License)); err != nil {
			return fmt.Errorf("insert origin of catalog skill %s@%s: %w", skill.SkillID, skill.Version, err)
		}
	}
	for _, required := range skill.Requires {
		if _, err := tx.Query(ctx, qCatalogRequireInsert, skill.SkillID, skill.Version, required.SkillID, required.Version); err != nil {
			return fmt.Errorf("insert requirement %s of catalog skill %s@%s: %w", required.SkillID, skill.SkillID, skill.Version, err)
		}
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
	if err := catalog.ready(ctx, origin); err != nil {
		return nil, err
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

// Imported lists every catalog version whose origin has the locator or the digest.
func (catalog *TableSkillCatalog) Imported(ctx context.Context, locator, digest string) ([]domain.SkillReference, error) {
	if err := catalog.init(ctx); err != nil {
		return nil, err
	}
	locator, digest = strings.TrimSpace(locator), strings.TrimSpace(digest)
	if locator == "" && digest == "" {
		return nil, fmt.Errorf("%w: a locator or a digest is required", domain.ErrValidation)
	}
	result, err := catalog.qs.Query(ctx, qCatalogImported, locator, digest)
	if err != nil {
		return nil, fmt.Errorf("list catalog skills imported from %q %q: %w", locator, digest, err)
	}
	return scanReferences(result.Rows), nil
}

// Dependents lists every catalog version requiring the catalog version.
func (catalog *TableSkillCatalog) Dependents(ctx context.Context, required domain.SkillReference) ([]domain.SkillReference, error) {
	if err := catalog.ready(ctx, required); err != nil {
		return nil, err
	}
	result, err := catalog.qs.Query(ctx, qCatalogDependents, required.SkillID, required.Version)
	if err != nil {
		return nil, fmt.Errorf("list dependents of catalog skill %s@%s: %w", required.SkillID, required.Version, err)
	}
	return scanReferences(result.Rows), nil
}

func (catalog *TableSkillCatalog) ready(ctx context.Context, reference domain.SkillReference) error {
	if err := catalog.init(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(reference.SkillID) == "" || strings.TrimSpace(reference.Version) == "" {
		return fmt.Errorf("%w: skill and version are required", domain.ErrValidation)
	}
	return nil
}

func nullIfBlank(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func scanReferences(rows [][]any) []domain.SkillReference {
	references := make([]domain.SkillReference, len(rows))
	for i, row := range rows {
		references[i] = domain.SkillReference{SkillID: common.AsString(row[0]), Version: common.AsString(row[1])}
	}
	return references
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
	if kind := common.AsString(row[6]); kind != "" {
		skill.Origin = &domain.SkillOrigin{Kind: domain.SkillOriginKind(kind), Locator: common.AsString(row[7]),
			Revision: common.AsString(row[8]), Digest: common.AsString(row[9]), License: common.AsString(row[10])}
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
	requires, err := qs.Query(ctx, qCatalogRequireList, skillID, version)
	if err != nil {
		return skill, false, fmt.Errorf("list requirements of catalog skill %s@%s: %w", skillID, version, err)
	}
	if len(requires.Rows) > 0 {
		skill.Requires = scanReferences(requires.Rows)
	}
	return skill, true, nil
}
