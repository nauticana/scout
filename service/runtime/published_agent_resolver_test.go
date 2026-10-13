package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	keelmodel "github.com/nauticana/keel/model"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/release"
)

type publishedQueryFake struct {
	rows     [][]any
	byName   map[string][][]any
	lastArgs map[string][]any
}

func (query publishedQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if query.lastArgs != nil {
		query.lastArgs[name] = append([]any(nil), args...)
	}
	if rows, ok := query.byName[name]; ok {
		return &keelmodel.QueryResult{Rows: rows}, nil
	}
	return &keelmodel.QueryResult{Rows: query.rows}, nil
}

func (publishedQueryFake) GenID() int64 { return 0 }

func encodedDefinition(t *testing.T, version string) string {
	t.Helper()
	encoded, err := json.Marshal(domain.AgentDefinition{
		AgentID: "writer-a", AgentTypeID: "writer", Version: version,
		DefinitionDigest: "digest", Languages: []domain.CompiledPrompt{{LanguageCode: "en-US"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestPublishedAgentResolverStable(t *testing.T) {
	resolver := &PublishedAgentResolver{qs: publishedQueryFake{rows: [][]any{{
		true, "3", nil, int64(0), encodedDefinition(t, "3"), nil, "writer-a", true,
	}}}}
	definition, err := resolver.Resolve(context.Background(), 8, "writer", "", "en-US", "conversation-a")
	if err != nil || definition.Version != "3" {
		t.Fatalf("definition = %+v, err = %v", definition, err)
	}
}

func TestPublishedAgentResolverNamedAgentNeverFallsBack(t *testing.T) {
	named := publishedQueryFake{rows: [][]any{{
		true, "5", nil, int64(0), encodedDefinition(t, "5"), nil, "writer-b", true,
	}}, lastArgs: map[string][]any{}}
	resolver := &PublishedAgentResolver{qs: named}
	if _, err := resolver.Resolve(context.Background(), 8, "writer", "writer-b", "en-US", ""); err != nil {
		t.Fatal(err)
	}
	if args := named.lastArgs[qPublishedAgent]; len(args) != 3 || args[0] != "writer-b" || args[1] != int64(8) || args[2] != "writer" {
		t.Fatalf("args = %v", args)
	}
	other := &PublishedAgentResolver{qs: publishedQueryFake{}}
	if _, err := other.Resolve(context.Background(), 8, "writer", "editor-a", "en-US", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("agent of another type err = %v, want not found", err)
	}
	disabled := &PublishedAgentResolver{qs: publishedQueryFake{rows: [][]any{{
		false, "5", nil, int64(0), encodedDefinition(t, "5"), nil, "writer-b", true,
	}}}}
	if _, err := disabled.Resolve(context.Background(), 8, "writer", "writer-b", "en-US", ""); !notReady(err, "writer-b", domain.AgentNotReadyInactive) {
		t.Fatalf("inactive named agent err = %v, want not ready", err)
	}
}

func notReady(err error, agentID string, reason domain.AgentNotReadyReason) bool {
	var typed *domain.AgentNotReadyError
	return errors.Is(err, domain.ErrNotReady) && errors.As(err, &typed) && typed.AgentID == agentID && typed.Reason == reason
}

func TestPublishedAgentResolverRejectsDisabledAndMissingLanguage(t *testing.T) {
	disabled := &PublishedAgentResolver{qs: publishedQueryFake{rows: [][]any{{
		true, "3", nil, int64(0), encodedDefinition(t, "3"), nil, "writer-a", false,
	}}}}
	if _, err := disabled.Resolve(context.Background(), 8, "writer", "", "en-US", ""); !notReady(err, "writer-a", domain.AgentNotReadyDisabled) {
		t.Fatalf("disabled err = %v", err)
	}
	unpublished := &PublishedAgentResolver{qs: publishedQueryFake{}}
	if _, err := unpublished.Resolve(context.Background(), 8, "writer", "", "en-US", ""); !notReady(err, "", domain.AgentNotReadyUnpublished) {
		t.Fatalf("unpublished err = %v", err)
	}
	namedUnpublished := &PublishedAgentResolver{qs: publishedQueryFake{rows: [][]any{{
		true, nil, nil, int64(0), nil, nil, "writer-a", true,
	}}}}
	if _, err := namedUnpublished.Resolve(context.Background(), 8, "writer", "writer-a", "en-US", ""); !notReady(err, "writer-a", domain.AgentNotReadyUnpublished) {
		t.Fatalf("named unpublished err = %v", err)
	}

	missing := &PublishedAgentResolver{qs: publishedQueryFake{rows: [][]any{{
		true, "3", nil, int64(0), encodedDefinition(t, "3"), nil, "writer-a", true,
	}}}}
	if _, err := missing.Resolve(context.Background(), 8, "writer", "", "fr-FR", ""); !notReady(err, "writer-a", domain.AgentNotReadyNoLanguage) {
		t.Fatalf("missing language err = %v", err)
	}
}

func TestCanarySelectionIsSticky(t *testing.T) {
	first := release.CanarySelected(8, "writer", "conversation-a", 50)
	for range 10 {
		if release.CanarySelected(8, "writer", "conversation-a", 50) != first {
			t.Fatal("canary selection changed for one conversation")
		}
	}
}

type versionManagerFake string

func (version versionManagerFake) ResolveVersion(context.Context, domain.VersionRequest) (string, error) {
	return string(version), nil
}
func (versionManagerFake) SetCanary(context.Context, int64, string, string, int) error { return nil }
func (versionManagerFake) Promote(context.Context, int64, string, string) error        { return nil }
func (versionManagerFake) Rollback(context.Context, int64, string) error               { return nil }

func TestPublishedAgentResolverDelegatesToTrafficManager(t *testing.T) {
	query := publishedQueryFake{rows: [][]any{{
		true, "3", "4", int64(0), encodedDefinition(t, "3"), encodedDefinition(t, "4"), "writer-a", true,
	}}}
	// Percentage zero would keep the reference hash on stable; the manager wins.
	resolver := &PublishedAgentResolver{qs: query, Versions: versionManagerFake("4")}
	definition, err := resolver.Resolve(context.Background(), 8, "writer", "", "en-US", "conversation-a")
	if err != nil || definition.Version != "4" {
		t.Fatalf("definition = %+v, err = %v", definition, err)
	}

	pinned := publishedQueryFake{
		rows:     [][]any{{true, "3", nil, int64(0), encodedDefinition(t, "3"), nil, "writer-a", true}},
		byName:   map[string][][]any{qPublishedAgentVersion: {{encodedDefinition(t, "9")}}},
		lastArgs: map[string][]any{},
	}
	off := &PublishedAgentResolver{qs: pinned, Versions: versionManagerFake("9")}
	definition, err = off.Resolve(context.Background(), 8, "writer", "", "en-US", "conversation-a")
	if err != nil || definition.Version != "9" {
		t.Fatalf("pinned definition = %+v, err = %v", definition, err)
	}
	args := pinned.lastArgs[qPublishedAgentVersion]
	if len(args) != 3 || args[0] != int64(8) || args[1] != "writer-a" || args[2] != "9" {
		t.Fatalf("version args = %v", args)
	}
}
