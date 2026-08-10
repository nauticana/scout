package mcp

import (
	"context"
	"fmt"
	"strings"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// DescriptorProvider supplies one static or query-backed field family.
type DescriptorProvider struct {
	Prefix    string
	Static    []domain.FieldDescriptor
	ListQuery string
	GetQuery  string
	Map       func(row []any) domain.FieldDescriptor
}

// BaseFieldCatalog merges and routes field descriptor families.
type BaseFieldCatalog struct {
	qs        keelport.QueryService
	providers []DescriptorProvider
}

func NewFieldCatalog(queryService keelport.QueryService, providers ...DescriptorProvider) *BaseFieldCatalog {
	return &BaseFieldCatalog{qs: queryService, providers: providers}
}

func (catalog *BaseFieldCatalog) ListFields(ctx context.Context) ([]domain.FieldDescriptor, error) {
	var fields []domain.FieldDescriptor
	for _, provider := range catalog.providers {
		fields = append(fields, provider.Static...)
		if provider.ListQuery == "" {
			continue
		}
		result, err := catalog.qs.Query(ctx, provider.ListQuery)
		if err != nil {
			return nil, fmt.Errorf("field catalog list %q: %w", provider.ListQuery, err)
		}
		for _, row := range result.Rows {
			fields = append(fields, provider.Map(row))
		}
	}
	return fields, nil
}

func (catalog *BaseFieldCatalog) DescribeField(ctx context.Context, name string) (*domain.FieldDescriptor, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("field name is required")
	}
	for _, provider := range catalog.providers {
		if provider.Prefix == "" || !strings.HasPrefix(name, provider.Prefix) {
			continue
		}
		if provider.GetQuery == "" {
			break
		}
		result, err := catalog.qs.Query(ctx, provider.GetQuery, strings.TrimPrefix(name, provider.Prefix))
		if err != nil {
			return nil, fmt.Errorf("field catalog describe %q: %w", provider.GetQuery, err)
		}
		if len(result.Rows) == 0 {
			return nil, nil
		}
		field := provider.Map(result.Rows[0])
		return &field, nil
	}
	fields, err := catalog.ListFields(ctx)
	if err != nil {
		return nil, err
	}
	for i := range fields {
		if fields[i].Name == name {
			return &fields[i], nil
		}
	}
	return nil, nil
}

var _ contract.MCPFieldCatalog = (*BaseFieldCatalog)(nil)
