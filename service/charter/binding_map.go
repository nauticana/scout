package charter

import (
	"context"

	"github.com/nauticana/charter/sdk/corpus"

	"github.com/nauticana/scout/domain"
)

// BindingMap is BindingTools fixed at composition, the same for every tenant.
type BindingMap map[corpus.DocumentKey]domain.ToolReference

var _ BindingTools = BindingMap(nil)

func (m BindingMap) Tool(_ context.Context, _ int64, key corpus.DocumentKey) (domain.ToolReference, bool, error) {
	tool, found := m[key]
	return tool, found, nil
}
