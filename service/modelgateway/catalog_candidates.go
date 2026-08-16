package modelgateway

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qCandidateModels       = "scout_modelgateway_candidate_models"
	qCandidateCapabilities = "scout_modelgateway_candidate_capabilities"
)

var candidateCatalogQueries = map[string]string{
	qCandidateModels: `
SELECT d.provider_id, d.model_id, d.context_token_limit, d.output_token_limit
  FROM tenant_model_access a
  JOIN model_definition d ON d.provider_id = a.provider_id AND d.model_id = a.model_id
  JOIN model_provider p ON p.provider_id = d.provider_id
 WHERE a.tenant_id = ? AND d.is_active AND p.is_active
 ORDER BY d.provider_id, d.model_id`,
	qCandidateCapabilities: `
SELECT c.provider_id, c.model_id, c.capability_code
  FROM tenant_model_access a
  JOIN model_capability c ON c.provider_id = a.provider_id AND c.model_id = a.model_id
 WHERE a.tenant_id = ?
 ORDER BY c.provider_id, c.model_id, c.capability_code`,
}

// TableCandidateCatalog derives a tenant's candidate set from model_definition,
// model_capability, and tenant_model_access. The schema carries no version,
// region, route, or quality columns, so each model is one route (RouteID
// provider/model) in the configured Region with quality class zero, and the
// generation is a content hash of the returned rows.
type TableCandidateCatalog struct {
	DB keelport.DatabaseRepository
	// Region stamps every candidate with the deployment's residency region; empty leaves it unknown.
	Region string

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.ModelCandidateCatalog = (*TableCandidateCatalog)(nil)

func (catalog *TableCandidateCatalog) init(ctx context.Context) error {
	if catalog.DB == nil {
		return fmt.Errorf("candidate catalog: database is required")
	}
	catalog.once.Do(func() { catalog.qs = catalog.DB.GetQueryService(ctx, candidateCatalogQueries) })
	if catalog.qs == nil {
		return fmt.Errorf("candidate catalog: query service is required")
	}
	return nil
}

// CandidatesFor lists the tenant's active, accessible models as routes.
func (catalog *TableCandidateCatalog) CandidatesFor(ctx context.Context, tenant domain.TenantContext) (domain.ModelCandidateSet, error) {
	if tenant.TenantID <= 0 {
		return domain.ModelCandidateSet{}, fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	if err := catalog.init(ctx); err != nil {
		return domain.ModelCandidateSet{}, err
	}
	capabilities, err := catalog.qs.Query(ctx, qCandidateCapabilities, tenant.TenantID)
	if err != nil {
		return domain.ModelCandidateSet{}, fmt.Errorf("list candidate capabilities: %w", err)
	}
	byModel := make(map[domain.ModelReference][]string, len(capabilities.Rows))
	for _, row := range capabilities.Rows {
		reference := domain.ModelReference{ProviderID: strings.TrimSpace(common.AsString(row[0])), ModelID: strings.TrimSpace(common.AsString(row[1]))}
		byModel[reference] = append(byModel[reference], strings.TrimSpace(common.AsString(row[2])))
	}
	models, err := catalog.qs.Query(ctx, qCandidateModels, tenant.TenantID)
	if err != nil {
		return domain.ModelCandidateSet{}, fmt.Errorf("list candidate models: %w", err)
	}
	region := strings.TrimSpace(catalog.Region)
	hash := fnv.New64a()
	set := domain.ModelCandidateSet{Candidates: make([]domain.ModelCandidate, 0, len(models.Rows))}
	for _, row := range models.Rows {
		reference := domain.ModelReference{ProviderID: strings.TrimSpace(common.AsString(row[0])), ModelID: strings.TrimSpace(common.AsString(row[1]))}
		candidate := domain.ModelCandidate{
			Provider:         reference.ProviderID,
			Model:            reference.ModelID,
			Region:           region,
			RouteID:          reference.ProviderID + "/" + reference.ModelID,
			Capabilities:     byModel[reference],
			MaxContextTokens: common.AsInt64(row[2]),
			MaxOutputTokens:  common.AsInt64(row[3]),
		}
		set.Candidates = append(set.Candidates, candidate)
		_, _ = hash.Write([]byte(candidate.RouteID + "|" + strconv.FormatInt(candidate.MaxContextTokens, 10) + "|" +
			strconv.FormatInt(candidate.MaxOutputTokens, 10) + "|" + strings.Join(candidate.Capabilities, ",") + "\n"))
	}
	set.Generation = int64(hash.Sum64() & math.MaxInt64)
	return set, nil
}
