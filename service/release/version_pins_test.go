package release

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var writerRequest = domain.VersionRequest{TenantID: 8, AgentID: "writer-a", ConversationID: "conversation-a"}

func pinnedManager(pins []domain.VersionPin, cohort *domain.ExperimentCohort) (*PinnedTrafficManager, *fake.RecordingAuditSink) {
	audit := &fake.RecordingAuditSink{}
	manager := &PinnedTrafficManager{
		Pins:  &fake.VersionPinStore{Pins: pins},
		Audit: audit,
		Deployments: &fake.AgentDeploymentStore{Deployments: map[string]domain.AgentDeployment{
			"8|writer-a": {TenantID: 8, AgentID: "writer-a", StableVersion: "3", CanaryVersion: "4", CanaryPercentage: 100},
		}},
		Now: func() time.Time { return testStart },
	}
	if cohort != nil {
		manager.Cohorts = fake.ExperimentCohortResolverFunc(func(context.Context, int64, string, string) (domain.ExperimentCohort, bool, error) {
			return *cohort, true, nil
		})
	}
	return manager, audit
}

func testPin(scope domain.PinScope, version, approver string) domain.VersionPin {
	return domain.VersionPin{
		TenantID: 8, AgentID: "writer-a", Version: version, Scope: scope, Reason: "audit hold",
		Owner: "compliance", ApprovedBy: approver, EffectiveAt: testStart.Add(-time.Hour), ExpiresAt: testStart.Add(time.Hour),
	}
}

func TestPinnedTrafficManagerPrecedence(t *testing.T) {
	cohort := domain.ExperimentCohort{TenantID: 8, AgentID: "writer-a", ExperimentID: "tone", Version: "5", Percentage: 100}
	cases := []struct {
		name    string
		pins    []domain.VersionPin
		cohort  *domain.ExperimentCohort
		version string
		source  domain.VersionSource
	}{
		{"compliance wins", []domain.VersionPin{testPin(domain.PinScopeTenant, "2", "lead"), testPin(domain.PinScopeCompliance, "1", "dpo")}, &cohort, "1", domain.VersionFromCompliancePin},
		{"approved tenant pin", []domain.VersionPin{testPin(domain.PinScopeTenant, "2", "lead")}, &cohort, "2", domain.VersionFromTenantPin},
		{"unapproved tenant pin ignored", []domain.VersionPin{testPin(domain.PinScopeTenant, "2", "")}, &cohort, "5", domain.VersionFromCohort},
		{"expired pin ignored", []domain.VersionPin{func() domain.VersionPin {
			pin := testPin(domain.PinScopeTenant, "2", "lead")
			pin.ExpiresAt = testStart.Add(-time.Minute)
			return pin
		}()}, nil, "4", domain.VersionFromCanary},
		{"assignment pin for another assignment ignored", []domain.VersionPin{assignmentPin("ASGN-2")}, &cohort, "5", domain.VersionFromCohort},
		{"cohort before default", nil, &cohort, "5", domain.VersionFromCohort},
		{"deployment default", nil, nil, "4", domain.VersionFromCanary},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			manager, audit := pinnedManager(testCase.pins, testCase.cohort)
			resolution, err := manager.ExplainVersion(context.Background(), writerRequest)
			if err != nil {
				t.Fatal(err)
			}
			if resolution.Version != testCase.version || resolution.Source != testCase.source {
				t.Fatalf("resolution = %+v", resolution)
			}
			if got := audit.Categories(); len(got) != 1 || got[0] != "agent_version.resolved" {
				t.Fatalf("audit = %v", got)
			}
		})
	}
}

func assignmentPin(assignment string) domain.VersionPin {
	pin := testPin(domain.PinScopeAssignment, "3", "lead")
	pin.AssignmentRef = assignment
	return pin
}

// An assignment pin serves only the conversations naming its assignment, after the tenant pin and before a pilot.
func TestPinnedTrafficManagerAssignmentPinPrecedence(t *testing.T) {
	cohort := domain.ExperimentCohort{TenantID: 8, AgentID: "writer-a", ExperimentID: "tone", Version: "5", Percentage: 100}
	assigned := writerRequest
	assigned.AssignmentRef, assigned.Subject = "ASGN-1", pilotPerson
	cases := []struct {
		name    string
		pins    []domain.VersionPin
		request domain.VersionRequest
		version string
		source  domain.VersionSource
	}{
		{"assignment pin beats the pilot", []domain.VersionPin{assignmentPin("ASGN-1")}, assigned, "3", domain.VersionFromAssignmentPin},
		{"tenant pin beats the assignment pin", []domain.VersionPin{assignmentPin("ASGN-1"), testPin(domain.PinScopeTenant, "2", "lead")}, assigned, "2", domain.VersionFromTenantPin},
		{"a conversation naming no assignment takes none", []domain.VersionPin{assignmentPin("ASGN-1")}, writerRequest, "5", domain.VersionFromCohort},
		{"an expired assignment pin falls through", []domain.VersionPin{func() domain.VersionPin {
			pin := assignmentPin("ASGN-1")
			pin.ExpiresAt = testStart.Add(-time.Minute)
			return pin
		}()}, assigned, "6", domain.VersionFromPilot},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			manager, _ := pinnedManager(testCase.pins, &cohort)
			manager.Pilots = fake.PilotResolverFunc(func(context.Context, int64, string, domain.PrincipalRef, time.Time) (domain.PilotMember, bool, error) {
				return domain.PilotMember{Version: "6", Subject: domain.PilotSubject{Kind: domain.PilotPrincipal, ID: pilotPerson.ID}}, true, nil
			})
			resolution, err := manager.ExplainVersion(context.Background(), testCase.request)
			if err != nil || resolution.Version != testCase.version || resolution.Source != testCase.source {
				t.Fatalf("resolution = %+v, err = %v", resolution, err)
			}
		})
	}
}

func TestPinnedTrafficManagerRejectsIncompatiblePin(t *testing.T) {
	pin := testPin(domain.PinScopeCompliance, "1", "dpo")
	pin.CompatiblePolicyVersions = []string{"safety-2026.05"}
	manager, _ := pinnedManager([]domain.VersionPin{pin}, nil)
	manager.PolicyVersion = "safety-2026.08"
	if _, err := manager.ResolveVersion(context.Background(), writerRequest); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestPinnedTrafficManagerRegionScopedPin(t *testing.T) {
	pin := testPin(domain.PinScopeTenant, "2", "lead")
	pin.Region = "eu-central"
	manager, _ := pinnedManager([]domain.VersionPin{pin}, nil)
	manager.Region = "us-east"
	resolution, err := manager.ExplainVersion(context.Background(), writerRequest)
	if err != nil || resolution.Source != domain.VersionFromCanary {
		t.Fatalf("resolution = %+v, err = %v", resolution, err)
	}
	manager.Region = "eu-central"
	if resolution, err = manager.ExplainVersion(context.Background(), writerRequest); err != nil || resolution.Version != "2" {
		t.Fatalf("regional resolution = %+v, err = %v", resolution, err)
	}
}

func TestPinnedTrafficManagerRollbackChangesOnlyNewAssignments(t *testing.T) {
	manager, audit := pinnedManager(nil, nil)
	if err := manager.Rollback(context.Background(), 8, "writer-a"); err != nil {
		t.Fatal(err)
	}
	resolution, err := manager.ExplainVersion(context.Background(), writerRequest)
	if err != nil || resolution.Version != "3" || resolution.Source != domain.VersionFromStable {
		t.Fatalf("resolution = %+v, err = %v", resolution, err)
	}
	if got := audit.Categories(); got[0] != "agent_version.rolled_back" {
		t.Fatalf("audit = %v", got)
	}
}

func TestTableVersionPinStoreArguments(t *testing.T) {
	query := newQueryFake(map[string][][]any{qPinInsert: {{int64(12)}}})
	store := &TableVersionPinStore{DB: dbFake{query: query}}
	pin := testPin(domain.PinScopeCompliance, "1", "dpo")
	pin.Signature = "sig"
	pin.CompatiblePolicyVersions = []string{"safety-2026.08"}
	id, err := store.Put(context.Background(), pin)
	if err != nil || id != 12 {
		t.Fatalf("id = %d, err = %v", id, err)
	}
	args := query.args[qPinInsert]
	if len(args) != 15 || args[0] != int64(8) || args[1] != "writer-a" || args[2] != "1" || args[3] != string(domain.PinScopeCompliance) ||
		args[4] != nil || args[7] != "dpo" || args[9] != `["safety-2026.08"]` || args[10] != nil || args[14] != nil {
		t.Fatalf("args = %v", args)
	}
}

func TestTableVersionPinStoreAssignmentPins(t *testing.T) {
	store := &TableVersionPinStore{DB: dbFake{query: newQueryFake(map[string][][]any{qPinInsert: {{int64(13)}}})}}
	pin := testPin(domain.PinScopeAssignment, "3", "lead")
	pin.AssignmentRef = "ASGN-1"
	if id, err := store.Put(context.Background(), pin); err != nil || id != 13 {
		t.Fatalf("id = %d, err = %v", id, err)
	}
	for name, mutate := range map[string]func(*domain.VersionPin){
		"no assignment":   func(pin *domain.VersionPin) { pin.AssignmentRef = "" },
		"tenant with one": func(pin *domain.VersionPin) { pin.Scope = domain.PinScopeTenant },
		"over 200 bytes":  func(pin *domain.VersionPin) { pin.AssignmentRef = strings.Repeat("a", 201) },
	} {
		changed := pin
		mutate(&changed)
		if _, err := store.Put(context.Background(), changed); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	pin.ApprovedBy = ""
	if _, err := store.Put(context.Background(), pin); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unapproved assignment pin: err = %v", err)
	}
}

func TestTableVersionPinStoreRejectsUnsignedCompliancePin(t *testing.T) {
	store := &TableVersionPinStore{DB: dbFake{query: newQueryFake(nil)}}
	if _, err := store.Put(context.Background(), testPin(domain.PinScopeCompliance, "1", "dpo")); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
}

func TestPinAwareGarbageCollectorKeepsReferencedVersions(t *testing.T) {
	for _, retained := range []bool{true, false} {
		query := newQueryFake(map[string][][]any{qVersionRetained: {{retained}}})
		collector := &PinAwareGarbageCollector{DB: dbFake{query: query}, Now: func() time.Time { return testStart }}
		collectable, err := collector.Collectable(context.Background(), 8, "writer-a", "3")
		if err != nil {
			t.Fatal(err)
		}
		if collectable == retained {
			t.Fatalf("retained %t returned collectable %t", retained, collectable)
		}
		requireArgs(t, query, qVersionRetained,
			int64(8), "writer-a", "3", "3",
			int64(8), "writer-a", "3", testStart,
			int64(8), "writer-a", "3",
			int64(8), "writer-a", "3", testStart,
			int64(8), "writer-a", "3")
	}
}

func TestTableAgentDeploymentStoreRestorePrevious(t *testing.T) {
	query := newQueryFake(map[string][][]any{
		qDeploymentGet:      {{"3", nil, int64(0)}},
		qDeploymentPrevious: {{"2"}},
		qDeploymentRestore:  {{"2"}},
	})
	store := &TableAgentDeploymentStore{DB: dbFake{query: query}}
	version, err := store.RestorePrevious(context.Background(), 8, "writer-a")
	if err != nil || version != "2" {
		t.Fatalf("version = %q, err = %v", version, err)
	}
	requireArgs(t, query, qDeploymentRestore, "2", int64(8), "writer-a", "3")
}

func TestTableAgentDeploymentStoreGuardsReviewedTrafficChanges(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(map[string][][]any{
		qDeploymentReweight: {{"3"}},
		qDeploymentPromoted: {{"4"}},
		qDeploymentClear:    {{"4"}},
	})
	store := &TableAgentDeploymentStore{DB: dbFake{query: query}}

	if err := store.ReweightCanary(ctx, 8, "writer-a", "4", 25); err != nil {
		t.Fatal(err)
	}
	requireArgs(t, query, qDeploymentReweight, 25, int64(8), "writer-a", "4")
	if err := store.PromoteDeployed(ctx, 8, "writer-a", "4"); err != nil {
		t.Fatal(err)
	}
	requireArgs(t, query, qDeploymentPromoted, "4", int64(8), "writer-a", "4", "4")
	if version, err := store.EndCanary(ctx, 8, "writer-a"); err != nil || version != "4" {
		t.Fatalf("ended canary at %q: %v", version, err)
	}

	query.rows = nil
	if err := store.ReweightCanary(ctx, 8, "writer-a", "2", 25); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("reweight stale canary = %v, want conflict", err)
	}
	if err := store.PromoteDeployed(ctx, 8, "writer-a", "2"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("promote stale release = %v, want conflict", err)
	}
	if _, err := store.EndCanary(ctx, 8, "writer-a"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("end missing canary = %v, want conflict", err)
	}
}

func TestCohortSelectionIsStable(t *testing.T) {
	cohort := domain.ExperimentCohort{AgentID: "writer-a", ExperimentID: "tone", Percentage: 50, Salt: "s1"}
	first := CohortSelected(cohort, 8, "conversation-a")
	for range 10 {
		if CohortSelected(cohort, 8, "conversation-a") != first {
			t.Fatal("cohort membership changed for one subject")
		}
	}
	cohort.Salt = "s2"
	changed := 0
	for index := range 200 {
		subject := "conversation-" + string(rune('a'+index%26)) + string(rune('a'+index/26))
		if CohortSelected(cohort, 8, subject) {
			changed++
		}
	}
	if changed == 0 || changed == 200 {
		t.Fatalf("cohort selected %d of 200 subjects", changed)
	}
}
