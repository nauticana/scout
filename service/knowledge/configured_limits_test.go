package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestHybridRetrieverRRFOffset(t *testing.T) {
	single := retrieverFunc(func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{Matches: matches("a", "b")}, nil
	})
	for name, tc := range map[string]struct {
		offset int
		want   float64
	}{
		"default":    {want: 1.0 / float64(DefaultRRFOffset+1)},
		"configured": {offset: 1, want: 0.5},
	} {
		result, err := (&HybridRetriever{Legs: legs(single), RRFOffset: tc.offset}).Retrieve(context.Background(), hybridQuery)
		if err != nil || len(result.Matches) == 0 || result.Matches[0].Score != tc.want {
			t.Fatalf("%s: result = %+v, %v; want head score %v", name, result.Matches, err, tc.want)
		}
	}
	if _, err := (&HybridRetriever{Legs: legs(single), RRFOffset: -1}).Retrieve(context.Background(), hybridQuery); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative offset must be refused, got %v", err)
	}
}

func TestObjectStorageLoaderRefusesNegativeMaxBytes(t *testing.T) {
	loader := &ObjectStorageLoader{Storage: &fake.ObjectStorage{Name: "chunks"}, MaxBytes: -1}
	if _, err := loader.Load(context.Background(), domain.KnowledgeDocument{SourceURI: "object://chunks/a"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative max bytes must be refused, got %v", err)
	}
}
