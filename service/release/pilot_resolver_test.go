package release

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var pilotPerson = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}

func pilotRow(kind domain.PilotSubjectKind, id, version string, begda time.Time) []any {
	return []any{string(kind), id, version, "supervised pilot", "human", "7", begda, nil}
}

// positionsOf is the product port for pilotPerson; it counts how often Scout asks.
func positionsOf(held ...string) (fake.PositionResolverFunc, *int) {
	calls := new(int)
	return func(_ context.Context, tenantID int64, person domain.PrincipalRef, at time.Time) ([]string, error) {
		*calls++
		if tenantID != 8 || person != pilotPerson || !at.Equal(testStart) {
			return nil, errors.New("positions asked for the wrong person, tenant, or instant")
		}
		return held, nil
	}, calls
}

func TestTablePilotResolverPrefersThePersonsOwnMembership(t *testing.T) {
	query := newQueryFake(map[string][][]any{qPilotInForce: {
		pilotRow(domain.PilotPrincipal, "42", "6", testStart.Add(-2*time.Hour)),
		pilotRow(domain.PilotPosition, "sales-lead", "5", testStart.Add(-time.Hour)),
	}})
	positions, calls := positionsOf("sales-lead")
	resolver := &TablePilotResolver{DB: dbFake{query: query}, Positions: positions}
	member, selected, err := resolver.Resolve(context.Background(), 8, "writer-a", pilotPerson, testStart)
	if err != nil || !selected || member.Version != "6" || member.Subject != (domain.PilotSubject{Kind: domain.PilotPrincipal, ID: "42"}) {
		t.Fatalf("member = %+v, selected = %t, err = %v", member, selected, err)
	}
	if *calls != 0 {
		t.Fatal("a person's own membership must not consult the product's positions")
	}
	requireArgs(t, query, qPilotInForce, int64(8), "writer-a", testStart, testStart, "42")
}

func TestTablePilotResolverServesTheNewestHeldPosition(t *testing.T) {
	// Rows arrive newest first; a position the person does not hold never applies.
	query := newQueryFake(map[string][][]any{qPilotInForce: {
		pilotRow(domain.PilotPosition, "emea-sales", "7", testStart.Add(-time.Minute)),
		pilotRow(domain.PilotPosition, "sales-lead", "5", testStart.Add(-time.Hour)),
		pilotRow(domain.PilotPosition, "support", "4", testStart.Add(-2*time.Hour)),
	}})
	positions, calls := positionsOf("support", "sales-lead")
	resolver := &TablePilotResolver{DB: dbFake{query: query}, Positions: positions}
	member, selected, err := resolver.Resolve(context.Background(), 8, "writer-a", pilotPerson, testStart)
	if err != nil || !selected || member.Version != "5" || member.Subject.ID != "sales-lead" || *calls != 1 {
		t.Fatalf("member = %+v, selected = %t, calls = %d, err = %v", member, selected, *calls, err)
	}
	unlisted, _ := positionsOf("finance")
	resolver = &TablePilotResolver{DB: dbFake{query: query}, Positions: unlisted}
	if _, selected, err = resolver.Resolve(context.Background(), 8, "writer-a", pilotPerson, testStart); err != nil || selected {
		t.Fatalf("a person holding no listed position was piloted: selected = %t, err = %v", selected, err)
	}
}

func TestTablePilotResolverAsksForPositionsOnlyWhenOneIsListed(t *testing.T) {
	positions, calls := positionsOf("sales-lead")
	resolver := &TablePilotResolver{DB: dbFake{query: newQueryFake(nil)}, Positions: positions}
	if _, selected, err := resolver.Resolve(context.Background(), 8, "writer-a", pilotPerson, testStart); err != nil || selected || *calls != 0 {
		t.Fatalf("selected = %t, calls = %d, err = %v", selected, *calls, err)
	}
}

func TestTablePilotResolverWithoutPositionsIgnoresPositionMemberships(t *testing.T) {
	query := newQueryFake(map[string][][]any{qPilotInForce: {pilotRow(domain.PilotPosition, "sales-lead", "5", testStart)}})
	if _, selected, err := (&TablePilotResolver{DB: dbFake{query: query}}).Resolve(context.Background(), 8, "writer-a", pilotPerson, testStart); err != nil || selected {
		t.Fatalf("selected = %t, err = %v", selected, err)
	}
}

func TestTablePilotResolverFailsWhenPositionsCannotBeResolved(t *testing.T) {
	query := newQueryFake(map[string][][]any{qPilotInForce: {pilotRow(domain.PilotPosition, "sales-lead", "5", testStart)}})
	broken := fake.PositionResolverFunc(func(context.Context, int64, domain.PrincipalRef, time.Time) ([]string, error) {
		return nil, domain.ErrCircuitOpen
	})
	_, selected, err := (&TablePilotResolver{DB: dbFake{query: query}, Positions: broken}).Resolve(context.Background(), 8, "writer-a", pilotPerson, testStart)
	if !errors.Is(err, domain.ErrCircuitOpen) || selected {
		t.Fatalf("selected = %t, err = %v; an unknown position must not be guessed", selected, err)
	}
	query.err[qPilotInForce] = errors.New("connection reset")
	if _, _, err = (&TablePilotResolver{DB: dbFake{query: query}}).Resolve(context.Background(), 8, "writer-a", pilotPerson, testStart); err == nil {
		t.Fatal("a failed membership read must surface")
	}
}

func TestTablePilotResolverBoundsThePositionsPort(t *testing.T) {
	query := newQueryFake(map[string][][]any{qPilotInForce: {pilotRow(domain.PilotPosition, "sales-lead", "5", testStart)}})
	hanging := fake.PositionResolverFunc(func(ctx context.Context, _ int64, _ domain.PrincipalRef, _ time.Time) ([]string, error) {
		<-ctx.Done()
		return []string{"sales-lead"}, nil
	})
	resolver := &TablePilotResolver{DB: dbFake{query: query}, Positions: hanging, PositionTimeout: time.Millisecond}
	_, selected, err := resolver.Resolve(context.WithoutCancel(context.Background()), 8, "writer-a", pilotPerson, testStart)
	if !errors.Is(err, context.DeadlineExceeded) || selected {
		t.Fatalf("selected = %t, err = %v; a port that outlasts its bound must fail the resolution", selected, err)
	}
}

func TestTablePilotResolverDefaultsThePositionTimeout(t *testing.T) {
	resolver := &TablePilotResolver{}
	if got := resolver.positionTimeout(); got != DefaultPilotPositionTimeout {
		t.Fatalf("position timeout = %s, want %s", got, DefaultPilotPositionTimeout)
	}
}

func TestTablePilotResolverServesOnlyAPerson(t *testing.T) {
	query := newQueryFake(map[string][][]any{qPilotInForce: {pilotRow(domain.PilotPrincipal, "42", "6", testStart)}})
	resolver := &TablePilotResolver{DB: dbFake{query: query}}
	for _, subject := range []domain.PrincipalRef{{}, {Kind: domain.PrincipalService, ID: "42"}, {Kind: domain.PrincipalAgent, ID: "42"}, {Kind: domain.PrincipalHuman}} {
		if _, selected, err := resolver.Resolve(context.Background(), 8, "writer-a", subject, testStart); err != nil || selected {
			t.Fatalf("subject %+v: selected = %t, err = %v", subject, selected, err)
		}
	}
	if query.args[qPilotInForce] != nil {
		t.Fatal("a subject that is not a person must not read memberships")
	}
}

func TestPinnedTrafficManagerPilotPrecedence(t *testing.T) {
	cohort := domain.ExperimentCohort{TenantID: 8, AgentID: "writer-a", ExperimentID: "tone", Version: "5", Percentage: 100}
	member := domain.PilotMember{TenantID: 8, AgentID: "writer-a", Version: "6", Subject: domain.PilotSubject{Kind: domain.PilotPosition, ID: "sales-lead"}}
	person := writerRequest
	person.Subject = pilotPerson
	cases := []struct {
		name     string
		pins     []domain.VersionPin
		cohort   *domain.ExperimentCohort
		request  domain.VersionRequest
		unlisted bool
		version  string
		source   domain.VersionSource
	}{
		{"compliance pin beats pilot", []domain.VersionPin{testPin(domain.PinScopeCompliance, "1", "dpo")}, &cohort, person, false, "1", domain.VersionFromCompliancePin},
		{"tenant pin beats pilot", []domain.VersionPin{testPin(domain.PinScopeTenant, "2", "lead")}, &cohort, person, false, "2", domain.VersionFromTenantPin},
		{"pilot beats cohort", nil, &cohort, person, false, "6", domain.VersionFromPilot},
		{"pilot beats canary", nil, nil, person, false, "6", domain.VersionFromPilot},
		{"unlisted person keeps the cohort", nil, &cohort, person, true, "5", domain.VersionFromCohort},
		{"unlisted person keeps the canary", nil, nil, person, true, "4", domain.VersionFromCanary},
		{"no person keeps the cohort", nil, &cohort, writerRequest, false, "5", domain.VersionFromCohort},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			manager, _ := pinnedManager(testCase.pins, testCase.cohort)
			asked := 0
			manager.Pilots = fake.PilotResolverFunc(func(_ context.Context, tenantID int64, agentID string, subject domain.PrincipalRef, at time.Time) (domain.PilotMember, bool, error) {
				asked++
				if tenantID != 8 || agentID != "writer-a" || subject != pilotPerson || !at.Equal(testStart) {
					t.Fatalf("pilot asked for tenant %d, agent %q, subject %+v at %v", tenantID, agentID, subject, at)
				}
				return member, !testCase.unlisted, nil
			})
			resolution, err := manager.ExplainVersion(context.Background(), testCase.request)
			if err != nil || resolution.Version != testCase.version || resolution.Source != testCase.source {
				t.Fatalf("resolution = %+v, err = %v", resolution, err)
			}
			if testCase.request.Subject.ID == "" && asked != 0 {
				t.Fatal("a conversation acting for no one must not consult pilots")
			}
			if wantPilot := testCase.source == domain.VersionFromPilot; wantPilot != (resolution.Pilot == member.Subject) {
				t.Fatalf("pilot subject = %+v for source %s", resolution.Pilot, resolution.Source)
			}
		})
	}
}

func TestPinnedTrafficManagerFailsWhenThePilotCannotBeRead(t *testing.T) {
	manager, audit := pinnedManager(nil, nil)
	manager.Pilots = fake.PilotResolverFunc(func(context.Context, int64, string, domain.PrincipalRef, time.Time) (domain.PilotMember, bool, error) {
		return domain.PilotMember{}, false, domain.ErrCircuitOpen
	})
	request := writerRequest
	request.Subject = pilotPerson
	if _, err := manager.ExplainVersion(context.Background(), request); !errors.Is(err, domain.ErrCircuitOpen) {
		t.Fatalf("err = %v; a pilot that cannot be read must not fall through to the canary", err)
	}
	if len(audit.Events) != 0 {
		t.Fatal("a failed resolution must not be audited as resolved")
	}
}

func TestPinnedTrafficManagerAuditsThePilotThatChose(t *testing.T) {
	manager, audit := pinnedManager(nil, nil)
	manager.Pilots = fake.PilotResolverFunc(func(context.Context, int64, string, domain.PrincipalRef, time.Time) (domain.PilotMember, bool, error) {
		return domain.PilotMember{Version: "6", Subject: domain.PilotSubject{Kind: domain.PilotPrincipal, ID: "42"}}, true, nil
	})
	request := writerRequest
	request.Subject = pilotPerson
	if _, err := manager.ExplainVersion(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(audit.Events) != 1 || audit.Events[0].Reason != string(domain.VersionFromPilot) || audit.Events[0].ReleaseVersion != "6" {
		t.Fatalf("audit = %+v", audit.Events)
	}
	var payload struct {
		Source domain.VersionSource `json:"source"`
		Pilot  domain.PilotSubject  `json:"pilot"`
	}
	if err := json.Unmarshal(audit.Events[0].Payload, &payload); err != nil || payload.Pilot != (domain.PilotSubject{Kind: domain.PilotPrincipal, ID: "42"}) {
		t.Fatalf("payload = %s, err = %v", audit.Events[0].Payload, err)
	}
}

func TestStudioExplainReportsAPilotRelease(t *testing.T) {
	query := newQueryFake(map[string][][]any{qTrafficConversation: {{"writer-a", "writer", "6", "pilot", "", "digest", testStart}}})
	explained, err := studioTraffic(query, domain.StudioView).Explain(context.Background(), trafficActor, "conversation-1")
	if err != nil || explained.Source != domain.VersionFromPilot || explained.Version != "6" {
		t.Fatalf("explained = %+v, err = %v", explained, err)
	}
}
