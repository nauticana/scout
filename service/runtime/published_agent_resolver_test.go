package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	keelmodel "github.com/nauticana/keel/model"

	"github.com/nauticana/scout/domain"
)

type publishedQueryFake struct {
	rows [][]any
}

func (query publishedQueryFake) Query(context.Context, string, ...any) (*keelmodel.QueryResult, error) {
	return &keelmodel.QueryResult{Rows: query.rows}, nil
}

func (publishedQueryFake) GenID() int64 { return 0 }

func encodedDefinition(t *testing.T, version string) string {
	t.Helper()
	encoded, err := json.Marshal(domain.AgentDefinition{
		AgentID: "writer-a", AgentKind: "writer", Version: version,
		DefinitionDigest: "digest", Languages: []domain.CompiledPrompt{{LanguageCode: "en-US"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestPublishedAgentResolverStable(t *testing.T) {
	resolver := &PublishedAgentResolver{qs: publishedQueryFake{rows: [][]any{{
		true, "3", nil, int64(0), encodedDefinition(t, "3"), nil,
	}}}}
	definition, err := resolver.Resolve(context.Background(), 8, "writer", "en-US", "conversation-a")
	if err != nil || definition.Version != "3" {
		t.Fatalf("definition = %+v, err = %v", definition, err)
	}
}

func TestPublishedAgentResolverRejectsDisabledAndMissingLanguage(t *testing.T) {
	disabled := &PublishedAgentResolver{qs: publishedQueryFake{rows: [][]any{{
		false, "3", nil, int64(0), encodedDefinition(t, "3"), nil,
	}}}}
	if _, err := disabled.Resolve(context.Background(), 8, "writer", "en-US", ""); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("disabled err = %v", err)
	}

	missing := &PublishedAgentResolver{qs: publishedQueryFake{rows: [][]any{{
		true, "3", nil, int64(0), encodedDefinition(t, "3"), nil,
	}}}}
	if _, err := missing.Resolve(context.Background(), 8, "writer", "fr-FR", ""); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("missing language err = %v", err)
	}
}

func TestCanarySelectionIsSticky(t *testing.T) {
	first := canarySelected(8, "writer", "conversation-a", 50)
	for range 10 {
		if canarySelected(8, "writer", "conversation-a", 50) != first {
			t.Fatal("canary selection changed for one conversation")
		}
	}
}
