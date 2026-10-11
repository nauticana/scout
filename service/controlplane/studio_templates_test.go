package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type templateHarness struct {
	db        *seedDB
	studio    *StudioService
	templates *StudioTemplates
	store     *fake.AgentTemplateStore
	denied    map[domain.StudioAction]bool
	shareable map[string]bool
	bindings  []domain.ScopedBinding
}

func newTemplateHarness(t *testing.T) *templateHarness {
	t.Helper()
	h := &templateHarness{denied: map[domain.StudioAction]bool{}, shareable: map[string]bool{}, bindings: agentScopeBindings(), store: &fake.AgentTemplateStore{}}
	h.db = newSeedDB(map[string][][]any{
		qStudioGetDraft:     {draftRow(false)},
		qSeedPromptSections: {{int64(1), "task"}, {int64(2), "tone"}},
		qSeedAgentType:      {{"1", typeDigest}},
		qSeedTypeVersion:    {{typeDigest}},
		qSeedKnowledgeOwned: {{"kb-9"}},
		qStudioLockType:     {{"writer"}},
		qStudioTypeCensus:   {{int64(0), int64(0)}},
		qStudioCreateRecord: {{"writer-00000000000000000000"}},
		qStudioListAgents:   {{"writer-00000000000000000000", "writer", "From template", "draft", false, int64(1), int64(0), false, nil, nil, nil}},
	})
	h.db.byArgs[qSeedKnowledgeOwned] = func(args []any) ([][]any, bool) {
		if args[0] == int64(8) {
			return [][]any{{"kb-1"}}, true
		}
		return nil, false
	}
	h.studio = &StudioService{
		DB: h.db.repository(), Principals: studioPrincipals, MaxAgentsPerType: 5, Entropy: bytes.NewReader(make([]byte, 64)),
		Roles: fake.PrincipalAuthorizerFunc(func(_ context.Context, _ domain.Principal, _, action, value string) (domain.AuthorizationGrant, error) {
			allowed := !h.denied[domain.StudioAction(action)]
			if domain.StudioAction(action) == domain.StudioTemplatePublish {
				allowed = h.shareable[value]
			}
			return domain.AuthorizationGrant{Allowed: allowed, LowLimit: "*"}, nil
		}),
		Scopes: &fake.ScopeRepository{
			ChainFunc: studioScopes(nil).ChainFunc,
			BindingsFunc: func(_ context.Context, _ int64, ids []string, _ time.Time) ([]domain.ScopedBinding, error) {
				return slices.DeleteFunc(slices.Clone(h.bindings), func(b domain.ScopedBinding) bool { return !slices.Contains(ids, b.ScopeID) }), nil
			},
		},
		Sources: studioSourcesFake{resolved: domain.ResolvedPrompts{AgentID: "writer-a", AgentTypeID: "writer", LanguageCode: "en-US",
			Sections: []domain.PromptSectionSource{layered(1, 1, baseLayer("write", ""))}}},
		Assembler: &PromptDraftAssembler{Compiler: &PromptCompiler{}}, Compiler: &PromptCompiler{},
		Kinds: writerKinds, Catalog: writerModels, Templates: h.store, TemplateLimits: domain.TemplateLimits{MaxPerType: 3, MaxVersions: 3},
	}
	h.templates = &StudioTemplates{Studio: h.studio}
	return h
}

var ownerSlots = map[domain.ResourceKind]map[string]string{
	domain.ResourceKnowledge: {"kb-1": "docs"}, domain.ResourceEntitlement: {"label-a": "audience"},
}

func (h *templateHarness) save(key, name string) (domain.AgentTemplate, error) {
	return h.templates.SaveTemplate(context.Background(), humanActor(8, 9), domain.AgentTemplateSaveRequest{
		AgentID: "writer-a", IdempotencyKey: key, DisplayName: name, Slots: ownerSlots,
	})
}

func payloadValue(t *testing.T, payload domain.TemplatePayload, kind domain.ResourceKind) string {
	t.Helper()
	for _, binding := range payload.Bindings {
		if binding.ResourceKind == kind {
			return string(binding.Value)
		}
	}
	t.Fatalf("payload has no %s binding: %+v", kind, payload)
	return ""
}

func TestStudioTemplateSaveStoresSlotNamesNeverOwnerValues(t *testing.T) {
	h := newTemplateHarness(t)
	saved, err := h.save("key-1", "Formal writer")
	if err != nil {
		t.Fatal(err)
	}
	latest := saved.Latest
	if latest.TemplateID != "tpl-00000000000000000001" || latest.Version != 1 || latest.Owner != domain.TemplateOwn || latest.SourceAgentID != "writer-a" ||
		latest.TypeVersion != "1" || latest.TypeDigest != typeDigest || saved.Audience == nil || saved.Audience.All {
		t.Fatalf("saved = %+v", saved)
	}
	if payloadValue(t, latest.Payload, domain.ResourceKnowledge) != `["docs"]` || payloadValue(t, latest.Payload, domain.ResourceEntitlement) != `["audience"]` {
		t.Fatalf("payload = %+v", latest.Payload)
	}
	encoded, _ := json.Marshal(latest.Payload)
	if strings.Contains(string(encoded), "kb-1") || strings.Contains(string(encoded), "label-a") {
		t.Fatalf("a template carries an owner value: %s", encoded)
	}
	if latest.Payload.Slots[domain.ResourceKnowledge][0] != "docs" || latest.Payload.Slots[domain.ResourceEntitlement][0] != "audience" {
		t.Fatalf("slots = %v", latest.Payload.Slots)
	}
	digest, err := templatePayloadDigest(latest.Payload)
	if err != nil || digest != latest.Digest {
		t.Fatalf("the digest covers the canonical payload: %v", err)
	}

	if replay, err := h.save("key-1", "Formal writer"); err != nil || replay.Latest.TemplateID != latest.TemplateID {
		t.Fatalf("a key replay returns the same template: %+v %v", replay, err)
	}
	if _, err := h.save("key-1", "Another name"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("the same key with other content conflicts, got %v", err)
	}
	update := domain.AgentTemplateSaveRequest{AgentID: "writer-a", TemplateID: latest.TemplateID, DisplayName: "Formal writer", Slots: ownerSlots}
	if same, err := h.templates.SaveTemplate(context.Background(), humanActor(8, 9), update); err != nil || same.Latest.Version != 1 {
		t.Fatalf("equal content stores no version: %+v %v", same, err)
	}
	update.Description = "for older readers"
	if second, err := h.templates.SaveTemplate(context.Background(), humanActor(8, 9), update); err != nil || second.Latest.Version != 2 {
		t.Fatalf("a changed label is a new version: %+v %v", second, err)
	}
	h.studio.TemplateLimits.MaxVersions = 2
	update.Description = "for younger readers"
	if _, err := h.templates.SaveTemplate(context.Background(), humanActor(8, 9), update); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("the version cap refuses, got %v", err)
	}
	h.studio.TemplateLimits.MaxPerType = 1
	if _, err := h.save("key-2", "Second"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("the per-type cap refuses, got %v", err)
	}
	h.studio.TemplateLimits = domain.TemplateLimits{}
	if _, err := newTemplateHarness(t).templates.SaveTemplate(context.Background(), humanActor(8, 9), domain.AgentTemplateSaveRequest{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an empty request is invalid, got %v", err)
	}
	zero := newTemplateHarness(t)
	zero.studio.TemplateLimits = domain.TemplateLimits{}
	if _, err := zero.save("key-1", "Formal writer"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("zero limits refuse every template, got %v", err)
	}
	if _, err := h.templates.SaveTemplate(context.Background(), humanActor(7, 9), domain.AgentTemplateSaveRequest{
		AgentID: "writer-a", TemplateID: latest.TemplateID, DisplayName: "Mine now", Slots: ownerSlots,
	}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant's template is not found, got %v", err)
	}
}

func TestStudioTemplateSaveRefusals(t *testing.T) {
	cases := map[string]struct {
		prepare func(*templateHarness, *domain.AgentTemplateSaveRequest)
		want    error
	}{
		"edit denied":     {func(h *templateHarness, _ *domain.AgentTemplateSaveRequest) { h.denied[domain.StudioEdit] = true }, domain.ErrForbidden},
		"template denied": {func(h *templateHarness, _ *domain.AgentTemplateSaveRequest) { h.denied[domain.StudioTemplate] = true }, domain.ErrForbidden},
		"unmapped owner value": {func(_ *templateHarness, r *domain.AgentTemplateSaveRequest) {
			r.Slots = map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"kb-1": "docs"}}
		}, domain.ErrValidation},
		"identity mapping": {func(h *templateHarness, r *domain.AgentTemplateSaveRequest) {
			h.bindings[1].Value = []byte(`["docs"]`)
			r.Slots = map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"docs": "docs"}, domain.ResourceEntitlement: {"label-a": "audience"}}
		}, domain.ErrValidation},
		"slot naming an owned knowledge base": {func(h *templateHarness, _ *domain.AgentTemplateSaveRequest) {
			h.db.byArgs[qSeedKnowledgeOwned] = func([]any) ([][]any, bool) { return [][]any{{"docs"}}, true }
		}, domain.ErrValidation},
		"unused slot": {func(_ *templateHarness, r *domain.AgentTemplateSaveRequest) {
			r.Slots = map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"kb-1": "docs", "kb-2": "more"}, domain.ResourceEntitlement: {"label-a": "audience"}}
		}, domain.ErrValidation},
		"no key": {func(_ *templateHarness, r *domain.AgentTemplateSaveRequest) { r.IdempotencyKey = "" }, domain.ErrValidation},
		"bad slot": {func(_ *templateHarness, r *domain.AgentTemplateSaveRequest) {
			r.Slots[domain.ResourceKnowledge] = map[string]string{"kb-1": "Docs!"}
		}, domain.ErrValidation},
		"tool slot": {func(_ *templateHarness, r *domain.AgentTemplateSaveRequest) {
			r.Slots[domain.ResourceTool] = map[string]string{"search": "s"}
		}, domain.ErrValidation},
		"no store": {func(h *templateHarness, _ *domain.AgentTemplateSaveRequest) { h.studio.Templates = nil }, domain.ErrNotReady},
		"product kind": {func(h *templateHarness, _ *domain.AgentTemplateSaveRequest) {
			h.bindings = append(h.bindings, domain.ScopedBinding{ScopeID: "a:writer-a", ResourceKind: "crm", ResourceID: "crm", Value: []byte(`1`)})
		}, domain.ErrValidation},
	}
	for name, test := range cases {
		h := newTemplateHarness(t)
		request := domain.AgentTemplateSaveRequest{AgentID: "writer-a", IdempotencyKey: "key-1", DisplayName: "Formal writer",
			Slots: map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"kb-1": "docs"}, domain.ResourceEntitlement: {"label-a": "audience"}}}
		test.prepare(h, &request)
		if _, err := h.templates.SaveTemplate(context.Background(), humanActor(8, 9), request); !errors.Is(err, test.want) {
			t.Errorf("%s: want %v, got %v", name, test.want, err)
		}
		if templates, _ := h.store.Usable(context.Background(), 8, "writer"); len(templates) != 0 {
			t.Errorf("%s: a refused save stored %+v", name, templates)
		}
	}
}

func TestStudioTemplateVisibilityAndPublication(t *testing.T) {
	h := newTemplateHarness(t)
	saved, err := h.save("key-1", "Formal writer")
	if err != nil {
		t.Fatal(err)
	}
	id := saved.Latest.TemplateID
	ctx := context.Background()
	_, missing := h.store.Version(ctx, 9, "tpl-ffffffffffffffffffff", 0)
	if _, private := h.store.Version(ctx, 9, id, 0); !errors.Is(private, domain.ErrNotFound) || !errors.Is(missing, domain.ErrNotFound) {
		t.Fatalf("a private template answers exactly like a missing one: %v / %v", private, missing)
	}
	if listed, err := h.templates.ListTemplates(ctx, humanActor(9, 21), "writer"); err != nil || len(listed) != 0 {
		t.Fatalf("tenant 9 sees nothing yet: %+v %v", listed, err)
	}

	share := domain.AgentTemplatePublication{TemplateID: id, Audience: domain.TemplateAudience{Tenants: []int64{9}}}
	if _, err := h.templates.PublishTemplate(ctx, humanActor(8, 9), share); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("no role holds TEMPLATE_PUBLISH unless granted, got %v", err)
	}
	h.shareable["template_audience:tenants"] = true
	published, err := h.templates.PublishTemplate(ctx, humanActor(8, 9), share)
	if err != nil || published.Audience == nil || !slices.Equal(published.Audience.Tenants, []int64{9}) {
		t.Fatalf("published = %+v %v", published, err)
	}
	if again, err := h.templates.PublishTemplate(ctx, humanActor(8, 9), share); err != nil || len(again.Audience.Tenants) != 1 {
		t.Fatalf("publishing again is idempotent: %+v %v", again, err)
	}
	listed, err := h.templates.ListTemplates(ctx, humanActor(9, 21), "writer")
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed = %+v %v", listed, err)
	}
	shared := listed[0]
	if shared.Latest.Owner != domain.TemplateShared || shared.Latest.SourceAgentID != "" || shared.Audience != nil {
		t.Fatalf("a shared listing carries no source agent or audience: %+v", shared)
	}
	if other, _ := h.templates.ListTemplates(ctx, humanActor(10, 21), "writer"); len(other) != 0 {
		t.Fatalf("tenant 10 is not in the audience: %+v", other)
	}
	if _, err := h.templates.PublishTemplate(ctx, humanActor(8, 9), domain.AgentTemplatePublication{TemplateID: id, Audience: domain.TemplateAudience{All: true}}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("sharing with every tenant needs its own grant, got %v", err)
	}
	h.shareable["template_audience:all"] = true
	if _, err := h.templates.PublishTemplate(ctx, humanActor(8, 9), domain.AgentTemplatePublication{TemplateID: id, Audience: domain.TemplateAudience{All: true}}); err != nil {
		t.Fatal(err)
	}
	if other, _ := h.templates.ListTemplates(ctx, humanActor(10, 21), "writer"); len(other) != 1 {
		t.Fatalf("published to all, tenant 10 sees it: %+v", other)
	}
	for _, withdraw := range []domain.AgentTemplatePublication{
		{TemplateID: id, Audience: domain.TemplateAudience{All: true}, Withdraw: true},
		{TemplateID: id, Audience: domain.TemplateAudience{Tenants: []int64{9}}, Withdraw: true},
	} {
		if _, err := h.templates.PublishTemplate(ctx, humanActor(8, 9), withdraw); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.Version(ctx, 9, id, 0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a withdrawn template is hidden, got %v", err)
	}
	if _, err := h.templates.PublishTemplate(ctx, humanActor(9, 21), share); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("publishing to the caller's own tenant is invalid, got %v", err)
	}
	if _, err := h.templates.PublishTemplate(ctx, humanActor(7, 21), domain.AgentTemplatePublication{TemplateID: id, Audience: domain.TemplateAudience{Tenants: []int64{9}}}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant cannot publish it, got %v", err)
	}
	many := make([]int64, 101)
	for index := range many {
		many[index] = int64(index + 100)
	}
	for name, audience := range map[string]domain.TemplateAudience{"both": {All: true, Tenants: []int64{9}}, "neither": {}, "too many": {Tenants: many}, "duplicate": {Tenants: []int64{9, 9}}} {
		if _, err := h.templates.PublishTemplate(ctx, humanActor(8, 9), domain.AgentTemplatePublication{TemplateID: id, Audience: audience}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: want a validation error, got %v", name, err)
		}
	}
	h.denied[domain.StudioCreate], h.denied[domain.StudioTemplate] = true, true
	if _, err := h.templates.ListTemplates(ctx, humanActor(9, 21), "writer"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("listing needs CREATE or TEMPLATE on the type, got %v", err)
	}
}

func TestStudioCreateFromATemplate(t *testing.T) {
	h := newTemplateHarness(t)
	saved, err := h.save("key-1", "Formal writer")
	if err != nil {
		t.Fatal(err)
	}
	id := saved.Latest.TemplateID
	h.shareable["template_audience:tenants"] = true
	if _, err = h.templates.PublishTemplate(context.Background(), humanActor(8, 9), domain.AgentTemplatePublication{TemplateID: id, Audience: domain.TemplateAudience{Tenants: []int64{9}}}); err != nil {
		t.Fatal(err)
	}
	references := map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"docs": "kb-9"}, domain.ResourceEntitlement: {"audience": "label-z"}}
	request := domain.AgentCreateRequest{AgentTypeID: "writer", DisplayName: "From template", TemplateID: id, IdempotencyKey: "create-1", References: references}

	created, err := h.studio.Create(context.Background(), humanActor(9, 21), request)
	if err != nil {
		t.Fatal(err)
	}
	if created.AgentID != "writer-00000000000000000000" {
		t.Fatalf("created = %+v", created)
	}
	values := map[string]string{}
	for _, args := range h.db.args(qStudioInsertSetting) {
		values[args[2].(string)] = args[7].(string)
	}
	if values["knowledge"] != `["kb-9"]` || values["entitlement"] != `["label-z"]` || values["prompt_section"] != `{"instruction":"be brief"}` {
		t.Fatalf("values = %v", values)
	}
	origin := h.db.last(qSeedPutOrigin)
	if origin[2] != "AgentTemplate" || origin[3] != id || origin[6] != "1" || origin[7] != "sha256:"+saved.Latest.Digest ||
		origin[8] != int64(8) || origin[9] != id || origin[10] != int64(1) || origin[11] != nil {
		t.Fatalf("origin = %v", origin)
	}
	if audit := h.db.last(qStudioAudit); audit[2] != "CREATE" || audit[3] != "template "+id+" version 1" {
		t.Fatalf("audit = %v", audit)
	}

	refusals := map[string]func(*templateHarness, *domain.AgentCreateRequest){
		"unmapped slot": func(_ *templateHarness, r *domain.AgentCreateRequest) {
			r.References = map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"docs": "kb-9"}}
		},
		"unowned target":        func(h *templateHarness, _ *domain.AgentCreateRequest) { h.db.rows[qSeedKnowledgeOwned] = nil },
		"version not permitted": func(_ *templateHarness, r *domain.AgentCreateRequest) { r.TemplateVersion = 5 },
		"type digest mismatch": func(h *templateHarness, _ *domain.AgentCreateRequest) {
			h.db.rows[qSeedTypeVersion] = [][]any{{strings.Repeat("0", 64)}}
		},
		"another tenant's slots": func(_ *templateHarness, r *domain.AgentCreateRequest) {
			r.References[domain.ResourceKnowledge]["other"] = "kb-9"
		},
		"type mismatch": func(h *templateHarness, r *domain.AgentCreateRequest) {
			r.AgentTypeID = "editor"
			h.studio.Kinds = nil
		},
		"no store": func(h *templateHarness, _ *domain.AgentCreateRequest) { h.studio.Templates = nil },
	}
	for name, change := range refusals {
		h2 := newTemplateHarness(t)
		h2.store = h.store
		h2.studio.Templates = h.store
		attempt := request
		attempt.IdempotencyKey = "create-" + name
		attempt.References = map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"docs": "kb-9"}, domain.ResourceEntitlement: {"audience": "label-z"}}
		change(h2, &attempt)
		if _, err := h2.studio.Create(context.Background(), humanActor(9, 21), attempt); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: want a validation error, got %v", name, err)
		}
		if h2.db.count(qStudioInsertSetting) != 0 || h2.db.count(qProvisionProfile) != 0 || h2.db.commits != 0 {
			t.Errorf("%s: a refused create wrote %v", name, h2.db.names())
		}
	}
}

func TestStudioTemplateStatusReportsANewerVersion(t *testing.T) {
	h := newTemplateHarness(t)
	saved, err := h.save("key-1", "Formal writer")
	if err != nil {
		t.Fatal(err)
	}
	id := saved.Latest.TemplateID
	h.db.rows[qSeedDraftOrigin] = [][]any{{"AgentTemplate", id, nil, true, "1", "sha256:" + saved.Latest.Digest, int64(8), id, int64(1), nil, nil}}
	status, err := h.templates.TemplateStatus(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || !status.Available || status.Newer || status.Version != 1 || status.DerivedFrom == nil {
		t.Fatalf("status = %+v %v", status, err)
	}
	if _, err = h.templates.SaveTemplate(context.Background(), humanActor(8, 9), domain.AgentTemplateSaveRequest{
		AgentID: "writer-a", TemplateID: id, DisplayName: "Formal writer v2", Slots: ownerSlots,
	}); err != nil {
		t.Fatal(err)
	}
	if status, err = h.templates.TemplateStatus(context.Background(), humanActor(8, 9), "writer-a"); err != nil || !status.Newer || status.LatestVersion != 2 {
		t.Fatalf("status = %+v %v", status, err)
	}
	if status, err = h.templates.TemplateStatus(context.Background(), humanActor(9, 21), "writer-a"); err != nil || status.Available || status.Newer {
		t.Fatalf("a template the tenant may not use is unavailable: %+v %v", status, err)
	}
	h.db.rows[qSeedDraftOrigin] = nil
	if status, err = h.templates.TemplateStatus(context.Background(), humanActor(8, 9), "writer-a"); err != nil || status.DerivedFrom != nil || status.TemplateID != "" {
		t.Fatalf("an agent derived from nothing reports nothing: %+v %v", status, err)
	}
	h.denied[domain.StudioView] = true
	if _, err = h.templates.TemplateStatus(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("status needs VIEW, got %v", err)
	}
}

func TestStudioTemplateRegisterPlatform(t *testing.T) {
	h := newTemplateHarness(t)
	version := domain.AgentTemplateVersion{
		TemplateID: "writer.formal", Version: 1, AgentTypeID: "writer", DisplayName: "Formal writer",
		Payload: domain.TemplatePayload{
			Bindings: []domain.PortableBinding{
				{ResourceKind: domain.ResourcePromptSection, Caption: "task", Language: "en-US", MergeMode: domain.MergeAppend, Value: json.RawMessage(`{"instruction":"formal"}`)},
				{ResourceKind: domain.ResourceKnowledge, ResourceID: "knowledge", MergeMode: domain.MergeReplace, Value: json.RawMessage(`["docs"]`)},
			},
			Slots: map[domain.ResourceKind][]string{domain.ResourceKnowledge: {"docs"}},
		},
	}
	ctx := context.Background()
	if err := h.templates.RegisterPlatform(ctx, version); err != nil {
		t.Fatal(err)
	}
	if err := h.templates.RegisterPlatform(ctx, version); err != nil {
		t.Fatalf("registering equal content again is a no-op: %v", err)
	}
	changed := version
	changed.DisplayName = "Changed"
	if err := h.templates.RegisterPlatform(ctx, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a registered version is immutable, got %v", err)
	}
	skipped := version
	skipped.Version = 3
	if err := h.templates.RegisterPlatform(ctx, skipped); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("versions are sequential, got %v", err)
	}
	reserved := version
	reserved.TemplateID = "tpl-00000000000000000009"
	if err := h.templates.RegisterPlatform(ctx, reserved); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("the tenant prefix is reserved, got %v", err)
	}
	undeclared := version
	undeclared.TemplateID = "writer.other"
	undeclared.Payload.Slots = nil
	if err := h.templates.RegisterPlatform(ctx, undeclared); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a knowledge value must be a declared slot, got %v", err)
	}
	unused := version
	unused.TemplateID = "writer.unused"
	unused.Payload.Slots = map[domain.ResourceKind][]string{domain.ResourceKnowledge: {"docs", "faq"}}
	var invalid *StudioValidationError
	if err := h.templates.RegisterPlatform(ctx, unused); !errors.As(err, &invalid) || len(invalid.Fields) != 1 ||
		invalid.Fields[0].Field != "slots.knowledge" || !strings.Contains(invalid.Fields[0].Message, "faq") {
		t.Fatalf("a declared slot no binding uses would make the version unusable, got %v", err)
	}
	listed, err := h.templates.ListTemplates(ctx, humanActor(42, 1), "writer")
	if err != nil || len(listed) != 1 || listed[0].Latest.Owner != domain.TemplatePlatform {
		t.Fatalf("a platform template is usable by every tenant: %+v %v", listed, err)
	}
	created, err := h.studio.Create(ctx, humanActor(42, 1), domain.AgentCreateRequest{AgentTypeID: "writer", DisplayName: "From template",
		TemplateID: "writer.formal", IdempotencyKey: "k", References: map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"docs": "kb-9"}}})
	if err != nil || created.AgentID == "" {
		t.Fatalf("create from a platform template: %+v %v", created, err)
	}
	if origin := h.db.last(qSeedPutOrigin); origin[8] != nil || origin[11] != "writer.formal" || origin[12] != int64(1) {
		t.Fatalf("a platform origin names the catalog: %v", origin)
	}
}
