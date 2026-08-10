package mcp

import (
	"context"
	"testing"

	"github.com/nauticana/keel/common"
	keelmodel "github.com/nauticana/keel/model"

	"github.com/nauticana/scout/domain"
)

type fieldQueryFake struct {
	rows map[string][][]any
}

func (query fieldQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	rows := query.rows[name]
	if len(args) == 1 {
		var selected [][]any
		for _, row := range rows {
			if common.AsString(row[0]) == common.AsString(args[0]) {
				selected = append(selected, row)
			}
		}
		rows = selected
	}
	return &keelmodel.QueryResult{Rows: rows}, nil
}

func (fieldQueryFake) GenID() int64 { return 0 }

func testFieldCatalog() *BaseFieldCatalog {
	mapper := func(row []any) domain.FieldDescriptor {
		return domain.FieldDescriptor{Name: "attr." + common.AsString(row[0]), Kind: "attribute", Label: common.AsString(row[1])}
	}
	return NewFieldCatalog(fieldQueryFake{rows: map[string][][]any{
		"attrs": {{"wifi", "Wi-Fi"}, {"parking", "Parking"}},
		"attr":  {{"wifi", "Wi-Fi"}, {"parking", "Parking"}},
	}},
		DescriptorProvider{Static: []domain.FieldDescriptor{{Name: "name", Kind: "core"}}},
		DescriptorProvider{Prefix: "attr.", ListQuery: "attrs", GetQuery: "attr", Map: mapper},
	)
}

func TestBaseFieldCatalog(t *testing.T) {
	catalog := testFieldCatalog()
	fields, err := catalog.ListFields(context.Background())
	if err != nil || len(fields) != 3 {
		t.Fatalf("fields = %+v, err = %v", fields, err)
	}
	field, err := catalog.DescribeField(context.Background(), "attr.parking")
	if err != nil || field == nil || field.Label != "Parking" {
		t.Fatalf("field = %+v, err = %v", field, err)
	}
}
