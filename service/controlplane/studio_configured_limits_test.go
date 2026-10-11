package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestStudioServiceRefusesNegativeLimits(t *testing.T) {
	for name, set := range map[string]func(*StudioService){
		"activation lead":    func(s *StudioService) { s.MaxActivationLead = -1 },
		"portable bindings":  func(s *StudioService) { s.MaxPortableBindings = -1 },
		"references":         func(s *StudioService) { s.MaxReferences = -1 },
		"portable extension": func(s *StudioService) { s.MaxPortableExtension = -1 },
		"template audience":  func(s *StudioService) { s.MaxTemplateAudience = -1 },
		"comments shown":     func(s *StudioService) { s.MaxCommentsShown = -1 },
	} {
		service, _ := publishService()
		set(service)
		if _, err := service.ListAgents(context.Background(), humanActor(8, 9)); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: error = %v, want a validation refusal", name, err)
		}
	}
}

func TestPortableBindingLimit(t *testing.T) {
	bindings := []domain.PortableBinding{{ResourceKind: domain.ResourceTool, ResourceID: "a"}, {ResourceKind: domain.ResourceTool, ResourceID: "b"}}
	service := &StudioService{MaxPortableBindings: 1}
	_, err := service.adopt(context.Background(), humanActor(8, 9), "writer", "document.bindings", bindings, nil, adoptImport, nil)
	if fields := fieldsOf(t, err); !slices.Equal(fields, []string{"document.bindings"}) || !strings.Contains(err.(*StudioValidationError).Fields[0].Message, "1 bindings") {
		t.Fatalf("fields = %v, err = %v", fields, err)
	}
	if limit := (&StudioService{}).portableBindingLimit(); limit != DefaultMaxPortableBindings {
		t.Fatalf("zero limit = %d, want %d", limit, DefaultMaxPortableBindings)
	}
}

func TestReferenceLimit(t *testing.T) {
	references := map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"docs": "kb-9", "faq": "kb-10"}}
	request := domain.AgentCreateRequest{AgentTypeID: "writer", DisplayName: "From template", IdempotencyKey: "k", TemplateID: "tpl-1", References: references}
	if err := validateCreate(request, (&StudioService{}).referenceLimit()); err != nil {
		t.Fatalf("two mappings within the default: %v", err)
	}
	err := validateCreate(request, (&StudioService{MaxReferences: 1}).referenceLimit())
	if fields := fieldsOf(t, err); !slices.Equal(fields, []string{"references"}) {
		t.Fatalf("fields = %v", fields)
	}
}

func TestPortableExtensionLimit(t *testing.T) {
	raw := json.RawMessage(`{"a": 1}`)
	if compact, err := compactExtension(raw, (&StudioService{}).extensionLimit()); err != nil || string(compact) != `{"a":1}` {
		t.Fatalf("compact = %s, err = %v", compact, err)
	}
	if _, err := compactExtension(raw, (&StudioService{MaxPortableExtension: 4}).extensionLimit()); err == nil || !strings.Contains(err.Error(), "4 bytes") {
		t.Fatalf("an extension above the configured limit must be refused, got %v", err)
	}
}

func TestTemplateAudienceLimit(t *testing.T) {
	h := newTemplateHarness(t)
	saved, err := h.save("key-1", "Formal writer")
	if err != nil {
		t.Fatal(err)
	}
	h.shareable["template_audience:tenants"] = true
	h.studio.MaxTemplateAudience = 1
	publication := domain.AgentTemplatePublication{TemplateID: saved.Latest.TemplateID, Audience: domain.TemplateAudience{Tenants: []int64{9, 10}}}
	if _, err = h.templates.PublishTemplate(context.Background(), humanActor(8, 9), publication); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an audience above the configured limit must be refused, got %v", err)
	}
	publication.Audience.Tenants = []int64{9}
	if _, err = h.templates.PublishTemplate(context.Background(), humanActor(8, 9), publication); err != nil {
		t.Fatalf("an audience within the configured limit: %v", err)
	}
}

func TestDraftCommentListLimit(t *testing.T) {
	comments, query := commentService()
	if _, err := comments.Comments(context.Background(), humanActor(8, 9), "writer-a", ""); err != nil {
		t.Fatal(err)
	}
	if args := query.args[qCommentList]; args[len(args)-1] != DefaultMaxCommentsShown {
		t.Fatalf("list args = %v, want the default limit last", args)
	}
	comments.Studio.MaxCommentsShown = 3
	if _, err := comments.Comments(context.Background(), humanActor(8, 9), "writer-a", domain.DraftCommentOpen); err != nil {
		t.Fatal(err)
	}
	if args := query.args[qCommentListStatus]; args[2] != string(domain.DraftCommentOpen) || args[3] != 3 {
		t.Fatalf("status list args = %v, want the configured limit", args)
	}
}

func TestGovernanceItemLimit(t *testing.T) {
	fixture := newGovernanceFixture(map[string][][]any{})
	fixture.service.MaxItems = 2
	_, err := fixture.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(billingGovernance()))
	if fields := fieldsOf(t, err); !slices.Contains(fields, "triggerChannels") {
		t.Fatalf("fields = %v, want the three trigger channels refused", fields)
	}
	if limit := (&StudioGovernance{}).itemLimit(); limit != DefaultMaxGovernanceItems {
		t.Fatalf("zero limit = %d, want %d", limit, DefaultMaxGovernanceItems)
	}
	negative := newGovernanceFixture(map[string][][]any{})
	negative.service.MaxItems = -1
	if _, err = negative.service.SaveGovernance(context.Background(), humanActor(8, 9), governanceChange(billingGovernance())); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative item cap must be refused, got %v", err)
	}
	if _, err = negative.service.Governance(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative item cap must be refused on read, got %v", err)
	}
}
