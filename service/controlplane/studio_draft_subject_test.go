package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// A frozen draft is digested exactly as Publish digests it, and nothing is written.
func TestFreezeDraftDigestsLikePublishWithoutPersisting(t *testing.T) {
	query := &studioQueryFake{rows: draftRows(), args: map[string][]any{}}
	service := testDraftService(query, fake.DraftTester{})
	frozen, err := service.FreezeDraft(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatalf("FreezeDraft: %v", err)
	}
	definition := frozen.Definition
	if definition.AgentID != "writer-a" || len(definition.DefinitionDigest) != 64 || definition.SettingsDigest == "" {
		t.Fatalf("frozen definition = %+v", definition)
	}
	recomputed, err := service.Compiler.DefinitionDigest(definition)
	if err != nil || recomputed != definition.DefinitionDigest {
		t.Fatalf("digest %q does not cover the frozen definition (%q, %v)", definition.DefinitionDigest, recomputed, err)
	}
	subject := frozen.Subject()
	if subject.Kind != domain.SubjectDraft || subject.AgentID != "writer-a" || subject.DefinitionDigest != definition.DefinitionDigest || subject.Version != "" {
		t.Fatalf("subject = %+v", subject)
	}
	for _, write := range []string{qStudioInsertVersion, qStudioAudit} {
		if _, written := query.args[write]; written {
			t.Fatalf("freezing a draft wrote %s", write)
		}
	}
}

// The draft subject an evaluation pins must be the release Publish writes for the same draft.
func TestFreezeDraftDigestEqualsThePublishedDigest(t *testing.T) {
	rows := draftRows()
	rows[qStudioLockDraft] = [][]any{{int64(4), "writer"}}
	rows[qStudioLockAlias] = [][]any{{"writer-a", int64(2)}}
	rows[qStudioNextVersion] = [][]any{{int64(1)}}
	query := &studioQueryFake{rows: rows, args: map[string][]any{}}
	service := testDraftService(query, fake.DraftTester{})
	frozen, err := service.FreezeDraft(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatalf("FreezeDraft: %v", err)
	}
	if _, err = service.Publish(context.Background(), humanActor(8, 9), domain.AgentPublishRequest{
		AgentID: "writer-a", ChangeSummary: "initial", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published := query.args[qStudioInsertVersion][4]; published != frozen.Definition.DefinitionDigest {
		t.Fatalf("published digest %v differs from the frozen draft's %q", published, frozen.Definition.DefinitionDigest)
	}
}

func TestFreezeDraftRequiresTestGrant(t *testing.T) {
	query := &studioQueryFake{rows: draftRows(), args: map[string][]any{}}
	service := testDraftService(query, fake.DraftTester{})
	service.Roles = fake.PrincipalAuthorizerFunc(func(_ context.Context, _ domain.Principal, _, action, _ string) (domain.AuthorizationGrant, error) {
		return domain.AuthorizationGrant{Allowed: action != string(domain.StudioTest)}, nil
	})
	if _, err := service.FreezeDraft(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("FreezeDraft without TEST = %v, want forbidden", err)
	}
	if _, read := query.args[qStudioGetDraft]; read {
		t.Fatal("an unauthorized freeze read the draft")
	}
}
