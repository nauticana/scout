package release

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

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
		{"cohort before default", nil, &cohort, "5", domain.VersionFromCohort},
		{"deployment default", nil, nil, "4", domain.VersionFromCanary},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			manager, audit := pinnedManager(testCase.pins, testCase.cohort)
			resolution, err := manager.ExplainVersion(context.Background(), 8, "writer-a", "conversation-a")
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

func TestPinnedTrafficManagerRejectsIncompatiblePin(t *testing.T) {
	pin := testPin(domain.PinScopeCompliance, "1", "dpo")
	pin.CompatiblePolicyVersions = []string{"safety-2026.05"}
	manager, _ := pinnedManager([]domain.VersionPin{pin}, nil)
	manager.PolicyVersion = "safety-2026.08"
	if _, err := manager.ResolveVersion(context.Background(), 8, "writer-a", "conversation-a"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestPinnedTrafficManagerRegionScopedPin(t *testing.T) {
	pin := testPin(domain.PinScopeTenant, "2", "lead")
	pin.Region = "eu-central"
	manager, _ := pinnedManager([]domain.VersionPin{pin}, nil)
	manager.Region = "us-east"
	resolution, err := manager.ExplainVersion(context.Background(), 8, "writer-a", "conversation-a")
	if err != nil || resolution.Source != domain.VersionFromCanary {
		t.Fatalf("resolution = %+v, err = %v", resolution, err)
	}
	manager.Region = "eu-central"
	if resolution, err = manager.ExplainVersion(context.Background(), 8, "writer-a", "conversation-a"); err != nil || resolution.Version != "2" {
		t.Fatalf("regional resolution = %+v, err = %v", resolution, err)
	}
}

func TestPinnedTrafficManagerRollbackChangesOnlyNewAssignments(t *testing.T) {
	manager, audit := pinnedManager(nil, nil)
	if err := manager.Rollback(context.Background(), 8, "writer-a"); err != nil {
		t.Fatal(err)
	}
	resolution, err := manager.ExplainVersion(context.Background(), 8, "writer-a", "conversation-a")
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
	if len(args) != 14 || args[0] != int64(8) || args[1] != "writer-a" || args[2] != "1" || args[3] != "compliance" ||
		args[4] != nil || args[7] != "dpo" || args[9] != `["safety-2026.08"]` || args[10] != nil {
		t.Fatalf("args = %v", args)
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
