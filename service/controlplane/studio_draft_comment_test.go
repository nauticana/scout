package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func commentService() (*StudioDraftComments, *studioQueryFake) {
	studio, query := publishService()
	return &StudioDraftComments{Studio: studio}, query
}

func TestCommentIsAnchoredToTheRevisionTheAuthorSaw(t *testing.T) {
	comments, query := commentService()
	query.rows[qCommentInsert] = [][]any{{int64(5), reviewedAt}}
	comment, err := comments.Comment(context.Background(), humanActor(8, 9), domain.DraftComment{
		AgentID: "writer-a", DraftRevision: 4, Anchor: domain.CommentOnPromptSection, PromptSectionID: 1, LanguageCode: "en-US", Body: " shorter please ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if comment.CommentID != 5 || comment.Status != domain.DraftCommentOpen || comment.Author.ID != "9" || comment.Body != "shorter please" {
		t.Fatalf("comment = %+v", comment)
	}
	args := query.args[qCommentInsert]
	if args[0] != string(domain.CommentOnPromptSection) || args[1] != int64(1) || args[2] != "en-US" || args[3] != nil || args[7] != "9" || args[10] != int64(4) {
		t.Fatalf("insert args = %v", args)
	}

	_, err = comments.Comment(context.Background(), humanActor(8, 9), domain.DraftComment{
		AgentID: "writer-a", DraftRevision: 4, Anchor: domain.CommentOnSetting, ResourceKind: domain.ResourceBudget, ResourceID: "budget", Body: "too high",
	})
	if args = query.args[qCommentInsert]; err != nil || args[1] != nil || args[3] != "budget" || args[4] != "budget" {
		t.Fatalf("setting comment = %v, args %v", err, args)
	}

	query.rows[qCommentInsert] = nil
	if _, err = comments.Comment(context.Background(), humanActor(8, 9), domain.DraftComment{
		AgentID: "writer-a", DraftRevision: 3, Anchor: domain.CommentOnDraft, Body: "stale",
	}); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("stale revision = %v", err)
	}
}

func TestCommentValidatesItsAnchorAndBody(t *testing.T) {
	comments, query := commentService()
	for name, comment := range map[string]domain.DraftComment{
		"unknown anchor":           {Anchor: "page", Body: "x", DraftRevision: 4},
		"empty body":               {Anchor: domain.CommentOnDraft, Body: "  ", DraftRevision: 4},
		"long body":                {Anchor: domain.CommentOnDraft, Body: strings.Repeat("é", 2001), DraftRevision: 4},
		"no revision":              {Anchor: domain.CommentOnDraft, Body: "x"},
		"draft with a section":     {Anchor: domain.CommentOnDraft, Body: "x", DraftRevision: 4, PromptSectionID: 1},
		"section not in the draft": {Anchor: domain.CommentOnPromptSection, Body: "x", DraftRevision: 4, PromptSectionID: 7},
		"section in no language":   {Anchor: domain.CommentOnPromptSection, Body: "x", DraftRevision: 4, PromptSectionID: 1, LanguageCode: "de-DE"},
		"prompt kind as setting":   {Anchor: domain.CommentOnSetting, Body: "x", DraftRevision: 4, ResourceKind: domain.ResourcePromptSection, ResourceID: "1"},
		"unknown setting kind":     {Anchor: domain.CommentOnSetting, Body: "x", DraftRevision: 4, ResourceKind: "colour", ResourceID: "1"},
		"setting without an id":    {Anchor: domain.CommentOnSetting, Body: "x", DraftRevision: 4, ResourceKind: domain.ResourceTool},
	} {
		comment.AgentID = "writer-a"
		if _, err := comments.Comment(context.Background(), humanActor(8, 9), comment); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v", name, err)
		}
	}
	if _, ok := query.args[qCommentInsert]; ok {
		t.Fatal("an invalid comment must not be stored")
	}
}

func TestCommentingNeedsEditOrPublishReview(t *testing.T) {
	comments, query := commentService()
	query.rows[qCommentInsert] = [][]any{{int64(5), reviewedAt}}
	draft := domain.DraftComment{AgentID: "writer-a", DraftRevision: 4, Anchor: domain.CommentOnDraft, Body: "ok"}
	comments.Studio.Roles = denyActions(domain.StudioEdit)
	if _, err := comments.Comment(context.Background(), humanActor(8, 10), draft); err != nil {
		t.Fatalf("a publish reviewer comments: %v", err)
	}
	comments.Studio.Roles = denyActions(domain.StudioEdit, domain.StudioApprovePublish)
	if _, err := comments.Comment(context.Background(), humanActor(8, 10), draft); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer = %v", err)
	}
	if _, err := comments.Resolve(context.Background(), humanActor(8, 10), "writer-a", 5); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer resolving = %v", err)
	}
	if list, err := comments.Comments(context.Background(), humanActor(8, 10), "writer-a", ""); err != nil || list == nil {
		t.Fatalf("a viewer lists comments: %v", err)
	}
	if _, err := comments.Comments(context.Background(), humanActor(8, 10), "writer-a", "closed"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown status = %v", err)
	}
}

func TestResolveClosesAnOpenCommentOnce(t *testing.T) {
	comments, query := commentService()
	query.rows[qCommentResolve] = [][]any{{int64(5), int64(4), string(domain.CommentOnSetting), nil, nil, "tool", "tool", "drop crawl", string(domain.DraftCommentResolved),
		"human", "9", reviewedAt, "human", "10", reviewedAt}}
	resolved, err := comments.Resolve(context.Background(), humanActor(8, 10), "writer-a", 5)
	if err != nil || resolved.Status != domain.DraftCommentResolved || resolved.ResolvedBy.ID != "10" || resolved.ResourceKind != domain.ResourceTool || resolved.ResolvedAt == nil {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
	query.rows[qCommentResolve] = nil
	query.rows[qCommentGet] = [][]any{{string(domain.DraftCommentResolved)}}
	if _, err = comments.Resolve(context.Background(), humanActor(8, 10), "writer-a", 5); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("resolved twice = %v", err)
	}
	query.rows[qCommentGet] = nil
	if _, err = comments.Resolve(context.Background(), humanActor(8, 10), "writer-a", 6); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if _, err = comments.Resolve(context.Background(), humanActor(8, 10), "writer-a", 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no id = %v", err)
	}
}
