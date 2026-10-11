package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var restoredDigest = strings.Repeat("a", 64)

func restoredDefinition(digest string) string {
	encoded, _ := json.Marshal(domain.AgentDefinition{AgentID: "writer-a", AgentTypeID: "writer", Version: "2", DefinitionDigest: digest,
		DraftRevision: 3, PromptProfileRevision: 1, Languages: []domain.CompiledPrompt{{LanguageCode: "en-US",
			Sections: []domain.CompiledPromptSection{{Sequence: 1, PromptSectionID: 1, Caption: "task", Instruction: "old"}}}}})
	return string(encoded)
}

func activeDefinition() string {
	encoded, _ := json.Marshal(domain.AgentDefinition{AgentID: "writer-a", AgentTypeID: "writer", Version: "3",
		DefinitionDigest: strings.Repeat("b", 64), Languages: []domain.CompiledPrompt{{LanguageCode: "en-US",
			Sections: []domain.CompiledPromptSection{{Sequence: 1, PromptSectionID: 1, Caption: "task", Instruction: "new"}}}}})
	return string(encoded)
}

// restoreReviewService serves version 2 over active version 3; the user who last edited
// version 2's tool binding is 7, and user 12 changed the draft since the last publish.
func restoreReviewService(t *testing.T, required bool) (*StudioPublishReviews, *studioQueryFake, *[]excuseCall) {
	t.Helper()
	reviews, query, _, excused := reviewService(t, required)
	query.rows[qReviewVersion] = [][]any{{restoredDefinition(restoredDigest)}}
	query.rows[qReviewActive] = [][]any{{"3", activeDefinition()}}
	query.rows[qReviewActiveLocked] = query.rows[qReviewActive]
	query.rows[qReviewDraftEditors] = [][]any{{"12"}}
	editor := int64(7)
	reviews.Releases = &fake.EffectiveReleaseRepository{GetFunc: func(_ context.Context, tenantID int64, _, version string) (domain.EffectiveRelease, error) {
		if tenantID != 8 {
			return domain.EffectiveRelease{}, domain.ErrNotFound
		}
		switch version {
		case "2":
			return domain.EffectiveRelease{Resources: []domain.EffectiveResource{{ResourceKind: domain.ResourceTool, ResourceID: "tool",
				Value: []byte(`["search"]`), Source: domain.Provenance{Approver: &editor}}}}, nil
		case "3":
			return domain.EffectiveRelease{}, nil
		}
		return domain.EffectiveRelease{}, domain.ErrNotFound
	}}
	return reviews, query, excused
}

// restoreReviewRow is a stored review of restoring version over base.
func restoreReviewRow(id int64, digest string, status domain.PublishReviewStatus, requestedBy int64, decidedBy, exception any, base, version string) []any {
	row := reviewRow(id, digest, status, requestedBy, decidedBy, exception)
	row[4], row[5], row[16] = base, `{"active_version":"`+base+`"}`, version
	return row
}

func restoreCandidate(publisher int64, version string) domain.PublishCandidate {
	return domain.PublishCandidate{Actor: humanActor(8, publisher), RestoredFrom: version,
		Definition: domain.AgentDefinition{AgentID: "writer-a", DefinitionDigest: restoredDigest}}
}

func TestRestoreReviewRecordsTheVersionAndItsDiffAgainstTheActiveRelease(t *testing.T) {
	reviews, query, _ := restoreReviewService(t, true)
	// A draft saved meanwhile does not matter: a restore reinstates a published version.
	query.rows[qReviewLockDraft] = [][]any{{int64(9)}}
	query.rows[qReviewInsert] = [][]any{{int64(21), reviewedAt}}
	var evaluated domain.PublishCandidate
	reviews.Evaluator = fake.PublishReviewEvaluator(func(_ context.Context, candidate domain.PublishCandidate) ([]domain.PublishReviewEvaluation, error) {
		evaluated = candidate
		return nil, nil
	})
	// Requesting a restore review takes RESTORE, not EDIT.
	reviews.Studio.Roles = denyActions(domain.StudioEdit)

	review, err := reviews.Request(context.Background(), humanActor(8, 9), domain.PublishReviewRequest{AgentID: "writer-a", RestoreVersion: "2", Note: "roll back"})
	if err != nil {
		t.Fatal(err)
	}
	if review.ReviewID != 21 || review.RestoreVersion != "2" || review.BaseVersion != "3" || review.DefinitionDigest != restoredDigest ||
		review.DraftRevision != 3 || review.PromptProfileRevision != 1 || review.RequestedBy != 9 {
		t.Fatalf("review = %+v", review)
	}
	if sections := review.Diff.PromptSections; len(sections) != 1 || sections[0].Change != domain.DiffChanged {
		t.Fatalf("prompt section diff = %+v", sections)
	}
	if settings := review.Diff.Settings; len(settings) != 1 || settings[0].Change != domain.DiffAdded || string(settings[0].After) != `["search"]` {
		t.Fatalf("settings diff = %+v", settings)
	}
	if len(review.Diff.Editors) != 0 || slices.Contains(query.queries, qReviewDraftEditors) {
		t.Fatalf("a restore's only author is its requester: %v", review.Diff.Editors)
	}
	if evaluated.RestoredFrom != "2" || evaluated.Definition.DefinitionDigest != restoredDigest {
		t.Fatalf("evaluated candidate = %+v", evaluated)
	}
	args := query.args[qReviewInsert]
	if args[0] != int64(8) || args[2] != restoredDigest || args[5] != "3" || args[6] != "2" || args[9] != int64(9) {
		t.Fatalf("insert args = %v", args)
	}
	if !slices.Contains(query.queries, qReviewSupersedeRestore) || slices.Contains(query.queries, qReviewSupersede) {
		t.Fatalf("a restore request must supersede only open restore reviews: %v", query.queries)
	}
	if audit := query.args[qReviewAudit]; audit[2] != "RESTORE_REVIEW_REQUEST" || !strings.Contains(audit[3].(string), "restoring version 2 over 3") || query.commits != 1 {
		t.Fatalf("audit = %v, commits = %d", audit, query.commits)
	}
	if args := query.args[qReviewVersion]; args[0] != int64(8) || args[2] != "2" {
		t.Fatalf("version lookup args = %v", args)
	}
}

func TestRestoreReviewRequestRefusals(t *testing.T) {
	reviews, query, excused := restoreReviewService(t, true)
	query.rows[qReviewLockDraft] = [][]any{{int64(4)}}
	query.rows[qReviewInsert] = [][]any{{int64(21), reviewedAt}}
	request := func(actor domain.StudioActor, version, exception string) error {
		_, err := reviews.Request(context.Background(), actor, domain.PublishReviewRequest{AgentID: "writer-a", RestoreVersion: version, SodExceptionID: exception})
		return err
	}
	for name, version := range map[string]string{"padded": " 2", "too long": strings.Repeat("9", 81)} {
		if err := request(humanActor(8, 9), version, ""); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s version = %v", name, err)
		}
	}
	service := domain.StudioActor{TenantID: 8, Principal: domain.PrincipalRef{Kind: domain.PrincipalService, ID: "installer"}}
	if err := request(service, "2", ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("service = %v", err)
	}
	if err := request(humanActor(8, 9), "2", "EXC-BAD"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("refused exception = %v", err)
	}
	if last := (*excused)[len(*excused)-1]; last.kind != domain.PublishReviewOfRestore {
		t.Fatalf("the exception must be judged for a restore: %+v", last)
	}
	query.rows[qReviewVersion] = [][]any{{restoredDefinition("")}}
	if err := request(humanActor(8, 9), "2", ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("version without a digest = %v", err)
	}
	query.rows[qReviewVersion] = [][]any{{restoredDefinition(restoredDigest)}}
	if err := request(humanActor(8, 9), "5", ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("version without frozen settings = %v", err)
	}
	query.rows[qReviewVersion] = nil
	if err := request(humanActor(8, 9), "2", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown version = %v", err)
	}
	query.rows[qReviewVersion] = [][]any{{restoredDefinition(restoredDigest)}}
	if err := request(domain.StudioActor{TenantID: 9, Principal: humanActor(8, 9).Principal}, "2", ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("another tenant's version settings = %v", err)
	}
	reviews.Studio.Roles = denyActions(domain.StudioRestore)
	if err := request(humanActor(8, 9), "2", ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("without RESTORE = %v", err)
	}
	if query.commits != 0 || slices.Contains(query.queries, qReviewInsert) {
		t.Fatalf("a refused request must not commit: %d", query.commits)
	}
}

func TestApproveRestoreReviewNeedsANonRequesterAndTheSameActiveRelease(t *testing.T) {
	reviews, query, _ := restoreReviewService(t, true)
	query.rows[qReviewGet] = [][]any{restoreReviewRow(21, restoredDigest, domain.PublishReviewRequested, 9, nil, nil, "3", "2")}
	query.rows[qReviewDecide] = [][]any{{reviewedAt}}
	decision := domain.PublishReviewDecision{AgentID: "writer-a", ReviewID: 21}

	if _, err := reviews.Approve(context.Background(), humanActor(8, 9), decision); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("approval by the requester = %v", err)
	}
	// Editors of the restored content authored the original publish, not this restore.
	approved, err := reviews.Approve(context.Background(), humanActor(8, 7), decision)
	if err != nil || approved.Status != domain.PublishReviewApproved || *approved.DecidedBy != 7 || approved.RestoreVersion != "2" {
		t.Fatalf("approval = %+v, %v", approved, err)
	}
	if args := query.args[qReviewDecide]; args[0] != "approved" || args[3] != int64(8) || args[5] != int64(21) || args[6] != restoredDigest {
		t.Fatalf("decide args = %v", args)
	}
	if audit := query.args[qReviewAudit]; audit[2] != "RESTORE_REVIEW_APPROVE" {
		t.Fatalf("audit = %v", audit)
	}
	if slices.Contains(query.queries, qStudioGetDraft) && query.args[qStudioGetDraft] != nil && strings.Contains(query.queries[len(query.queries)-1], "draft_editors") {
		t.Fatal("approving a restore must not compile the draft")
	}

	query.rows[qReviewActive] = [][]any{{"4", activeDefinition()}}
	if _, err = reviews.Approve(context.Background(), humanActor(8, 10), decision); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("active release changed = %v", err)
	}
	query.rows[qReviewActive] = [][]any{{"3", activeDefinition()}}
	query.rows[qReviewVersion] = [][]any{{restoredDefinition(strings.Repeat("c", 64))}}
	if _, err = reviews.Approve(context.Background(), humanActor(8, 10), decision); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("version digest differs = %v", err)
	}
	query.rows[qReviewVersion] = nil
	if _, err = reviews.Approve(context.Background(), humanActor(8, 10), decision); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("version gone = %v", err)
	}
	query.rows[qReviewDecide] = [][]any{{reviewedAt}}
	rejected, err := reviews.Reject(context.Background(), humanActor(8, 10), decision)
	if err != nil || rejected.Status != domain.PublishReviewRejected || query.args[qReviewAudit][2] != "RESTORE_REVIEW_REJECT" {
		t.Fatalf("reject = %+v, %v", rejected, err)
	}
	reviews.Studio.Roles = denyActions(domain.StudioApprovePublish)
	if _, err = reviews.Approve(context.Background(), humanActor(8, 10), decision); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("without APPROVE_PUBLISH = %v", err)
	}
}

func TestGuardAdmitsARestoreOnlyWithAnApprovedReviewOfThatVersion(t *testing.T) {
	optional, query, _ := restoreReviewService(t, false)
	if err := optional.CheckPublish(context.Background(), restoreCandidate(9, "2")); err != nil {
		t.Fatalf("a tenant that does not review restores = %v", err)
	}
	if slices.Contains(query.queries, qReviewLatest) {
		t.Fatal("a restore must not read the publish review")
	}

	reviews, query, excused := restoreReviewService(t, true)
	if err := reviews.CheckPublish(context.Background(), restoreCandidate(10, "2")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("no restore review = %v", err)
	}
	// An approved publish review of the same digest does not admit a restore.
	query.rows[qReviewLatest] = [][]any{reviewRow(11, restoredDigest, domain.PublishReviewApproved, 9, int64(10), nil)}
	if err := reviews.CheckPublish(context.Background(), restoreCandidate(10, "2")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a publish review admitting a restore = %v", err)
	}
	query.rows[qReviewLatestRestore] = [][]any{restoreReviewRow(21, restoredDigest, domain.PublishReviewApproved, 9, int64(10), nil, "3", "2")}
	query.rows[qReviewOpenComments] = [][]any{{int64(2)}}
	for _, restorer := range []int64{10, 12} {
		if err := reviews.CheckPublish(context.Background(), restoreCandidate(restorer, "2")); err != nil {
			t.Fatalf("restorer %d = %v", restorer, err)
		}
	}
	if args := query.args[qReviewLatestRestore]; len(args) != 2 || args[0] != int64(8) {
		t.Fatalf("restore review lookup args = %v", args)
	}
	if err := reviews.CheckPublish(context.Background(), restoreCandidate(9, "2")); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("the requester restoring = %v", err)
	}
	if err := reviews.CheckPublish(context.Background(), restoreCandidate(10, "1")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("restoring another version = %v", err)
	}
	other := restoreCandidate(10, "2")
	other.Definition.DefinitionDigest = strings.Repeat("c", 64)
	if err := reviews.CheckPublish(context.Background(), other); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("restoring another definition = %v", err)
	}
	query.rows[qReviewActive] = [][]any{{"4", activeDefinition()}}
	if err := reviews.CheckPublish(context.Background(), restoreCandidate(10, "2")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("active release changed since the review = %v", err)
	}
	query.rows[qReviewActive] = [][]any{{"3", activeDefinition()}}
	for name, row := range map[string][]any{
		"requested": restoreReviewRow(22, restoredDigest, domain.PublishReviewRequested, 9, nil, nil, "3", "2"),
		"rejected":  restoreReviewRow(22, restoredDigest, domain.PublishReviewRejected, 9, int64(10), nil, "3", "2"),
		"consumed": func() []any {
			row := restoreReviewRow(21, restoredDigest, domain.PublishReviewApproved, 9, int64(10), nil, "3", "2")
			row[15] = "4"
			return row
		}(),
	} {
		query.rows[qReviewLatestRestore] = [][]any{row}
		if err := reviews.CheckPublish(context.Background(), restoreCandidate(10, "2")); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("latest restore review %s = %v", name, err)
		}
	}

	query.rows[qReviewLatestRestore] = [][]any{restoreReviewRow(21, restoredDigest, domain.PublishReviewApproved, 9, int64(10), "EXC-OK", "3", "2")}
	if err := reviews.CheckPublish(context.Background(), restoreCandidate(9, "2")); err != nil {
		t.Fatalf("the requester restoring under their exception = %v", err)
	}
	if last := (*excused)[len(*excused)-1]; last.kind != domain.PublishReviewOfRestore || last.exception != "EXC-OK" || last.person.ID != "9" {
		t.Fatalf("excused %+v", last)
	}
	activation := restoreCandidate(9, "")
	activation.Activation = true
	query.rows[qReviewLatestRestore] = nil
	if err := reviews.CheckPublish(context.Background(), activation); err != nil {
		t.Fatalf("scheduled activation = %v", err)
	}
}

func TestRestoreConsumesItsReviewInTheRestoreTransaction(t *testing.T) {
	reviews, query, _ := restoreReviewService(t, true)
	reviews.Studio.Guards = []contract.PublishGuard{reviews}
	reviews.Studio.ReleaseWriters = []ReleaseWriter{reviews}
	query.rows[qStudioGetVersion] = [][]any{{restoredDefinition(restoredDigest), false}}
	query.rows[qStudioGetSettings] = [][]any{{"a:writer-a", `[]`, reviewedAt, nil}}
	request := domain.AgentRestoreRequest{AgentID: "writer-a", Version: "2"}

	if _, err := reviews.Studio.Restore(context.Background(), humanActor(8, 10), request); !errors.Is(err, domain.ErrConflict) || query.commits != 0 {
		t.Fatalf("an unreviewed restore = %v, commits %d", err, query.commits)
	}
	query.rows[qReviewLatestRestore] = [][]any{restoreReviewRow(21, restoredDigest, domain.PublishReviewApproved, 9, int64(10), nil, "3", "2")}
	query.rows[qReviewClaimRestore] = query.rows[qReviewLatestRestore]
	query.rows[qReviewConsume] = [][]any{{int64(21)}}

	release, err := reviews.Studio.Restore(context.Background(), humanActor(8, 10), request)
	if err != nil {
		t.Fatal(err)
	}
	if release.DefinitionDigest != restoredDigest || query.commits != 1 || len(query.args[qReviewClaimRestore]) != 2 || slices.Contains(query.queries, qReviewClaim) {
		t.Fatalf("release %+v, commits %d, queries %v", release, query.commits, query.queries)
	}
	// The deployment the review was requested over stays locked until the restore deploys.
	if !slices.Contains(query.queries, qReviewActiveLocked) || !strings.Contains(reviewQueries[qReviewActiveLocked], "FOR UPDATE OF dep") {
		t.Fatalf("the restore must read the active release under a lock: %v", query.queries)
	}
	if args := query.args[qReviewConsume]; args[0] != "1" || args[1] != int64(8) || args[3] != int64(21) {
		t.Fatalf("consume args = %v", args)
	}
	if audit := query.args[qReviewAudit]; audit[2] != "RESTORE_REVIEWED" || !strings.Contains(audit[3].(string), "restoring 2 approved in review 21") || audit[5] != "10" {
		t.Fatalf("audit = %v", audit)
	}

	query.rows[qReviewConsume] = nil
	if _, err = reviews.Studio.Restore(context.Background(), humanActor(8, 10), request); !errors.Is(err, domain.ErrConflict) || query.commits != 1 {
		t.Fatalf("a review consumed concurrently = %v", err)
	}
	consumed := restoreReviewRow(21, restoredDigest, domain.PublishReviewApproved, 9, int64(10), nil, "3", "2")
	consumed[15] = "1"
	query.rows[qReviewClaimRestore] = [][]any{consumed}
	if _, err = reviews.Studio.Restore(context.Background(), humanActor(8, 10), request); !errors.Is(err, domain.ErrConflict) || query.commits != 1 {
		t.Fatalf("a review consumed between guard and commit = %v", err)
	}
}

func TestPublishAndRestoreReviewsAreSeparateTracks(t *testing.T) {
	for name, filter := range map[string]string{
		qReviewLatest: "restore_version IS NULL", qReviewClaim: "restore_version IS NULL", qReviewSupersede: "restore_version IS NULL",
		qReviewLatestRestore: "restore_version IS NOT NULL", qReviewClaimRestore: "restore_version IS NOT NULL", qReviewSupersedeRestore: "restore_version IS NOT NULL",
	} {
		if sql := reviewQueries[name]; !strings.Contains(sql, filter) || !strings.Contains(sql, "partner_id = ? AND agent_id = ?") {
			t.Fatalf("%s must read only its own kind of review in the tenant:\n%s", name, sql)
		}
	}
	if sql := reviewQueries[qReviewClaimRestore]; !strings.Contains(sql, "FOR UPDATE") || strings.Contains(sql, "restore_version = ?") {
		t.Fatalf("the restore claim locks the latest restore review whichever version it names:\n%s", sql)
	}
}

func TestRestoreReviewStateIsStaleWhenTheActiveReleaseMoved(t *testing.T) {
	reviews, query, _ := restoreReviewService(t, true)
	query.rows[qReviewLatestRestore] = [][]any{restoreReviewRow(21, restoredDigest, domain.PublishReviewApproved, 9, int64(10), nil, "3", "2")}
	state, err := reviews.Review(context.Background(), humanActor(8, 10), "writer-a", domain.PublishReviewOfRestore)
	if err != nil {
		t.Fatal(err)
	}
	if state.Kind != domain.PublishReviewOfRestore || !state.Required || state.ActiveVersion != "3" || state.Stale || state.Review.RestoreVersion != "2" ||
		state.CurrentDigest != "" || state.OpenComments != nil || slices.Contains(query.queries, qCommentListStatus) {
		t.Fatalf("state = %+v", state)
	}
	query.rows[qReviewActive] = [][]any{{"4", activeDefinition()}}
	if state, err = reviews.Review(context.Background(), humanActor(8, 10), "writer-a", domain.PublishReviewOfRestore); err != nil || !state.Stale {
		t.Fatalf("moved active release = %+v, %v", state, err)
	}
	if _, err = reviews.Review(context.Background(), humanActor(8, 10), "writer-a", "rollback"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown kind = %v", err)
	}
}
