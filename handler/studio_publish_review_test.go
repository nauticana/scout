package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type reviewBackendFake struct {
	actor    domain.StudioActor
	request  domain.PublishReviewRequest
	decision domain.PublishReviewDecision
	agentID  string
	kind     domain.PublishReviewKind
	err      error
}

func (f *reviewBackendFake) Request(_ context.Context, actor domain.StudioActor, request domain.PublishReviewRequest) (domain.PublishReview, error) {
	f.actor, f.request = actor, request
	return domain.PublishReview{ReviewID: 11, AgentID: request.AgentID, Status: domain.PublishReviewRequested, RestoreVersion: request.RestoreVersion}, f.err
}

func (f *reviewBackendFake) Review(_ context.Context, actor domain.StudioActor, agentID string, kind domain.PublishReviewKind) (domain.PublishReviewState, error) {
	f.actor, f.agentID, f.kind = actor, agentID, kind
	if kind == domain.PublishReviewOfRestore {
		return domain.PublishReviewState{AgentID: agentID, Kind: kind, Required: true, ActiveVersion: "3",
			Review: &domain.PublishReview{ReviewID: 21, BaseVersion: "3", RestoreVersion: "2"}}, f.err
	}
	return domain.PublishReviewState{AgentID: agentID, Required: true, Stale: true, Review: &domain.PublishReview{ReviewID: 11,
			Diff: domain.PublishReviewDiff{Settings: []domain.SettingChange{{ResourceKind: domain.ResourceTool, ResourceID: "tool", Change: domain.DiffAdded, After: json.RawMessage(`["search"]`)}}}},
			OpenComments: []domain.DraftComment{{CommentID: 3, Anchor: domain.CommentOnDraft, Author: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "10"}}}},
		f.err
}

func (f *reviewBackendFake) Approve(_ context.Context, actor domain.StudioActor, decision domain.PublishReviewDecision) (domain.PublishReview, error) {
	f.actor, f.decision = actor, decision
	return domain.PublishReview{ReviewID: decision.ReviewID, Status: domain.PublishReviewApproved}, f.err
}

func (f *reviewBackendFake) Reject(_ context.Context, actor domain.StudioActor, decision domain.PublishReviewDecision) (domain.PublishReview, error) {
	f.actor, f.decision = actor, decision
	return domain.PublishReview{ReviewID: decision.ReviewID, Status: domain.PublishReviewRejected}, f.err
}

func TestPublishReviewRoutesDispatchByMethodAndMapTheWire(t *testing.T) {
	backend := &reviewBackendFake{}
	handler := &StudioPublishReviewHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Backend: backend}
	routes := handler.Routes()
	if len(routes) != 3 || handler.Feature().ID != StudioFeaturePublishReview {
		t.Fatalf("routes = %v", routes)
	}

	recorder := httptest.NewRecorder()
	routes[api.StudioPublishReviewPath](recorder, authenticatedRequest(http.MethodPost, api.StudioPublishReviewPath, []byte(`{"agent_name":"writer-a","note":"go","sod_exception_id":"EXC-1"}`)))
	if recorder.Code != http.StatusOK || backend.request.AgentID != "writer-a" || backend.request.Note != "go" || backend.request.SodExceptionID != "EXC-1" ||
		backend.actor.Principal.ID != "42" {
		t.Fatalf("request: %d %s %+v", recorder.Code, recorder.Body.String(), backend)
	}

	recorder = httptest.NewRecorder()
	routes[api.StudioPublishReviewPath](recorder, authenticatedRequest(http.MethodGet, api.StudioPublishReviewPath+"?agent_name=writer-a", nil))
	var state api.PublishReviewState
	if err := unwrapData(recorder.Body.Bytes(), &state); err != nil || recorder.Code != http.StatusOK {
		t.Fatalf("state: %d %s", recorder.Code, recorder.Body.String())
	}
	if !state.Required || !state.Stale || state.Review.ReviewID != 11 || string(state.Review.Diff.Settings[0].After) != `["search"]` ||
		state.Review.Diff.Causes == nil || state.Review.Diff.Editors == nil || len(state.OpenComments) != 1 || state.OpenComments[0].AuthorID != "10" {
		t.Fatalf("state = %+v", state)
	}

	recorder = httptest.NewRecorder()
	routes[api.StudioPublishReviewApprovePath](recorder, authenticatedRequest(http.MethodPost, api.StudioPublishReviewApprovePath,
		[]byte(`{"agent_name":"writer-a","review_id":11,"note":"ok"}`)))
	if recorder.Code != http.StatusOK || backend.decision.ReviewID != 11 || backend.decision.Note != "ok" {
		t.Fatalf("approve: %d %+v", recorder.Code, backend.decision)
	}

	backend.err = domain.ErrForbidden
	recorder = httptest.NewRecorder()
	routes[api.StudioPublishReviewRejectPath](recorder, authenticatedRequest(http.MethodPost, api.StudioPublishReviewRejectPath, []byte(`{"agent_name":"writer-a","review_id":11}`)))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("refused reject status = %d", recorder.Code)
	}
}

func TestRestoreReviewTravelsAsRestoreVersionAndKind(t *testing.T) {
	backend := &reviewBackendFake{}
	handler := &StudioPublishReviewHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Backend: backend}
	routes := handler.Routes()
	if actions := handler.Feature().Actions; !slices.Contains(actions, string(domain.StudioRestore)) {
		t.Fatalf("feature actions = %v", actions)
	}

	recorder := httptest.NewRecorder()
	routes[api.StudioPublishReviewPath](recorder, authenticatedRequest(http.MethodPost, api.StudioPublishReviewPath, []byte(`{"agent_name":"writer-a","restore_version":"2"}`)))
	var review api.PublishReview
	if err := unwrapData(recorder.Body.Bytes(), &review); err != nil || recorder.Code != http.StatusOK || backend.request.RestoreVersion != "2" || review.RestoreVersion != "2" {
		t.Fatalf("request: %d %s %+v", recorder.Code, recorder.Body.String(), backend.request)
	}

	recorder = httptest.NewRecorder()
	routes[api.StudioPublishReviewPath](recorder, authenticatedRequest(http.MethodGet, api.StudioPublishReviewPath+"?agent_name=writer-a&kind=restore", nil))
	var state api.PublishReviewState
	if err := unwrapData(recorder.Body.Bytes(), &state); err != nil || recorder.Code != http.StatusOK {
		t.Fatalf("state: %d %s", recorder.Code, recorder.Body.String())
	}
	if backend.kind != domain.PublishReviewOfRestore || state.Kind != "restore" || state.ActiveVersion != "3" || state.Review.RestoreVersion != "2" || state.Review.BaseVersion != "3" {
		t.Fatalf("state = %+v", state)
	}

	recorder = httptest.NewRecorder()
	routes[api.StudioPublishReviewPath](recorder, authenticatedRequest(http.MethodGet, api.StudioPublishReviewPath+"?agent_name=writer-a", nil))
	if recorder.Code != http.StatusOK || backend.kind != "" {
		t.Fatalf("an absent kind is passed through empty: %d %q", recorder.Code, backend.kind)
	}
}

// unwrapData decodes the data member of keel's response envelope.
func unwrapData(body []byte, target any) error {
	envelope := struct {
		Data json.RawMessage `json:"data"`
	}{}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	return json.Unmarshal(envelope.Data, target)
}

type commentBackendFake struct {
	status  domain.DraftCommentStatus
	comment domain.DraftComment
	resolve int64
}

func (f *commentBackendFake) Comments(_ context.Context, _ domain.StudioActor, _ string, status domain.DraftCommentStatus) ([]domain.DraftComment, error) {
	f.status = status
	return nil, nil
}

func (f *commentBackendFake) Comment(_ context.Context, actor domain.StudioActor, comment domain.DraftComment) (domain.DraftComment, error) {
	f.comment = comment
	comment.CommentID, comment.Author, comment.CreatedAt = 5, actor.Principal, time.Now()
	return comment, nil
}

func (f *commentBackendFake) Resolve(_ context.Context, actor domain.StudioActor, agentID string, commentID int64) (domain.DraftComment, error) {
	f.resolve = commentID
	return domain.DraftComment{CommentID: commentID, AgentID: agentID, Status: domain.DraftCommentResolved, ResolvedBy: &actor.Principal}, nil
}

func TestDraftCommentRoutesMapTheAnchor(t *testing.T) {
	backend := &commentBackendFake{}
	handler := &StudioDraftCommentHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Backend: backend}
	routes := handler.Routes()

	recorder := httptest.NewRecorder()
	routes[api.StudioCommentPath](recorder, authenticatedRequest(http.MethodPost, api.StudioCommentPath,
		[]byte(`{"agent_name":"writer-a","draft_revision":4,"anchor":"S","resource_kind":"budget","resource_id":"budget","body":"high","status":"R"}`)))
	var created api.DraftComment
	if err := unwrapData(recorder.Body.Bytes(), &created); err != nil || recorder.Code != http.StatusOK {
		t.Fatalf("comment: %d %s", recorder.Code, recorder.Body.String())
	}
	if backend.comment.Anchor != domain.CommentOnSetting || backend.comment.ResourceKind != domain.ResourceBudget || backend.comment.Status != "" ||
		created.CommentID != 5 || created.AuthorID != "42" {
		t.Fatalf("comment = %+v, created = %+v", backend.comment, created)
	}

	recorder = httptest.NewRecorder()
	routes[api.StudioCommentsPath](recorder, authenticatedRequest(http.MethodGet, api.StudioCommentsPath+"?agent_name=writer-a&status="+string(domain.DraftCommentOpen), nil))
	if recorder.Code != http.StatusOK || backend.status != domain.DraftCommentOpen || strings.Contains(recorder.Body.String(), `"data":null`) {
		t.Fatalf("list: %d %q", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	routes[api.StudioCommentResolvePath](recorder, authenticatedRequest(http.MethodPost, api.StudioCommentResolvePath, []byte(`{"agent_name":"writer-a","comment_id":5}`)))
	if recorder.Code != http.StatusOK || backend.resolve != 5 {
		t.Fatalf("resolve: %d", recorder.Code)
	}
}

func TestPromptLayerExposesItsLastEditor(t *testing.T) {
	editor := int64(9)
	draft := domain.AgentDraft{AgentID: "writer-a", Languages: []domain.AgentLanguageDraft{{LanguageCode: "en-US", Sections: []domain.AgentPromptSection{{
		PromptSectionID: 1, Layers: []domain.PromptLayer{{ScopeID: "global"}, {ScopeID: "a:writer-a", BoundBy: &editor}},
	}}}}}
	layers := apiDraft(draft).Languages[0].PromptSections[0].Layers
	if layers[0].BoundBy != nil || layers[1].BoundBy == nil || *layers[1].BoundBy != 9 {
		t.Fatalf("layers = %+v", layers)
	}
	request := api.AgentDraft{AgentName: "writer-a", Languages: []api.AgentLanguageDraft{{LanguageCode: "en-US", PromptSections: []api.AgentPromptSection{{
		PromptHeaderID: 1, Layers: []api.AgentPromptLayer{{ScopeID: "a:writer-a", ScopeKind: "agent", MergeMode: string(domain.MergeReplace), Instruction: "x", BoundBy: &editor}},
	}}}}}
	if layer := domainDraft(request).Languages[0].Sections[0].Layers[0]; layer.BoundBy != nil {
		t.Fatalf("bound_by must not be read from the request: %+v", layer)
	}
}
