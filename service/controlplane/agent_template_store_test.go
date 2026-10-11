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
)

var storedPayload = domain.TemplatePayload{
	Bindings: []domain.PortableBinding{{ResourceKind: domain.ResourceKnowledge, ResourceID: "knowledge", MergeMode: domain.MergeReplace, Value: json.RawMessage(`["docs"]`)}},
	Slots:    map[domain.ResourceKind][]string{domain.ResourceKnowledge: {"docs"}},
}

func payloadRow(t *testing.T, payload domain.TemplatePayload) (string, string) {
	t.Helper()
	encoded, err := canonicalTemplatePayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded), valueDigest(encoded)
}

// templateRow is one tenant template read: the shared columns, the owner, and the owner-only source.
func templateRow(t *testing.T, owner int64, id string, version int64, source any) []any {
	t.Helper()
	payload, digest := payloadRow(t, storedPayload)
	var revision any
	if source != nil {
		revision = int64(4)
	}
	return []any{id, version, "writer", "1", typeDigest, "Formal writer", nil, nil, payload, digest, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), owner, source, revision}
}

func newTemplateStore(db *seedDB) *TableAgentTemplateStore {
	return &TableAgentTemplateStore{DB: db.repository(), Entropy: bytes.NewReader(make([]byte, 32))}
}

func templateVersion(t *testing.T) domain.AgentTemplateVersion {
	t.Helper()
	_, digest := payloadRow(t, storedPayload)
	return domain.AgentTemplateVersion{AgentTypeID: "writer", TypeVersion: "1", DisplayName: "Formal writer", Payload: storedPayload,
		Digest: digest, SourceAgentID: "writer-a", SourceDraftRevision: 4}
}

func TestTemplateStoreSavesANewTemplateUnderTheTypeLock(t *testing.T) {
	const id = "tpl-00000000000000000000"
	db := newSeedDB(map[string][][]any{
		qTemplateLockType:   {{"writer"}},
		qTemplateCount:      {{int64(0)}},
		qTemplateInsert:     {{id}},
		qTemplateOwnVersion: {templateRow(t, 8, id, 1, "writer-a")},
		qTemplateAudience:   {{nil}, {int64(9)}},
	})
	store := newTemplateStore(db)
	saved, err := store.Save(context.Background(), humanActor(8, 9), templateVersion(t), "key-1", domain.TemplateLimits{MaxPerType: 2, MaxVersions: 2})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{qTemplateLockType, qTemplateByKey, qTemplateCount, qTemplateInsert, qTemplateNewest, qTemplateInsertVersion, qStudioAudit, qTemplateOwnVersion, qTemplateAudience}
	if !slices.Equal(db.names(), want) || db.commits != 1 {
		t.Fatalf("queries = %v, commits %d", db.names(), db.commits)
	}
	inserted := db.last(qTemplateInsertVersion)
	payload, digest := payloadRow(t, storedPayload)
	if inserted[0] != int64(8) || inserted[1] != id || inserted[2] != int64(1) || inserted[7] != payload || inserted[8] != digest ||
		inserted[10] != "writer-a" || inserted[11] != int64(4) || inserted[12] != "human" || inserted[13] != "9" {
		t.Fatalf("version insert = %v", inserted)
	}
	if audit := db.last(qStudioAudit); audit[1] != "writer-a" || audit[2] != "TEMPLATE" || audit[3] != "template "+id+" version 1" {
		t.Fatalf("audit = %v", audit)
	}
	if saved.Latest.Owner != domain.TemplateOwn || saved.Latest.SourceAgentID != "writer-a" || !saved.Audience.All || !slices.Equal(saved.Audience.Tenants, []int64{9}) {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestTemplateStoreSaveReplaysLimitsAndOwnership(t *testing.T) {
	const id = "tpl-00000000000000000001"
	_, digest := payloadRow(t, storedPayload)
	rows := func() map[string][][]any {
		return map[string][][]any{
			qTemplateLockType:   {{"writer"}},
			qTemplateCount:      {{int64(0)}},
			qTemplateInsert:     {{id}},
			qTemplateOwnVersion: {templateRow(t, 8, id, 1, "writer-a")},
		}
	}
	limits := domain.TemplateLimits{MaxPerType: 2, MaxVersions: 2}

	db := newSeedDB(rows())
	db.rows[qTemplateByKey] = [][]any{{id, "writer"}}
	db.rows[qTemplateFirstVersion] = [][]any{{digest, "Formal writer", nil, "1"}}
	if _, err := newTemplateStore(db).Save(context.Background(), humanActor(8, 9), templateVersion(t), "key-1", limits); err != nil || db.count(qTemplateInsertVersion) != 0 || db.count(qTemplateInsert) != 0 {
		t.Fatalf("a key replay inserts nothing: %v %v", err, db.names())
	}
	db = newSeedDB(rows())
	db.rows[qTemplateByKey] = [][]any{{id, "writer"}}
	db.rows[qTemplateFirstVersion] = [][]any{{strings.Repeat("0", 64), "Formal writer", nil, "1"}}
	if _, err := newTemplateStore(db).Save(context.Background(), humanActor(8, 9), templateVersion(t), "key-1", limits); !errors.Is(err, domain.ErrConflict) || db.commits != 0 {
		t.Fatalf("the same key with other content conflicts: %v", err)
	}
	db = newSeedDB(rows())
	db.rows[qTemplateCount] = [][]any{{int64(2)}}
	if _, err := newTemplateStore(db).Save(context.Background(), humanActor(8, 9), templateVersion(t), "key-2", limits); !errors.Is(err, domain.ErrConflict) || db.count(qTemplateInsert) != 0 {
		t.Fatalf("the per-type cap refuses: %v", err)
	}

	existing := templateVersion(t)
	existing.TemplateID = id
	db = newSeedDB(rows())
	db.rows[qTemplateLock] = [][]any{{"writer"}}
	db.rows[qTemplateNewest] = [][]any{{int64(1), digest, "Formal writer", nil, "1"}}
	if saved, err := newTemplateStore(db).Save(context.Background(), humanActor(8, 9), existing, "", limits); err != nil || saved.Latest.Version != 1 || db.count(qTemplateInsertVersion) != 0 {
		t.Fatalf("equal content inserts no version: %v %v", err, db.names())
	}
	repinned := existing
	repinned.TypeVersion = "2"
	db = newSeedDB(rows())
	db.rows[qTemplateLock] = [][]any{{"writer"}}
	db.rows[qTemplateNewest] = [][]any{{int64(1), digest, "Formal writer", nil, "1"}}
	if _, err := newTemplateStore(db).Save(context.Background(), humanActor(8, 9), repinned, "", limits); err != nil || db.count(qTemplateInsertVersion) != 1 {
		t.Fatalf("another type version is a new template version: %v %v", err, db.names())
	}
	if inserted := db.last(qTemplateInsertVersion); inserted[2] != int64(2) || inserted[6] != "2" {
		t.Fatalf("version 2 pins type version 2: %v", inserted)
	}
	db = newSeedDB(rows())
	db.rows[qTemplateByKey] = [][]any{{id, "writer"}}
	db.rows[qTemplateFirstVersion] = [][]any{{digest, "Formal writer", nil, "1"}}
	keyed := templateVersion(t)
	keyed.TypeVersion = "2"
	if _, err := newTemplateStore(db).Save(context.Background(), humanActor(8, 9), keyed, "key-1", limits); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("the same key with another type version conflicts: %v", err)
	}
	existing.Description = "new"
	db = newSeedDB(rows())
	db.rows[qTemplateLock] = [][]any{{"writer"}}
	db.rows[qTemplateNewest] = [][]any{{int64(2), digest, "Formal writer", nil, "1"}}
	if _, err := newTemplateStore(db).Save(context.Background(), humanActor(8, 9), existing, "", limits); !errors.Is(err, domain.ErrConflict) || db.count(qTemplateInsertVersion) != 0 {
		t.Fatalf("the version cap refuses: %v", err)
	}
	db = newSeedDB(rows())
	db.rows[qTemplateLock] = [][]any{{"editor"}}
	if _, err := newTemplateStore(db).Save(context.Background(), humanActor(8, 9), existing, "", limits); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a template of another type is not found: %v", err)
	}
	db = newSeedDB(rows())
	if _, err := newTemplateStore(db).Save(context.Background(), humanActor(7, 9), existing, "", limits); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant's template is not found: %v", err)
	}
}

func TestTemplateStoreVersionAppliesTheVisibilityPredicate(t *testing.T) {
	const id = "tpl-00000000000000000001"
	db := newSeedDB(map[string][][]any{qTemplateUsableVersion: {templateRow(t, 8, id, 2, nil)}})
	store := newTemplateStore(db)
	version, err := store.Version(context.Background(), 9, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if args := db.last(qTemplateUsableVersion); args[0] != id || args[1] != int64(0) || args[3] != int64(9) || args[4] != int64(9) {
		t.Fatalf("visibility args = %v", args)
	}
	if version.Owner != domain.TemplateShared || version.SourceAgentID != "" || version.OwnerTenantID != 8 || version.TypeDigest != typeDigest || len(version.Payload.Bindings) != 1 {
		t.Fatalf("version = %+v", version)
	}
	db.rows[qTemplateUsableVersion] = nil
	if _, err = store.Version(context.Background(), 9, id, 0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an invisible template is not found, got %v", err)
	}
	platform := templateRow(t, 0, "writer.formal", 1, nil)
	db.rows[qTemplatePlatformVersion] = [][]any{platform}
	if version, err = store.Version(context.Background(), 9, "writer.formal", 1); err != nil || version.Owner != domain.TemplatePlatform || db.count(qTemplateUsableVersion) != 2 {
		t.Fatalf("a platform id reads the catalog: %+v %v", version, err)
	}
	tampered := templateRow(t, 8, id, 2, nil)
	tampered[8] = `{"bindings":[]}`
	db.rows[qTemplateUsableVersion] = [][]any{tampered}
	if _, err = store.Version(context.Background(), 9, id, 0); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a payload that does not match its digest is refused, got %v", err)
	}
}

func TestTemplateStoreUsableListsEachClass(t *testing.T) {
	db := newSeedDB(map[string][][]any{
		qTemplateOwnList:      {templateRow(t, 8, "tpl-00000000000000000001", 2, "writer-a")},
		qTemplateOwnAudiences: {{"tpl-00000000000000000001", int64(9)}},
		qTemplateSharedList:   {templateRow(t, 7, "tpl-00000000000000000002", 1, nil)},
		qTemplatePlatformList: {templateRow(t, 0, "writer.formal", 3, nil)},
	})
	templates, err := newTemplateStore(db).Usable(context.Background(), 8, "writer")
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 3 || templates[0].Latest.Owner != domain.TemplateOwn || templates[0].Audience.Tenants[0] != 9 ||
		templates[1].Latest.Owner != domain.TemplateShared || templates[1].Audience != nil || templates[1].Latest.SourceAgentID != "" ||
		templates[2].Latest.Owner != domain.TemplatePlatform {
		t.Fatalf("templates = %+v", templates)
	}
	if shared := db.last(qTemplateSharedList); shared[0] != "writer" || shared[1] != int64(8) || shared[2] != int64(8) || shared[3] != DefaultUsableTemplateLimit {
		t.Fatalf("shared args = %v", shared)
	}
}

func TestTemplateStoreUsableLimit(t *testing.T) {
	db := newSeedDB(map[string][][]any{})
	store := newTemplateStore(db)
	store.UsableLimit = 7
	if _, err := store.Usable(context.Background(), 8, "writer"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{qTemplateOwnList, qTemplateSharedList, qTemplatePlatformList} {
		if args := db.last(name); args[len(args)-1] != 7 {
			t.Fatalf("%s args = %v, want the configured limit last", name, args)
		}
	}
	negative := newTemplateStore(newSeedDB(map[string][][]any{}))
	negative.UsableLimit = -1
	if _, err := negative.Usable(context.Background(), 8, "writer"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative usable limit must be refused, got %v", err)
	}
}

func TestTemplateStorePublishChecksTheAudience(t *testing.T) {
	const id = "tpl-00000000000000000001"
	rows := map[string][][]any{
		qTemplateLock:         {{"writer"}},
		qTemplateTenantsExist: {{int64(9)}},
		qTemplateOwnVersion:   {templateRow(t, 8, id, 1, "writer-a")},
	}
	db := newSeedDB(rows)
	store := newTemplateStore(db)
	if _, err := store.Publish(context.Background(), humanActor(8, 9), domain.AgentTemplatePublication{TemplateID: id, Audience: domain.TemplateAudience{Tenants: []int64{9, 10}}}); !errors.Is(err, domain.ErrValidation) || db.count(qTemplatePublishTenant) != 0 {
		t.Fatalf("an unknown tenant is refused before anything is written: %v", err)
	}
	if _, err := store.Publish(context.Background(), humanActor(8, 9), domain.AgentTemplatePublication{TemplateID: id, Audience: domain.TemplateAudience{Tenants: []int64{9}}}); err != nil {
		t.Fatal(err)
	}
	if args := db.last(qTemplatePublishTenant); args[0] != int64(8) || args[2] != int64(9) || args[7] != int64(9) || db.last(qTemplateTenantsExist)[0] != "9" {
		t.Fatalf("publish args = %v", args)
	}
	if _, err := store.Publish(context.Background(), humanActor(8, 9), domain.AgentTemplatePublication{TemplateID: id, Audience: domain.TemplateAudience{All: true}, Withdraw: true}); err != nil {
		t.Fatal(err)
	}
	if args := db.last(qTemplateWithdrawAll); args[0] != "human" || args[1] != "9" || args[2] != int64(8) || args[3] != id {
		t.Fatalf("withdraw args = %v", args)
	}
	db.rows[qTemplateLock] = nil
	if _, err := store.Publish(context.Background(), humanActor(7, 9), domain.AgentTemplatePublication{TemplateID: id, Audience: domain.TemplateAudience{All: true}}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant's template is not found, got %v", err)
	}
}

func TestTemplateStoreRegisterPlatformIsImmutable(t *testing.T) {
	_, digest := payloadRow(t, storedPayload)
	version := domain.AgentTemplateVersion{TemplateID: "writer.formal", Version: 1, AgentTypeID: "writer", DisplayName: "Formal writer", Payload: storedPayload}
	db := newSeedDB(map[string][][]any{qTemplateCatalogLock: {{"writer"}}, qTemplateCatalogMax: {{int64(0)}}})
	store := newTemplateStore(db)
	if err := store.RegisterPlatform(context.Background(), version); err != nil || db.count(qTemplateCatalogNewVersion) != 1 || db.last(qTemplateCatalogNewVersion)[7] != digest {
		t.Fatalf("register = %v, %v", err, db.names())
	}
	db.rows[qTemplateCatalogVersion] = [][]any{{digest, "Formal writer", nil, nil, nil, nil}}
	if err := store.RegisterPlatform(context.Background(), version); err != nil || db.count(qTemplateCatalogNewVersion) != 1 {
		t.Fatalf("equal content is a no-op: %v", err)
	}
	db.rows[qTemplateCatalogVersion] = [][]any{{strings.Repeat("0", 64), "Formal writer", nil, nil, nil, nil}}
	if err := store.RegisterPlatform(context.Background(), version); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("other content conflicts, got %v", err)
	}
	db.rows[qTemplateCatalogVersion] = nil
	db.rows[qTemplateCatalogMax] = [][]any{{int64(3)}}
	if err := store.RegisterPlatform(context.Background(), version); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a version that is not next conflicts, got %v", err)
	}
	db.rows[qTemplateCatalogLock] = [][]any{{"editor"}}
	if err := store.RegisterPlatform(context.Background(), version); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a template of another type conflicts, got %v", err)
	}
}
