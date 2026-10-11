package controlplane

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

const createdAgentID = "writer-00000000000000000000"

func lifecycleService(rows map[string][][]any) (*StudioService, *studioQueryFake) {
	query := &studioQueryFake{rows: rows, args: map[string][]any{}}
	return &StudioService{
		Principals: studioPrincipals, Roles: studioRoles, Scopes: studioScopes(nil), MaxAgentsPerType: 5,
		Entropy: bytes.NewReader(make([]byte, 10)),
		DB:      studioDBFake{qs: query}, Sources: unusedSources{}, Assembler: unusedAssembler{}, Compiler: unusedCompiler{},
	}, query
}

func createRows() map[string][][]any {
	return map[string][][]any{
		qStudioLockType:     {{"writer"}},
		qStudioTypeCensus:   {{int64(1), int64(0)}},
		qStudioCreateRecord: {{createdAgentID}},
		qStudioListAgents:   {{createdAgentID, "writer", "Blog writer", "draft", false, int64(1), int64(0), false, nil, nil, nil}},
	}
}

func createRequest() domain.AgentCreateRequest {
	return domain.AgentCreateRequest{AgentTypeID: "writer", DisplayName: "Blog writer", IdempotencyKey: "install-1"}
}

func TestCreateMakesADisabledDraftAgentAndRecordsTheKey(t *testing.T) {
	service, query := lifecycleService(createRows())
	summary, err := service.Create(context.Background(), humanActor(8, 9), createRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if summary.AgentID != createdAgentID || summary.Active || summary.Default {
		t.Fatalf("summary = %+v", summary)
	}
	profile := query.args[qProvisionProfile]
	if profile[1] != createdAgentID || profile[4] != string(domain.AgentStateDraft) {
		t.Fatalf("profile args = %v", profile)
	}
	if record := query.args[qStudioCreateRecord]; record[1] != "human" || record[2] != "9" || record[3] != "install-1" || record[5] != createdAgentID {
		t.Fatalf("idempotency record = %v", record)
	}
	if audit := query.args[qStudioAudit]; audit[2] != "CREATE" || audit[3] != "type defaults" {
		t.Fatalf("audit = %v", audit)
	}
	if slices.Contains(query.queries, qStudioCopyBindings) || query.commits != 1 {
		t.Fatalf("queries = %v, commits = %d", query.queries, query.commits)
	}
}

func TestCreateReplayReturnsTheAgentItCreated(t *testing.T) {
	rows := createRows()
	rows[qStudioCreateReplay] = [][]any{{createDigest(createRequest()), createdAgentID}}
	service, query := lifecycleService(rows)
	summary, err := service.Create(context.Background(), humanActor(8, 9), createRequest())
	if err != nil || summary.AgentID != createdAgentID {
		t.Fatalf("summary = %+v, error = %v", summary, err)
	}
	if slices.Contains(query.queries, qProvisionProfile) {
		t.Fatal("a replay must not create a second agent")
	}
}

func TestCreateRefusesAKeyReusedForAnotherRequest(t *testing.T) {
	rows := createRows()
	rows[qStudioCreateReplay] = [][]any{{createDigest(domain.AgentCreateRequest{AgentTypeID: "writer", DisplayName: "Other"}), createdAgentID}}
	service, _ := lifecycleService(rows)
	if _, err := service.Create(context.Background(), humanActor(8, 9), createRequest()); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}

func TestCreateRefusesBeyondTheCapAndADuplicateName(t *testing.T) {
	rows := createRows()
	rows[qStudioTypeCensus] = [][]any{{int64(5), int64(0)}}
	service, _ := lifecycleService(rows)
	if _, err := service.Create(context.Background(), humanActor(8, 9), createRequest()); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("cap error = %v, want conflict", err)
	}
	rows[qStudioTypeCensus] = [][]any{{int64(1), int64(1)}}
	service, _ = lifecycleService(rows)
	if _, err := service.Create(context.Background(), humanActor(8, 9), createRequest()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("name error = %v, want validation", err)
	}
}

func TestCreateRefusesAnUnknownTypeAndACopyOfAnotherType(t *testing.T) {
	rows := createRows()
	delete(rows, qStudioLockType)
	service, _ := lifecycleService(rows)
	if _, err := service.Create(context.Background(), humanActor(8, 9), createRequest()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown type error = %v", err)
	}
	rows = createRows()
	rows[qStudioGetDraft] = [][]any{{"editor", "Editor", "active", true, nil, nil, nil, nil, nil, nil, nil, int64(4), int64(2), false}}
	service, query := lifecycleService(rows)
	request := createRequest()
	request.CopyFrom = "editor-a"
	if _, err := service.Create(context.Background(), humanActor(8, 9), request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("copy error = %v", err)
	}
	if slices.Contains(query.queries, qProvisionProfile) {
		t.Fatal("a refused copy must not create an agent")
	}
}

func TestCreateChecksTheTypeScopeAndFailsClosed(t *testing.T) {
	service, query := lifecycleService(createRows())
	var values []string
	service.Roles = fake.PrincipalAuthorizerFunc(func(_ context.Context, _ domain.Principal, object, action, value string) (domain.AuthorizationGrant, error) {
		values = append(values, object+"/"+action+"/"+value)
		return domain.AuthorizationGrant{}, nil
	})
	if _, err := service.Create(context.Background(), humanActor(8, 9), createRequest()); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want forbidden", err)
	}
	if len(values) != 1 || values[0] != "AGENT_STUDIO/CREATE/agent_type:writer" || len(query.queries) != 0 {
		t.Fatalf("checked %v, queries %v", values, query.queries)
	}
	service.Roles = nil
	if _, err := service.Create(context.Background(), humanActor(8, 9), createRequest()); err == nil {
		t.Fatal("a service without an authorizer must refuse")
	}
}

func TestCreateByAServicePrincipalLeavesUserColumnsEmpty(t *testing.T) {
	service, query := lifecycleService(createRows())
	installer := domain.StudioActor{TenantID: 8, Principal: domain.PrincipalRef{Kind: domain.PrincipalService, ID: "pack-installer"}}
	if _, err := service.Create(context.Background(), installer, createRequest()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if draft := query.args[qStudioCreateDraft]; draft[len(draft)-1] != nil {
		t.Fatalf("modified_by = %v, want NULL for a service", draft[len(draft)-1])
	}
	if audit := query.args[qStudioAudit]; audit[4] != "service" || audit[5] != "pack-installer" {
		t.Fatalf("audit = %v", audit)
	}
}

func archiveRows(state string, isDefault bool) map[string][][]any {
	return map[string][][]any{
		qStudioGetDraft:   {{"writer", "Writer", state, true, nil, nil, nil, nil, nil, nil, nil, int64(4), int64(2), isDefault}},
		qStudioBumpDraft:  {{int64(5)}},
		qStudioListAgents: {{"writer-a", "writer", "Writer", "archived", true, int64(5), int64(2), false, nil, nil, nil}},
	}
}

func TestArchiveWithdrawsAnAgentAndBlocksEdits(t *testing.T) {
	service, query := lifecycleService(archiveRows("active", false))
	summary, err := service.Archive(context.Background(), humanActor(8, 9), domain.AgentArchiveRequest{AgentID: "writer-a", ExpectedDraftRevision: 4})
	if err != nil || !summary.Archived {
		t.Fatalf("summary = %+v, error = %v", summary, err)
	}
	if state := query.args[qStudioSetProfileActive]; state[0] != string(domain.AgentStateArchived) {
		t.Fatalf("state args = %v", state)
	}
	service, _ = lifecycleService(archiveRows("archived", false))
	if _, err = service.SetEnabled(context.Background(), humanActor(8, 9), domain.AgentSetEnabledRequest{AgentID: "writer-a", Enabled: true, ExpectedDraftRevision: 4}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("editing an archived agent = %v, want conflict", err)
	}
}

func TestArchiveRefusesTheDefaultAndADelegatedAgent(t *testing.T) {
	service, _ := lifecycleService(archiveRows("active", true))
	request := domain.AgentArchiveRequest{AgentID: "writer-a", ExpectedDraftRevision: 4}
	if _, err := service.Archive(context.Background(), humanActor(8, 9), request); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("default error = %v", err)
	}
	service, query := lifecycleService(archiveRows("active", false))
	service.Delegations = &fake.DelegationGrantRepository{ForAgentFunc: func(context.Context, int64, string) ([]domain.DelegationGrant, error) {
		return []domain.DelegationGrant{{GrantID: "g-1"}}, nil
	}}
	if _, err := service.Archive(context.Background(), humanActor(8, 9), request); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("delegation error = %v", err)
	}
	if slices.Contains(query.queries, qStudioSetProfileActive) {
		t.Fatal("a refused archive must not change state")
	}
}

func TestUnarchiveRestoresDisabled(t *testing.T) {
	service, query := lifecycleService(archiveRows("archived", false))
	if _, err := service.Unarchive(context.Background(), humanActor(8, 9), domain.AgentArchiveRequest{AgentID: "writer-a", ExpectedDraftRevision: 4}); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if state := query.args[qStudioSetProfileActive]; state[0] != string(domain.AgentStateSuspended) || !slices.Contains(query.queries, qStudioDisableDraft) {
		t.Fatalf("state = %v, queries = %v", state, query.queries)
	}
	service, _ = lifecycleService(archiveRows("active", false))
	if _, err := service.Unarchive(context.Background(), humanActor(8, 9), domain.AgentArchiveRequest{AgentID: "writer-a", ExpectedDraftRevision: 4}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}

func TestRenameRefusesATakenNameAndAStaleRevision(t *testing.T) {
	rows := archiveRows("active", false)
	rows[qStudioNameTaken] = [][]any{{int64(1)}}
	service, _ := lifecycleService(rows)
	request := domain.AgentRenameRequest{AgentID: "writer-a", DisplayName: "Editor", ExpectedDraftRevision: 4}
	if _, err := service.Rename(context.Background(), humanActor(8, 9), request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("taken error = %v", err)
	}
	rows = archiveRows("active", false)
	delete(rows, qStudioBumpDraft)
	service, query := lifecycleService(rows)
	if _, err := service.Rename(context.Background(), humanActor(8, 9), request); !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("stale error = %v", err)
	}
	if slices.Contains(query.queries, qStudioRename) {
		t.Fatal("a stale rename must not write")
	}
}

func TestListAgentsShowsOnlyGrantedAgents(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{qStudioListAgents: {
		{"writer-a", "writer", "A", "active", true, int64(1), int64(1), true, nil, nil, nil},
		{"writer-b", "writer", "B", "active", true, int64(1), int64(1), false, nil, nil, nil},
	}}, args: map[string][]any{}}
	service := listAgentsService(query)
	service.Roles = fake.PrincipalAuthorizerFunc(func(_ context.Context, _ domain.Principal, _, _, value string) (domain.AuthorizationGrant, error) {
		return domain.AuthorizationGrant{Allowed: value == "agent:writer-b", LowLimit: value}, nil
	})
	summaries, err := service.ListAgents(context.Background(), humanActor(8, 9))
	if err != nil || len(summaries) != 1 || summaries[0].AgentID != "writer-b" {
		t.Fatalf("summaries = %+v, error = %v", summaries, err)
	}
	if _, err = service.GetDraft(context.Background(), humanActor(8, 9), "writer-a"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("direct access = %v, want forbidden", err)
	}
}

func TestArchiveRefusesAnAgentThatBecameTheDefault(t *testing.T) {
	rows := archiveRows("active", false)
	rows[qStudioLockAlias] = [][]any{{"writer-a", int64(3)}}
	service, query := lifecycleService(rows)
	if _, err := service.Archive(context.Background(), humanActor(8, 9), domain.AgentArchiveRequest{AgentID: "writer-a", ExpectedDraftRevision: 4}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	if slices.Contains(query.queries, qStudioSetProfileActive) || query.commits != 0 {
		t.Fatal("a refused archive must not change state")
	}
}
