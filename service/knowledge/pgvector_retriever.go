package knowledge

import (
	"context"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// PgVectorRetriever is the cosine-similarity leg of a HybridRetriever over one PgVectorIndex.
type PgVectorRetriever struct {
	Index *PgVectorIndex
}

// PgTextRetriever is the tsvector keyword leg of a HybridRetriever over the same PgVectorIndex.
type PgTextRetriever struct {
	Index *PgVectorIndex
}

var _ contract.KnowledgeRetriever = (*PgVectorRetriever)(nil)
var _ contract.KnowledgeRetriever = (*PgTextRetriever)(nil)

// Retrieve delegates to PgVectorIndex.SearchVector.
func (retriever *PgVectorRetriever) Retrieve(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	if retriever.Index == nil {
		return domain.KnowledgeResult{}, fmt.Errorf("pgvector retriever: index is required")
	}
	return retriever.Index.SearchVector(ctx, query)
}

// Retrieve delegates to PgVectorIndex.SearchText.
func (retriever *PgTextRetriever) Retrieve(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	if retriever.Index == nil {
		return domain.KnowledgeResult{}, fmt.Errorf("pgvector text retriever: index is required")
	}
	return retriever.Index.SearchText(ctx, query)
}
