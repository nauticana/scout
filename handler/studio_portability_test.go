package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type portabilityBackend struct {
	document json.RawMessage
	imported domain.AgentImportRequest
	actor    domain.StudioActor
	agentID  string
	err      error
}

func (b *portabilityBackend) Export(_ context.Context, actor domain.StudioActor, agentID string) (json.RawMessage, error) {
	b.actor, b.agentID = actor, agentID
	return b.document, b.err
}

func (b *portabilityBackend) Import(_ context.Context, actor domain.StudioActor, request domain.AgentImportRequest) (domain.AgentSummary, error) {
	b.actor, b.imported = actor, request
	if b.err != nil {
		return domain.AgentSummary{}, b.err
	}
	return domain.AgentSummary{AgentID: "writer-b", AgentTypeID: "writer", DisplayName: "Imported", DraftRevision: 1}, nil
}

func servePortability(backend *portabilityBackend, method, target, body string) *httptest.ResponseRecorder {
	handler := &StudioPortabilityHandler{UserService: conversationUsers{}, Backend: backend}
	recorder := httptest.NewRecorder()
	path, _, _ := strings.Cut(target, "?")
	handler.Routes()[path](recorder, authenticatedRequest(method, target, []byte(body)))
	return recorder
}

func TestStudioPortabilityExportReturnsTheDocument(t *testing.T) {
	backend := &portabilityBackend{document: json.RawMessage(`{"kind":"AgentDefinition","purpose":"<b>write</b>"}`)}
	recorder := servePortability(backend, http.MethodGet, api.StudioExportPath+"?agent_name=writer-a", "")
	var document map[string]any
	if recorder.Code != http.StatusOK || envelopeData(recorder, &document) != nil {
		t.Fatalf("GET = %d %s", recorder.Code, recorder.Body.String())
	}
	if document["kind"] != "AgentDefinition" || document["purpose"] != "<b>write</b>" || backend.agentID != "writer-a" || backend.actor.TenantID != 7 {
		t.Fatalf("document = %v, backend = %+v", document, backend)
	}
	if recorder := servePortability(backend, http.MethodPost, api.StudioExportPath, "{}"); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("export is GET only, got %d", recorder.Code)
	}
}

func TestStudioPortabilityImportMapsTheRequest(t *testing.T) {
	backend := &portabilityBackend{}
	body := `{"document":{"kind":"AgentDefinition"},"display_name":"Imported","idempotency_key":"k-1",
		"references":{"knowledge":{"kb-1":"kb-9"},"entitlement":{"a":"b"}}}`
	recorder := servePortability(backend, http.MethodPost, api.StudioImportPath, body)
	var summary api.AgentSummary
	if recorder.Code != http.StatusOK || envelopeData(recorder, &summary) != nil || summary.AgentName != "writer-b" {
		t.Fatalf("POST = %d %s", recorder.Code, recorder.Body.String())
	}
	request := backend.imported
	if string(request.Document) != `{"kind":"AgentDefinition"}` || request.DisplayName != "Imported" || request.IdempotencyKey != "k-1" ||
		request.References[domain.ResourceKnowledge]["kb-1"] != "kb-9" || request.References[domain.ResourceEntitlement]["a"] != "b" {
		t.Fatalf("request = %+v", request)
	}

	replace := `{"document":{},"agent_name":"writer-a","expected_agent_revision":4}`
	if recorder := servePortability(backend, http.MethodPost, api.StudioImportPath, replace); recorder.Code != http.StatusOK ||
		backend.imported.AgentID != "writer-a" || backend.imported.ExpectedDraftRevision != 4 {
		t.Fatalf("replace = %d %+v", recorder.Code, backend.imported)
	}
	for name, body := range map[string]string{
		"unknown field":     `{"document":{},"idempotency_key":"k","extra":1}`,
		"document a string": `{"document":"x","idempotency_key":"k"}`,
		"no document":       `{"idempotency_key":"k"}`,
	} {
		if recorder := servePortability(&portabilityBackend{}, http.MethodPost, api.StudioImportPath, body); recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d %s", name, recorder.Code, recorder.Body.String())
		}
	}
}

func TestStudioPortabilityMapsServiceErrors(t *testing.T) {
	for err, status := range map[error]int{
		domain.ErrForbidden:                              http.StatusForbidden,
		domain.ErrNotFound:                               http.StatusNotFound,
		domain.ErrRevisionConflict:                       http.StatusConflict,
		domain.ErrSealed:                                 http.StatusConflict,
		fmt.Errorf("x: %w", domain.ErrAuthorityExceeded): http.StatusForbidden,
		domain.ErrNotReady:                               http.StatusServiceUnavailable,
	} {
		recorder := servePortability(&portabilityBackend{err: err}, http.MethodPost, api.StudioImportPath, `{"document":{},"idempotency_key":"k"}`)
		if recorder.Code != status {
			t.Errorf("%v: want %d, got %d", err, status, recorder.Code)
		}
	}
	feature := (&StudioPortabilityHandler{}).Feature()
	if feature.ID != StudioFeaturePortability || feature.Version != 1 || strings.Join(feature.Actions, ",") != "VIEW,CREATE,EDIT" {
		t.Fatalf("feature = %+v", feature)
	}
}

type templatesBackend struct {
	templates   []domain.AgentTemplate
	saved       domain.AgentTemplateSaveRequest
	publication domain.AgentTemplatePublication
	agentType   string
	err         error
}

func (b *templatesBackend) ListTemplates(_ context.Context, _ domain.StudioActor, agentTypeID string) ([]domain.AgentTemplate, error) {
	b.agentType = agentTypeID
	return b.templates, b.err
}

func (b *templatesBackend) SaveTemplate(_ context.Context, _ domain.StudioActor, request domain.AgentTemplateSaveRequest) (domain.AgentTemplate, error) {
	b.saved = request
	if b.err != nil {
		return domain.AgentTemplate{}, b.err
	}
	return b.templates[0], nil
}

func (b *templatesBackend) PublishTemplate(_ context.Context, _ domain.StudioActor, publication domain.AgentTemplatePublication) (domain.AgentTemplate, error) {
	b.publication = publication
	if b.err != nil {
		return domain.AgentTemplate{}, b.err
	}
	return b.templates[0], nil
}

func (b *templatesBackend) TemplateStatus(_ context.Context, _ domain.StudioActor, agentID string) (domain.AgentTemplateStatus, error) {
	return domain.AgentTemplateStatus{AgentID: agentID, TemplateID: "tpl-1", Version: 1, LatestVersion: 2, Newer: true, Available: true,
		DerivedFrom: &domain.DerivedFrom{Source: domain.CharterObjectRef{Kind: "AgentTemplate", ID: "tpl-1", External: true}, Version: "1", Digest: "sha256:x"}}, b.err
}

func serveTemplates(backend *templatesBackend, method, target, body string) *httptest.ResponseRecorder {
	handler := &StudioTemplatesHandler{UserService: conversationUsers{}, Backend: backend}
	recorder := httptest.NewRecorder()
	path, _, _ := strings.Cut(target, "?")
	handler.Routes()[path](recorder, authenticatedRequest(method, target, []byte(body)))
	return recorder
}

func TestStudioTemplatesRoutes(t *testing.T) {
	version := domain.AgentTemplateVersion{TemplateID: "tpl-1", Version: 2, AgentTypeID: "writer", DisplayName: "Formal", Digest: "abc",
		CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), SourceAgentID: "writer-a",
		Payload: domain.TemplatePayload{
			Bindings: []domain.PortableBinding{{ResourceKind: domain.ResourcePromptSection, Caption: "task", Language: "en-US", MergeMode: domain.MergeAppend, Value: json.RawMessage(`{"instruction":"x"}`)}},
			Slots:    map[domain.ResourceKind][]string{domain.ResourceKnowledge: {"docs"}},
		}}
	own, shared := version, version
	own.Owner, shared.Owner = domain.TemplateOwn, domain.TemplateShared
	backend := &templatesBackend{templates: []domain.AgentTemplate{
		{Latest: own, Audience: &domain.TemplateAudience{Tenants: []int64{9}}},
		{Latest: shared, Audience: &domain.TemplateAudience{All: true}},
	}}

	recorder := serveTemplates(backend, http.MethodGet, api.StudioTemplatesPath+"?agent_type=writer", "")
	var listed []api.AgentTemplate
	if recorder.Code != http.StatusOK || envelopeData(recorder, &listed) != nil || len(listed) != 2 || backend.agentType != "writer" {
		t.Fatalf("GET = %d %s", recorder.Code, recorder.Body.String())
	}
	if listed[0].SourceAgent != "writer-a" || listed[0].Audience == nil || listed[0].Audience.Tenants[0] != 9 || listed[0].Slots["knowledge"][0] != "docs" ||
		listed[0].Bindings[0].Caption != "task" {
		t.Fatalf("own = %+v", listed[0])
	}
	if listed[1].SourceAgent != "" || listed[1].Audience != nil {
		t.Fatalf("a shared template shows no source or audience: %+v", listed[1])
	}

	save := `{"agent_name":"writer-a","idempotency_key":"k","display_name":"Formal","slots":{"knowledge":{"kb-1":"docs"}}}`
	if recorder := serveTemplates(backend, http.MethodPost, api.StudioTemplatePath, save); recorder.Code != http.StatusOK ||
		backend.saved.AgentID != "writer-a" || backend.saved.Slots[domain.ResourceKnowledge]["kb-1"] != "docs" {
		t.Fatalf("save = %d %+v", recorder.Code, backend.saved)
	}
	publish := `{"template_id":"tpl-1","tenants":[9,10],"withdraw":true}`
	if recorder := serveTemplates(backend, http.MethodPost, api.StudioTemplatePublishPath, publish); recorder.Code != http.StatusOK ||
		!backend.publication.Withdraw || len(backend.publication.Audience.Tenants) != 2 {
		t.Fatalf("publish = %d %+v", recorder.Code, backend.publication)
	}
	recorder = serveTemplates(backend, http.MethodGet, api.StudioTemplateStatusPath+"?agent_name=writer-a", "")
	var status api.AgentTemplateStatus
	if recorder.Code != http.StatusOK || envelopeData(recorder, &status) != nil || !status.Newer || status.LatestVersion != 2 || status.DerivedFrom.Source.Kind != "AgentTemplate" {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := serveTemplates(backend, http.MethodPost, api.StudioTemplatePath, `{"agent_name":"a","display_name":"x","unknown":1}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown fields are refused, got %d", recorder.Code)
	}
	if recorder := serveTemplates(&templatesBackend{err: domain.ErrForbidden}, http.MethodPost, api.StudioTemplatePublishPath, publish); recorder.Code != http.StatusForbidden {
		t.Fatalf("a forbidden publish is 403, got %d", recorder.Code)
	}
	feature := (&StudioTemplatesHandler{}).Feature()
	if feature.ID != StudioFeatureTemplates || strings.Join(feature.Actions, ",") != "VIEW,CREATE,EDIT,TEMPLATE,TEMPLATE_PUBLISH" {
		t.Fatalf("feature = %+v", feature)
	}
}
