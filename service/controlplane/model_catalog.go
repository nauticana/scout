package controlplane

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

// Usage categories reported by ModelCatalog rates.
const (
	RateInputPerMillion  = "input_per_million"
	RateOutputPerMillion = "output_per_million"
	RateImage            = "image"
	RateVideoSecond      = "video_second"
)

const (
	qCatalogModels       = "scout_catalog_models"
	qCatalogCapabilities = "scout_catalog_capabilities"
	qCatalogPrices       = "scout_catalog_prices"
	qCatalogModel        = "scout_catalog_model"
)

var modelCatalogQueries = map[string]string{
	qCatalogModels: `
SELECT provider_id, model_id, display_name, context_token_limit, output_token_limit, is_active
  FROM model_definition
 ORDER BY provider_id, display_name, model_id`,
	qCatalogCapabilities: `SELECT provider_id, model_id, capability_code FROM model_capability`,
	// Newest effective price per model and currency.
	qCatalogPrices: `
SELECT DISTINCT ON (provider_id, model_id, currency_code)
       provider_id, model_id, currency_code,
       input_minor_units_per_million, output_minor_units_per_million,
       image_minor_units, video_minor_units_per_second
  FROM model_price
 WHERE effective_at <= CURRENT_TIMESTAMP
 ORDER BY provider_id, model_id, currency_code, effective_at DESC`,
	qCatalogModel: `
SELECT d.is_active, c.capability_code
  FROM model_definition d
  LEFT JOIN model_capability c ON c.provider_id = d.provider_id AND c.model_id = d.model_id
 WHERE d.provider_id = ? AND d.model_id = ?`,
}

// ModelCatalog serves the tenant-selectable models from the Scout registry.
// Tenant scoping and product credit scales are layered by decorating it.
type ModelCatalog struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.StudioModelCatalog = (*ModelCatalog)(nil)

func (c *ModelCatalog) init(ctx context.Context) error {
	if c.qs != nil {
		return nil
	}
	if c.DB == nil {
		return fmt.Errorf("model catalog: database is required")
	}
	c.once.Do(func() { c.qs = c.DB.GetQueryService(ctx, modelCatalogQueries) })
	return nil
}

func (c *ModelCatalog) List(ctx context.Context, _ int64) ([]domain.StudioModel, error) {
	if err := c.init(ctx); err != nil {
		return nil, err
	}
	capabilities, err := c.capabilities(ctx)
	if err != nil {
		return nil, err
	}
	rates, err := c.rates(ctx)
	if err != nil {
		return nil, err
	}
	res, err := c.qs.Query(ctx, qCatalogModels)
	if err != nil {
		return nil, fmt.Errorf("list model definitions: %w", err)
	}
	models := make([]domain.StudioModel, 0, len(res.Rows))
	for _, row := range res.Rows {
		reference := domain.ModelReference{
			ProviderID: strings.TrimSpace(common.AsString(row[0])),
			ModelID:    strings.TrimSpace(common.AsString(row[1])),
		}
		models = append(models, domain.StudioModel{
			Reference:         reference,
			DisplayName:       common.AsString(row[2]),
			Capabilities:      capabilities[reference],
			ContextTokenLimit: common.AsInt64(row[3]),
			OutputTokenLimit:  common.AsInt64(row[4]),
			Active:            common.AsBool(row[5]),
			Rates:             rates[reference],
		})
	}
	return models, nil
}

// Validate reports selections that do not exist, are withdrawn, or cannot
// serve the modality they were assigned to.
func (c *ModelCatalog) Validate(ctx context.Context, _ int64, selection domain.AgentModelSelection) ([]domain.AgentFieldError, error) {
	if err := c.init(ctx); err != nil {
		return nil, err
	}
	fields := make([]domain.AgentFieldError, 0)
	for _, check := range []struct {
		field      string
		reference  *domain.ModelReference
		capability string
	}{
		{"models.text", selection.Text, CapabilityText},
		{"models.image", selection.Image, CapabilityImage},
		{"models.video", selection.Video, CapabilityVideo},
	} {
		if check.reference == nil {
			continue
		}
		if err := c.validate(ctx, &fields, check.field, *check.reference, check.capability); err != nil {
			return nil, err
		}
	}
	return fields, nil
}

// Capability codes seeded as the model_capability constant domain.
const (
	CapabilityText  = "text"
	CapabilityImage = "image"
	CapabilityVideo = "video"
)

func (c *ModelCatalog) validate(ctx context.Context, fields *[]domain.AgentFieldError, field string, reference domain.ModelReference, capability string) error {
	res, err := c.qs.Query(ctx, qCatalogModel, reference.ProviderID, reference.ModelID)
	if err != nil {
		return fmt.Errorf("check model %s/%s: %w", reference.ProviderID, reference.ModelID, err)
	}
	if len(res.Rows) == 0 {
		*fields = append(*fields, domain.AgentFieldError{Field: field, Message: "unknown model " + reference.ModelID})
		return nil
	}
	if !common.AsBool(res.Rows[0][0]) {
		*fields = append(*fields, domain.AgentFieldError{Field: field, Message: "model " + reference.ModelID + " is not active"})
	}
	for _, row := range res.Rows {
		if strings.TrimSpace(common.AsString(row[1])) == capability {
			return nil
		}
	}
	*fields = append(*fields, domain.AgentFieldError{Field: field, Message: "model " + reference.ModelID + " cannot generate " + capability})
	return nil
}

func (c *ModelCatalog) capabilities(ctx context.Context) (map[domain.ModelReference][]string, error) {
	res, err := c.qs.Query(ctx, qCatalogCapabilities)
	if err != nil {
		return nil, fmt.Errorf("list model capabilities: %w", err)
	}
	capabilities := map[domain.ModelReference][]string{}
	for _, row := range res.Rows {
		reference := domain.ModelReference{
			ProviderID: strings.TrimSpace(common.AsString(row[0])),
			ModelID:    strings.TrimSpace(common.AsString(row[1])),
		}
		capabilities[reference] = append(capabilities[reference], strings.TrimSpace(common.AsString(row[2])))
	}
	return capabilities, nil
}

func (c *ModelCatalog) rates(ctx context.Context) (map[domain.ModelReference][]domain.ModelRate, error) {
	res, err := c.qs.Query(ctx, qCatalogPrices)
	if err != nil {
		return nil, fmt.Errorf("list model prices: %w", err)
	}
	rates := map[domain.ModelReference][]domain.ModelRate{}
	for _, row := range res.Rows {
		reference := domain.ModelReference{
			ProviderID: strings.TrimSpace(common.AsString(row[0])),
			ModelID:    strings.TrimSpace(common.AsString(row[1])),
		}
		currency := strings.TrimSpace(common.AsString(row[2]))
		for _, rate := range []struct {
			category string
			amount   int64
		}{
			{RateInputPerMillion, common.AsInt64(row[3])},
			{RateOutputPerMillion, common.AsInt64(row[4])},
			{RateImage, common.AsInt64(row[5])},
			{RateVideoSecond, common.AsInt64(row[6])},
		} {
			if rate.amount > 0 {
				rates[reference] = append(rates[reference], domain.ModelRate{
					UsageCategory: rate.category, Unit: rate.category,
					AmountMinor: rate.amount, Currency: currency,
				})
			}
		}
	}
	return rates, nil
}
