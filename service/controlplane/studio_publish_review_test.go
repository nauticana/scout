package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var reviewedAt = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

type excuseCall struct {
	kind      domain.PublishReviewKind
	exception string
	person    domain.PrincipalRef
}

// reviewService composes publish reviews over the publish fixture; excused records every Excuse.
func reviewService(t *testing.T, required bool) (*StudioPublishReviews, *studioQueryFake, string, *[]excuseCall) {
	t.Helper()
	studio, query := publishService()
	studio.DB = catalogDBFake{studioDBFake{qs: query}}
	excused := &[]excuseCall{}
	reviews := &StudioPublishReviews{Studio: studio, Now: func() time.Time { return reviewedAt },
		Releases: &fake.EffectiveReleaseRepository{},
		Separation: fake.PublishSeparation{
			RequiredFunc: func(context.Context, int64, string, domain.PublishReviewKind, time.Time) (bool, error) {
				return required, nil
			},
			ExcuseFunc: func(_ context.Context, _ int64, _ string, kind domain.PublishReviewKind, exception string, person domain.PrincipalRef, _ time.Time) error {
				*excused = append(*excused, excuseCall{kind, exception, person})
				if exception == "EXC-OK" {
					return nil
				}
				return domain.ErrForbidden
			},
		}}
	_, candidate, err := studio.publishCandidate(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	return reviews, query, candidate.Definition.DefinitionDigest, excused
}

// reviewRow is a stored review; user 7 last edited a resource it changes.
func reviewRow(id int64, digest string, status domain.PublishReviewStatus, requestedBy int64, decidedBy, exception any) []any {
	var decidedAt any
	if decidedBy != nil {
		decidedAt = reviewedAt
	}
	return []any{id, digest, int64(4), int64(2), nil, `{"causes":["models"],"editors":[7]}`, `[{"name":"golden","passed":true}]`,
		string(status), requestedBy, "please", reviewedAt, decidedBy, nil, decidedAt, exception, nil, nil}
}

func consumedRow(id int64, digest string) []any {
	row := reviewRow(id, digest, domain.PublishReviewApproved, 9, int64(10), nil)
	row[15] = "1"
	return row
}

func TestRequestRecordsTheCandidateDigestDiffAndEvaluations(t *testing.T) {
	reviews, query, digest, _ := reviewService(t, true)
	query.rows[qReviewLockDraft] = [][]any{{int64(4)}}
	query.rows[qReviewInsert] = [][]any{{int64(11), reviewedAt}}
	active := domain.AgentDefinition{AgentID: "writer-a", AgentTypeID: "writer", Languages: []domain.CompiledPrompt{{LanguageCode: "en-US",
		Sections: []domain.CompiledPromptSection{{Sequence: 1, PromptSectionID: 1, Caption: "task", Instruction: "old"}, {PromptSectionID: 2, Caption: "tone"}}}}}
	encoded, _ := json.Marshal(active)
	query.rows[qReviewActive] = [][]any{{"3", string(encoded)}}
	// A person who only changed the models or a section still authored the release.
	query.rows[qReviewDraftEditors] = [][]any{{"12"}, {"9"}}
	reviews.Releases = &fake.EffectiveReleaseRepository{GetFunc: func(context.Context, int64, string, string) (domain.EffectiveRelease, error) {
		return domain.EffectiveRelease{Resources: []domain.EffectiveResource{{ResourceKind: domain.ResourceTool, ResourceID: "tool", Value: []byte(`["search"]`)}}}, nil
	}}
	reviews.Evaluator = fake.PublishReviewEvaluator(func(_ context.Context, candidate domain.PublishCandidate) ([]domain.PublishReviewEvaluation, error) {
		return []domain.PublishReviewEvaluation{{Name: "golden " + candidate.Definition.AgentID, Passed: true}}, nil
	})

	review, err := reviews.Request(context.Background(), humanActor(8, 9), domain.PublishReviewRequest{AgentID: "writer-a", Note: " ship it "})
	if err != nil {
		t.Fatal(err)
	}
	if review.ReviewID != 11 || review.DefinitionDigest != digest || review.RequestedBy != 9 || review.BaseVersion != "3" || review.RequestNote != "ship it" {
		t.Fatalf("review = %+v", review)
	}
	sections := review.Diff.PromptSections
	if len(sections) != 2 || sections[0].Change != domain.DiffChanged || sections[1].PromptSectionID != 2 || sections[1].Change != domain.DiffRemoved {
		t.Fatalf("prompt section diff = %+v", sections)
	}
	if settings := review.Diff.Settings; len(settings) != 1 || settings[0].Change != domain.DiffRemoved || string(settings[0].Before) != `["search"]` {
		t.Fatalf("settings diff = %+v", settings)
	}
	if !slices.Equal(review.Diff.Editors, []int64{9, 12}) || query.args[qReviewDraftEditors][0] != int64(8) {
		t.Fatalf("editors = %v", review.Diff.Editors)
	}
	args := query.args[qReviewInsert]
	if args[2] != digest || args[5] != "3" || args[6] != nil || !strings.Contains(args[7].(string), `"prompt_sections"`) || !strings.Contains(args[8].(string), "golden writer-a") ||
		args[9] != int64(9) || args[11] != nil {
		t.Fatalf("insert args = %v", args)
	}
	if !slices.Contains(query.queries, qReviewSupersede) || query.args[qReviewAudit][2] != "PUBLISH_REVIEW_REQUEST" || query.commits != 1 {
		t.Fatalf("queries = %v, commits = %d", query.queries, query.commits)
	}
}

func TestRequestNamesAnExceptionSeparationAdmitsForTheRequester(t *testing.T) {
	reviews, query, _, excused := reviewService(t, true)
	query.rows[qReviewLockDraft] = [][]any{{int64(4)}}
	query.rows[qReviewInsert] = [][]any{{int64(11), reviewedAt}}
	request := domain.PublishReviewRequest{AgentID: "writer-a", SodExceptionID: " EXC-BAD "}
	if _, err := reviews.Request(context.Background(), humanActor(8, 9), request); !errors.Is(err, domain.ErrForbidden) || query.commits != 0 {
		t.Fatalf("refused exception = %v, commits %d", err, query.commits)
	}
	request.SodExceptionID = "EXC-OK"
	review, err := reviews.Request(context.Background(), humanActor(8, 9), request)
	if err != nil || review.SodExceptionID != "EXC-OK" || query.args[qReviewInsert][11] != "EXC-OK" {
		t.Fatalf("review = %+v, %v, args %v", review, err, query.args[qReviewInsert])
	}
	if last := (*excused)[len(*excused)-1]; last.person.ID != "9" || last.kind != domain.PublishReviewOfPublish || !strings.Contains(query.args[qReviewAudit][3].(string), "EXC-OK") {
		t.Fatalf("excused %+v, audit %v", last, query.args[qReviewAudit])
	}
	request.SodExceptionID = strings.Repeat("x", 201)
	if _, err = reviews.Request(context.Background(), humanActor(8, 9), request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("long exception = %v", err)
	}
}

func TestRequestRefusesServicesStaleDraftsAndViewers(t *testing.T) {
	reviews, query, _, _ := reviewService(t, true)
	service := domain.StudioActor{TenantID: 8, Principal: domain.PrincipalRef{Kind: domain.PrincipalService, ID: "installer"}}
	if _, err := reviews.Request(context.Background(), service, domain.PublishReviewRequest{AgentID: "writer-a"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("service = %v", err)
	}
	query.rows[qReviewLockDraft] = [][]any{{int64(5)}}
	if _, err := reviews.Request(context.Background(), humanActor(8, 9), domain.PublishReviewRequest{AgentID: "writer-a"}); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("draft saved meanwhile = %v", err)
	}
	if _, err := reviews.Request(context.Background(), humanActor(8, 9), domain.PublishReviewRequest{AgentID: "writer-a", Note: strings.Repeat("x", 401)}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("long note = %v", err)
	}
	reviews.Studio.Roles = denyActions(domain.StudioEdit)
	if _, err := reviews.Request(context.Background(), humanActor(8, 9), domain.PublishReviewRequest{AgentID: "writer-a"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("viewer = %v", err)
	}
	if query.commits != 0 || slices.Contains(query.queries, qReviewInsert) {
		t.Fatalf("a refused request must not commit: %d", query.commits)
	}
}

func denyActions(denied ...domain.StudioAction) contract.PrincipalAuthorizer {
	return fake.PrincipalAuthorizerFunc(func(_ context.Context, _ domain.Principal, _, action, _ string) (domain.AuthorizationGrant, error) {
		return domain.AuthorizationGrant{Allowed: !slices.Contains(denied, domain.StudioAction(action))}, nil
	})
}

func TestApproveNeedsANonAuthorAndAnUnchangedDraft(t *testing.T) {
	reviews, query, digest, excused := reviewService(t, true)
	query.rows[qReviewGet] = [][]any{reviewRow(11, digest, domain.PublishReviewRequested, 9, nil, "EXC-OK")}
	query.rows[qReviewDecide] = [][]any{{reviewedAt}}
	decision := domain.PublishReviewDecision{AgentID: "writer-a", ReviewID: 11}

	for _, author := range []int64{9, 7} {
		if _, err := reviews.Approve(context.Background(), humanActor(8, author), decision); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("approval by author %d = %v", author, err)
		}
	}
	if len(*excused) != 0 || slices.Contains(query.queries, qReviewDecide) {
		t.Fatalf("an exception never relieves an approver: %+v", *excused)
	}
	approved, err := reviews.Approve(context.Background(), humanActor(8, 10), decision)
	if err != nil || *approved.DecidedBy != 10 || approved.Status != domain.PublishReviewApproved || approved.SodExceptionID != "EXC-OK" {
		t.Fatalf("approval = %+v, %v", approved, err)
	}
	if args := query.args[qReviewDecide]; len(args) != 7 || args[0] != string(domain.PublishReviewApproved) || args[1] != int64(10) || args[5] != int64(11) || args[6] != digest {
		t.Fatalf("decide args = %v", args)
	}

	query.rows[qReviewGet] = [][]any{reviewRow(11, strings.Repeat("0", 64), domain.PublishReviewRequested, 9, nil, nil)}
	if _, err = reviews.Approve(context.Background(), humanActor(8, 10), decision); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed draft = %v", err)
	}
	query.rows[qReviewGet] = [][]any{reviewRow(11, digest, domain.PublishReviewRejected, 9, int64(10), nil)}
	if _, err = reviews.Approve(context.Background(), humanActor(8, 10), decision); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("decided review = %v", err)
	}
	query.rows[qReviewGet] = [][]any{reviewRow(11, digest, domain.PublishReviewRequested, 9, nil, nil)}
	query.rows[qReviewDecide] = nil
	if _, err = reviews.Approve(context.Background(), humanActor(8, 10), decision); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("concurrent decision = %v", err)
	}
	query.rows[qReviewGet] = nil
	if _, err = reviews.Reject(context.Background(), humanActor(8, 10), decision); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing review = %v", err)
	}
	reviews.Studio.Roles = denyActions(domain.StudioApprovePublish)
	if _, err = reviews.Approve(context.Background(), humanActor(8, 10), decision); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("without APPROVE_PUBLISH = %v", err)
	}
}

func TestRejectClosesARequestedReview(t *testing.T) {
	reviews, query, digest, _ := reviewService(t, true)
	query.rows[qReviewGet] = [][]any{reviewRow(11, digest, domain.PublishReviewRequested, 9, nil, nil)}
	query.rows[qReviewDecide] = [][]any{{reviewedAt}}
	rejected, err := reviews.Reject(context.Background(), humanActor(8, 10), domain.PublishReviewDecision{AgentID: "writer-a", ReviewID: 11, Note: "tone"})
	if err != nil || rejected.Status != domain.PublishReviewRejected || query.args[qReviewDecide][0] != string(domain.PublishReviewRejected) || query.args[qReviewAudit][2] != "PUBLISH_REVIEW_REJECT" {
		t.Fatalf("rejected = %+v, %v", rejected, err)
	}
}

func TestReviewStateReportsStalenessAndOpenComments(t *testing.T) {
	reviews, query, digest, _ := reviewService(t, true)
	query.rows[qReviewLatest] = [][]any{reviewRow(11, strings.Repeat("0", 64), domain.PublishReviewApproved, 9, int64(10), nil)}
	query.rows[qCommentListStatus] = [][]any{{int64(3), int64(4), string(domain.CommentOnDraft), nil, nil, nil, nil, "too long", string(domain.DraftCommentOpen), "human", "10", reviewedAt, nil, nil, nil}}
	state, err := reviews.Review(context.Background(), humanActor(8, 10), "writer-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if state.Kind != domain.PublishReviewOfPublish || !state.Required || state.CurrentDigest != digest || !state.Stale || state.Review.ReviewID != 11 || len(state.Review.Evaluations) != 1 ||
		state.Review.Diff.Causes[0] != "models" || !slices.Equal(state.Review.Diff.Editors, []int64{7}) || len(state.OpenComments) != 1 || state.OpenComments[0].Author.ID != "10" {
		t.Fatalf("state = %+v", state)
	}
	if query.args[qCommentListStatus][2] != string(domain.DraftCommentOpen) {
		t.Fatalf("comment filter = %v", query.args[qCommentListStatus])
	}
	reviews.Studio.Roles = denyActions(domain.StudioView)
	if _, err = reviews.Review(context.Background(), humanActor(8, 10), "writer-a", domain.PublishReviewOfPublish); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("stranger = %v", err)
	}
}

func candidateOf(digest string, publisher int64) domain.PublishCandidate {
	return domain.PublishCandidate{Actor: humanActor(8, publisher), Definition: domain.AgentDefinition{AgentID: "writer-a", DefinitionDigest: digest}}
}

func TestGuardAdmitsOnlyTheLatestApprovedUnconsumedReviewOfTheDigest(t *testing.T) {
	optional, query, digest, _ := reviewService(t, false)
	if err := optional.CheckPublish(context.Background(), candidateOf(digest, 9)); err != nil {
		t.Fatalf("a tenant without separation publishes unreviewed: %v", err)
	}
	query.rows[qReviewLatest] = [][]any{reviewRow(11, digest, domain.PublishReviewRejected, 9, int64(10), nil)}
	if err := optional.CheckPublish(context.Background(), candidateOf(digest, 9)); err != nil {
		t.Fatalf("a tenant without separation publishes past a rejection: %v", err)
	}

	reviews, query, digest, _ := reviewService(t, true)
	if err := reviews.CheckPublish(context.Background(), candidateOf(digest, 10)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("no review = %v", err)
	}
	// A tenant separating publishing only restores unreviewed.
	publishOnly := &StudioPublishReviews{Studio: reviews.Studio, Now: reviews.Now, Releases: reviews.Releases,
		Separation: fake.PublishSeparation{RequiredFunc: func(_ context.Context, _ int64, _ string, kind domain.PublishReviewKind, _ time.Time) (bool, error) {
			return kind == domain.PublishReviewOfPublish, nil
		}}}
	restore := candidateOf(digest, 9)
	restore.RestoredFrom = "2"
	if err := publishOnly.CheckPublish(context.Background(), restore); err != nil {
		t.Fatalf("restore = %v", err)
	}
	// Publish consumed the review of a scheduled release, so going live must not ask for another.
	activation := candidateOf(digest, 9)
	activation.Activation = true
	if err := reviews.CheckPublish(context.Background(), activation); err != nil {
		t.Fatalf("scheduled activation = %v", err)
	}
	for name, row := range map[string][]any{
		"re-requested after approval": reviewRow(12, digest, domain.PublishReviewRequested, 9, nil, nil),
		"rejected after approval":     reviewRow(12, digest, domain.PublishReviewRejected, 9, int64(11), nil),
		"consumed":                    consumedRow(11, digest),
		"of another digest":           reviewRow(11, strings.Repeat("0", 64), domain.PublishReviewApproved, 9, int64(10), nil),
	} {
		query.rows[qReviewLatest] = [][]any{row}
		if err := reviews.CheckPublish(context.Background(), candidateOf(digest, 10)); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("latest review %s = %v", name, err)
		}
	}
	for _, name := range []string{qReviewLatest, qReviewClaim} {
		if sql := reviewQueries[name]; !strings.Contains(sql, "status_code <> "+sqlReviewSuperseded) || strings.Contains(sql, "status_code = "+sqlReviewApproved) ||
			strings.Contains(sql, "definition_digest = ?") {
			t.Fatalf("%s must read the latest review whatever its state or digest:\n%s", name, sql)
		}
	}
	if args := query.args[qReviewLatest]; len(args) != 2 {
		t.Fatalf("latest review args = %v", args)
	}
}

func TestGuardSeparatesAuthorsFromApproverAndPublisher(t *testing.T) {
	reviews, query, digest, excused := reviewService(t, true)
	query.rows[qReviewLatest] = [][]any{reviewRow(11, digest, domain.PublishReviewApproved, 9, int64(10), nil)}
	for _, publisher := range []int64{10, 12} {
		if err := reviews.CheckPublish(context.Background(), candidateOf(digest, publisher)); err != nil {
			t.Fatalf("publisher %d = %v", publisher, err)
		}
	}
	for _, author := range []int64{9, 7} {
		if err := reviews.CheckPublish(context.Background(), candidateOf(digest, author)); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("author %d publishing = %v", author, err)
		}
	}
	service := domain.PublishCandidate{Actor: domain.StudioActor{TenantID: 8, Principal: domain.PrincipalRef{Kind: domain.PrincipalService, ID: "installer"}},
		Definition: domain.AgentDefinition{AgentID: "writer-a", DefinitionDigest: digest}}
	if err := reviews.CheckPublish(context.Background(), service); err != nil {
		t.Fatalf("service publisher = %v", err)
	}
	query.rows[qReviewLatest] = [][]any{reviewRow(11, digest, domain.PublishReviewApproved, 9, int64(7), nil)}
	if err := reviews.CheckPublish(context.Background(), candidateOf(digest, 10)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an approval by an editor = %v", err)
	}

	query.rows[qReviewLatest] = [][]any{reviewRow(11, digest, domain.PublishReviewApproved, 9, int64(10), "EXC-OK")}
	if err := reviews.CheckPublish(context.Background(), candidateOf(digest, 9)); err != nil {
		t.Fatalf("the requester publishing under their exception = %v", err)
	}
	if last := (*excused)[len(*excused)-1]; last.exception != "EXC-OK" || last.person.ID != "9" {
		t.Fatalf("excused %+v", *excused)
	}
	calls := len(*excused)
	if err := reviews.CheckPublish(context.Background(), candidateOf(digest, 7)); !errors.Is(err, domain.ErrForbidden) || len(*excused) != calls {
		t.Fatalf("an editor publishing under the requester's exception = %v", err)
	}
	query.rows[qReviewLatest] = [][]any{reviewRow(11, digest, domain.PublishReviewApproved, 9, int64(10), "EXC-REVOKED")}
	if err := reviews.CheckPublish(context.Background(), candidateOf(digest, 9)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a revoked exception = %v", err)
	}
	if err := reviews.CheckPublish(context.Background(), candidateOf(digest, 12)); err != nil {
		t.Fatalf("a revoked exception does not bind another publisher: %v", err)
	}

	query.rows[qReviewLatest] = [][]any{reviewRow(11, digest, domain.PublishReviewApproved, 9, int64(10), nil)}
	query.rows[qReviewOpenComments] = [][]any{{int64(2)}}
	if err := reviews.CheckPublish(context.Background(), candidateOf(digest, 10)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("open comments = %v", err)
	}
}

func TestPublishConsumesTheReviewInItsTransaction(t *testing.T) {
	reviews, query, digest, _ := reviewService(t, true)
	reviews.Studio.Guards = []contract.PublishGuard{reviews}
	reviews.Studio.ReleaseWriters = []ReleaseWriter{reviews}
	query.rows[qReviewLatest] = [][]any{reviewRow(11, digest, domain.PublishReviewApproved, 9, int64(10), "EXC-OK")}
	query.rows[qReviewClaim] = query.rows[qReviewLatest]
	query.rows[qReviewConsume] = [][]any{{int64(11)}}
	request := domain.AgentPublishRequest{AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2}

	release, err := reviews.Studio.Publish(context.Background(), humanActor(8, 9), request)
	if err != nil {
		t.Fatal(err)
	}
	if release.DefinitionDigest != digest || len(query.args[qReviewClaim]) != 2 {
		t.Fatalf("release %s, claim %v", release.DefinitionDigest, query.args[qReviewClaim])
	}
	if args := query.args[qReviewConsume]; args[0] != "1" || args[3] != int64(11) {
		t.Fatalf("consume args = %v", args)
	}
	if audit := query.args[qReviewAudit]; audit[2] != "PUBLISH_REVIEWED" || !strings.Contains(audit[3].(string), "EXC-OK") || audit[5] != "9" {
		t.Fatalf("audit = %v", audit)
	}

	if _, err = reviews.Studio.Publish(context.Background(), humanActor(8, 10), request); err != nil {
		t.Fatal(err)
	}
	if audit := query.args[qReviewAudit]; strings.Contains(audit[3].(string), "EXC-OK") || audit[5] != "10" {
		t.Fatalf("an exception not relied on is not evidence: %v", audit)
	}

	query.rows[qReviewConsume] = nil
	commits := query.commits
	if _, err = reviews.Studio.Publish(context.Background(), humanActor(8, 9), request); !errors.Is(err, domain.ErrConflict) || query.commits != commits {
		t.Fatalf("a review consumed concurrently = %v", err)
	}
	query.rows[qReviewClaim] = [][]any{consumedRow(11, digest)}
	if _, err = reviews.Studio.Publish(context.Background(), humanActor(8, 10), request); !errors.Is(err, domain.ErrConflict) || query.commits != commits {
		t.Fatalf("a review consumed between guard and commit = %v", err)
	}
	query.rows[qReviewClaim] = nil
	if _, err = reviews.Studio.Publish(context.Background(), humanActor(8, 10), request); !errors.Is(err, domain.ErrConflict) || query.commits != commits {
		t.Fatalf("a review gone between guard and commit = %v", err)
	}
}

func TestWriteReleaseFailsClearlyUnconfigured(t *testing.T) {
	reviews, query, digest, _ := reviewService(t, false)
	restored := domain.AgentDefinition{AgentID: "writer-a", DefinitionDigest: digest, Version: "5", RestoredFromVersion: "2"}
	if err := reviews.WriteRelease(context.Background(), catalogTxFake{query}, 8, restored); err != nil {
		t.Fatalf("an unreviewed restore where none is required = %v", err)
	}
	if err := reviews.WriteRelease(context.Background(), query, 8, restored); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("restore in a plain transaction = %v", err)
	}
	if err := reviews.WriteRelease(context.Background(), query, 8, domain.AgentDefinition{AgentID: "writer-a", Version: "5"}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("plain transaction = %v", err)
	}
	if err := (&StudioPublishReviews{}).CheckPublish(context.Background(), candidateOf(digest, 9)); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("unconfigured guard = %v", err)
	}
	unwired := &StudioPublishReviews{Separation: fake.PublishSeparation{}}
	if err := unwired.WriteRelease(context.Background(), catalogTxFake{query}, 8, domain.AgentDefinition{AgentID: "writer-a", Version: "5"}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("writer without a studio = %v", err)
	}
}

func TestChangedEditorsAreTheLastEditorsOfAddedAndChangedResources(t *testing.T) {
	by := func(user int64) *int64 { return &user }
	resource := func(kind domain.ResourceKind, id, value string, editor *int64) domain.EffectiveResource {
		return domain.EffectiveResource{ResourceKind: kind, ResourceID: id, Value: []byte(value), Source: domain.Provenance{Approver: editor}}
	}
	before := []domain.EffectiveResource{
		resource(domain.ResourceTool, "tool", `["a"]`, by(1)),
		resource(domain.ResourcePromptSection, "1/en-US", `{"instruction":"old"}`, by(2)),
		resource(domain.ResourceBudget, "budget", `{}`, by(3)),
	}
	after := []domain.EffectiveResource{
		resource(domain.ResourceTool, "tool", `["a"]`, by(4)),
		resource(domain.ResourcePromptSection, "1/en-US", `{"instruction":"new"}`, by(6)),
		resource(domain.ResourceModel, "model", `["p/m"]`, by(5)),
		resource(domain.ResourceAutonomy, "autonomy", `{}`, nil),
		resource(domain.ResourceSkill, "skill", `[]`, by(5)),
	}
	if editors := changedEditors(before, after); !slices.Equal(editors, []int64{5, 6}) {
		t.Fatalf("editors = %v", editors)
	}
}

func TestBasePublishSeparationAdmitsNoException(t *testing.T) {
	separation := BasePublishSeparation{Tenants: map[int64]bool{8: true}}
	publish, restore := domain.PublishReviewOfPublish, domain.PublishReviewOfRestore
	if required, _ := separation.Required(context.Background(), 8, "writer-a", publish, reviewedAt); !required {
		t.Fatal("listed tenant must require two-person publish")
	}
	if required, _ := separation.Required(context.Background(), 9, "writer-a", publish, reviewedAt); required {
		t.Fatal("unlisted tenant must not")
	}
	if required, _ := (BasePublishSeparation{All: true}).Required(context.Background(), 9, "writer-a", publish, reviewedAt); !required {
		t.Fatal("All must require it everywhere")
	}
	if required, _ := separation.Required(context.Background(), 8, "writer-a", restore, reviewedAt); required {
		t.Fatal("restores are reviewed only with Restores")
	}
	separation.Restores = true
	if required, _ := separation.Required(context.Background(), 8, "writer-a", restore, reviewedAt); !required {
		t.Fatal("Restores must require reviewing a listed tenant's restores")
	}
	if required, _ := separation.Required(context.Background(), 9, "writer-a", restore, reviewedAt); required {
		t.Fatal("Restores must not reach an unlisted tenant")
	}
	if _, err := separation.Required(context.Background(), 8, "writer-a", "rollback", reviewedAt); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown kind = %v", err)
	}
	for _, kind := range []domain.PublishReviewKind{publish, restore} {
		if err := separation.Excuse(context.Background(), 8, "writer-a", kind, "EXC-1", domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}, reviewedAt); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("%s excuse = %v", kind, err)
		}
	}
}
