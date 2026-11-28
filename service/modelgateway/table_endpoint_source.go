package modelgateway

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const qModelEndpoint = "scout_modelgateway_model_endpoint"

var endpointQueries = map[string]string{
	qModelEndpoint: `
SELECT protocol_code, endpoint_uri, credential_ref, max_concurrency
  FROM model_provider
 WHERE provider_id = ? AND is_active`,
}

// TableEndpointSource reads registered endpoints from model_provider.
type TableEndpointSource struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.ModelEndpointSource = (*TableEndpointSource)(nil)

// ModelEndpoint returns the provider's active row.
func (source *TableEndpointSource) ModelEndpoint(ctx context.Context, providerID string) (domain.ModelEndpoint, error) {
	if source.DB == nil {
		return domain.ModelEndpoint{}, fmt.Errorf("endpoint source: database is required")
	}
	source.once.Do(func() { source.qs = source.DB.GetQueryService(ctx, endpointQueries) })
	if source.qs == nil {
		return domain.ModelEndpoint{}, fmt.Errorf("endpoint source: query service is required")
	}
	result, err := source.qs.Query(ctx, qModelEndpoint, providerID)
	if err != nil {
		return domain.ModelEndpoint{}, fmt.Errorf("read model endpoint %q: %w", providerID, err)
	}
	if len(result.Rows) == 0 {
		return domain.ModelEndpoint{}, fmt.Errorf("%w: model provider %q", domain.ErrNotFound, providerID)
	}
	row := result.Rows[0]
	return domain.ModelEndpoint{
		ProviderID:     providerID,
		Protocol:       domain.ModelProtocol(strings.TrimSpace(common.AsString(row[0]))),
		BaseURL:        strings.TrimSpace(common.AsString(row[1])),
		CredentialRef:  strings.TrimSpace(common.AsString(row[2])),
		MaxConcurrency: int(max(0, common.AsInt64(row[3]))),
	}, nil
}
