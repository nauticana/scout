package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestDeployedAgentIndexReportsStateAndDefinition(t *testing.T) {
	index := &DeployedAgentIndex{qs: publishedQueryFake{rows: [][]any{
		{"writer", "writer", "writer-a", true, true, "3", encodedDefinition(t, "3")},
		{"replier", "replier", "replier-a", true, false, "", nil},
		{"scorer", "scorer", "scorer-a", false, true, "1", encodedDefinition(t, "1")},
	}}}
	deployed, err := index.List(context.Background(), 8)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(deployed) != 3 {
		t.Fatalf("got %d aliases, want 3", len(deployed))
	}
	if writer := deployed["writer"]; writer.Definition == nil || writer.Definition.Version != "3" || !writer.Enabled {
		t.Fatalf("writer = %+v", writer)
	}
	// No stable deployment means no definition, not an error.
	if replier := deployed["replier"]; replier.Definition != nil || replier.Enabled {
		t.Fatalf("replier = %+v", replier)
	}
	// A deployed definition still reports the profile kill switch.
	if scorer := deployed["scorer"]; scorer.Active || scorer.Definition == nil {
		t.Fatalf("scorer = %+v", scorer)
	}
}

func TestDeployedAgentIndexRejectsCorruptDefinition(t *testing.T) {
	index := &DeployedAgentIndex{qs: publishedQueryFake{rows: [][]any{
		{"writer", "writer", "writer-a", true, true, "3", "{not json"},
	}}}
	if _, err := index.List(context.Background(), 8); err == nil {
		t.Fatal("a corrupt definition must not be reported as absent")
	}
}

func TestDeployedAgentIndexRequiresTenant(t *testing.T) {
	index := &DeployedAgentIndex{qs: publishedQueryFake{}}
	if _, err := index.List(context.Background(), 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}
