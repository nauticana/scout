package runtime

import (
	"context"
	"testing"

	"github.com/nauticana/scout/domain"
)

type versionRequestRecorder struct {
	versionManagerFake
	asked []domain.VersionRequest
}

func (recorder *versionRequestRecorder) ResolveVersion(_ context.Context, request domain.VersionRequest) (string, error) {
	recorder.asked = append(recorder.asked, request)
	return string(recorder.versionManagerFake), nil
}

func TestPublishedAgentResolverAsksForTheServingAgentWithoutAPerson(t *testing.T) {
	query := publishedQueryFake{rows: [][]any{{
		true, "3", "4", int64(0), encodedDefinition(t, "3"), encodedDefinition(t, "4"), "writer-a",
	}}}
	versions := &versionRequestRecorder{versionManagerFake: "3"}
	resolver := &PublishedAgentResolver{qs: query, Versions: versions}
	if _, err := resolver.Resolve(context.Background(), 8, "writer", "", "en-US", "conversation-a"); err != nil {
		t.Fatal(err)
	}
	// An alias resolution carries no session, so it names no person and no pilot can apply.
	want := domain.VersionRequest{TenantID: 8, AgentID: "writer-a", ConversationID: "conversation-a"}
	if len(versions.asked) != 1 || versions.asked[0] != want {
		t.Fatalf("asked = %+v, want %+v", versions.asked, want)
	}
}
