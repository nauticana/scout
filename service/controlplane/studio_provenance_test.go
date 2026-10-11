package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

var templateOriginRow = []any{"AgentTemplate", "tpl-00000000000000000001", nil, true, "2", "sha256:" + strings.Repeat("a", 64), int64(8), "tpl-00000000000000000001", int64(2), nil, nil}

func provenanceStudio(rows map[string][][]any) (*StudioService, *studioQueryFake) {
	query := &studioQueryFake{rows: rows, args: map[string][]any{}}
	return &StudioService{Principals: studioPrincipals, Roles: studioRoles, Scopes: studioScopes(nil),
		DB: studioDBFake{qs: query}, Catalog: studioModelCatalogFake{},
		Sources: studioSourcesFake{resolved: domain.ResolvedPrompts{AgentID: "writer-a", AgentTypeID: "writer", BaselineKey: "global",
			LanguageCode: "en-US", Sections: []domain.PromptSectionSource{layered(1, 1, baseLayer("write", ""))}}},
		Assembler: &PromptDraftAssembler{Compiler: &PromptCompiler{}}, Compiler: &PromptCompiler{},
	}, query
}

func provenanceRows() map[string][][]any {
	return map[string][][]any{
		qStudioGetDraft:    {{"writer", "Writer", "active", true, "provider-a", "model-a", nil, nil, nil, nil, nil, int64(4), int64(2), true}},
		qStudioLockDraft:   {{int64(4), "writer"}},
		qStudioLockAlias:   {{"writer-a", int64(2)}},
		qStudioNextVersion: {{int64(1)}},
	}
}

func TestStudioPublishFreezesTheDraftOrigin(t *testing.T) {
	request := domain.AgentPublishRequest{AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2}
	plain, _ := provenanceStudio(provenanceRows())
	baseline, err := plain.Publish(context.Background(), humanActor(8, 9), request)
	if err != nil {
		t.Fatal(err)
	}
	rows := provenanceRows()
	rows[qSeedDraftOrigin] = [][]any{templateOriginRow}
	derived, query := provenanceStudio(rows)
	release, err := derived.Publish(context.Background(), humanActor(8, 9), request)
	if err != nil {
		t.Fatal(err)
	}
	var stored domain.AgentDefinition
	if err = json.Unmarshal([]byte(query.args[qStudioInsertVersion][3].(string)), &stored); err != nil {
		t.Fatal(err)
	}
	want := domain.DerivedFrom{Source: domain.CharterObjectRef{Kind: "AgentTemplate", ID: "tpl-00000000000000000001", External: true}, Version: "2", Digest: "sha256:" + strings.Repeat("a", 64)}
	if stored.DerivedFrom == nil || *stored.DerivedFrom != want {
		t.Fatalf("derived_from = %+v", stored.DerivedFrom)
	}
	if release.DefinitionDigest == baseline.DefinitionDigest {
		t.Fatal("the definition digest covers the frozen provenance")
	}
	if digest, err := (&PromptCompiler{}).DefinitionDigest(stored); err != nil || digest != stored.DefinitionDigest {
		t.Fatalf("the stored definition verifies: %v", err)
	}
}

// A definition without provenance keeps the digest it had before provenance existed.
func TestDefinitionDigestWithoutProvenanceIsUnchanged(t *testing.T) {
	compiler := &PromptCompiler{}
	compiled, err := compiler.Compile("en-US", []domain.PromptSectionSource{layered(1, 1, baseLayer("write", ""))})
	if err != nil {
		t.Fatal(err)
	}
	definition := domain.AgentDefinition{AgentID: "writer-a", AgentTypeID: "writer", Enabled: true,
		Models: domain.AgentModelSelection{Text: modelReference("provider-a", "model-a")}, Languages: []domain.CompiledPrompt{compiled},
		Tools: []domain.ToolReference{{ToolID: "search", Version: "1"}}, SettingsDigest: strings.Repeat("c", 64),
		Sections: map[string]json.RawMessage{"governance": json.RawMessage(`{"purpose":"write"}`)}}
	digest, err := compiler.DefinitionDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	if digest != pinnedDefinitionDigest {
		t.Fatalf("digest = %s", digest)
	}
	definition.DerivedFrom = &domain.DerivedFrom{Source: domain.CharterObjectRef{Kind: "AgentDefinition", ID: "x-v1"}, Version: "1", Digest: "sha256:" + strings.Repeat("b", 64)}
	if derived, _ := compiler.DefinitionDigest(definition); derived == digest {
		t.Fatal("provenance must change the digest")
	}
}

func TestStudioDriftReportsAChangedOrigin(t *testing.T) {
	definition := domain.AgentDefinition{AgentID: "writer-a", AgentTypeID: "writer", Version: "1", Enabled: true,
		Models: domain.AgentModelSelection{Text: modelReference("provider-a", "model-a")}}
	encoded, _ := json.Marshal(definition)
	rows := provenanceRows()
	rows[qStudioActiveDefinition] = [][]any{{"1", string(encoded)}}
	rows[qSeedDraftOrigin] = [][]any{templateOriginRow}
	studio, _ := provenanceStudio(rows)
	draft, err := studio.GetDraft(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	if draft.Drift == nil || !strings.Contains(strings.Join(draft.Drift.Causes, ","), "derived_from") {
		t.Fatalf("drift = %+v", draft.Drift)
	}
}

func TestStudioRestoreKeepsTheVersionsProvenance(t *testing.T) {
	derived := &domain.DerivedFrom{Source: domain.CharterObjectRef{Kind: "AgentDefinition", ID: "writer-a-v3", Namespace: "acme.test", External: true}, Version: "3", Digest: "sha256:" + strings.Repeat("d", 64)}
	definition := domain.AgentDefinition{AgentID: "writer-a", AgentTypeID: "writer", Version: "1", Enabled: true, DerivedFrom: derived}
	encoded, _ := json.Marshal(definition)
	rows := provenanceRows()
	rows[qStudioGetVersion] = [][]any{{string(encoded), false}}
	rows[qStudioGetSettings] = [][]any{{"a:writer-a", `[]`, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), nil}}
	rows[qStudioNextVersion] = [][]any{{int64(2)}}
	studio, query := provenanceStudio(rows)
	if _, err := studio.Restore(context.Background(), humanActor(8, 9), domain.AgentRestoreRequest{AgentID: "writer-a", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	var stored domain.AgentDefinition
	if err := json.Unmarshal([]byte(query.args[qStudioInsertVersion][3].(string)), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.DerivedFrom == nil || *stored.DerivedFrom != *derived {
		t.Fatalf("restored provenance = %+v", stored.DerivedFrom)
	}
}

// Every request field but the idempotency key changes the create digest, whatever the create kind.
func TestCreateDigestCoversEveryRequestField(t *testing.T) {
	base := domain.AgentCreateRequest{AgentTypeID: "writer", DisplayName: "Writer", IdempotencyKey: "k"}
	sum := sha256.Sum256([]byte("writer\x1fWriter\x1f\x1f\x1f0\x1f"))
	if createDigest(base) != hex.EncodeToString(sum[:]) {
		t.Fatal("create digest layout changed")
	}
	rekeyed := base
	rekeyed.IdempotencyKey = "other"
	if createDigest(rekeyed) != createDigest(base) {
		t.Fatal("the idempotency key is not part of the request digest")
	}
	variants := map[string]func(*domain.AgentCreateRequest){
		"type":     func(r *domain.AgentCreateRequest) { r.AgentTypeID = "reviewer" },
		"name":     func(r *domain.AgentCreateRequest) { r.DisplayName = "Other" },
		"copy":     func(r *domain.AgentCreateRequest) { r.CopyFrom = "writer-a" },
		"template": func(r *domain.AgentCreateRequest) { r.TemplateID = "tpl-00000000000000000001" },
		"version":  func(r *domain.AgentCreateRequest) { r.TemplateVersion = 2 },
		"references": func(r *domain.AgentCreateRequest) {
			r.References = map[domain.ResourceKind]map[string]string{domain.ResourceKnowledge: {"docs": "kb-9"}}
		},
	}
	seen := map[string]string{createDigest(base): "base"}
	for name, mutate := range variants {
		request := base
		mutate(&request)
		digest := createDigest(request)
		if prior, ok := seen[digest]; ok {
			t.Fatalf("%s digests the same as %s", name, prior)
		}
		seen[digest] = name
	}
}

// pinnedDefinitionDigest is what this definition digested to before provenance existed.
const pinnedDefinitionDigest = "99df401677d30e5b5af87bf647922e27c2a7fd9334526947b276879e31b63f7c"
