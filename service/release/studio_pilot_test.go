package release

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// pilotTx is the transaction of one pilot change: it records the call order and whether
// the change committed or rolled back.
type pilotTx struct {
	*queryFake
	calls              []string
	commits, rollbacks int
}

func (tx *pilotTx) Query(ctx context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	tx.calls = append(tx.calls, name)
	return tx.queryFake.Query(ctx, name, args...)
}

func (tx *pilotTx) Commit(context.Context) error   { tx.commits++; return nil }
func (tx *pilotTx) Rollback(context.Context) error { tx.rollbacks++; return nil }

type pilotDB struct {
	keelport.DatabaseRepository
	tx *pilotTx
}

func (db pilotDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.tx
}

func (db pilotDB) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.tx, nil
}

var (
	pilotActor  = domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}
	pilotBegda  = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	pilotChange = domain.PilotChange{
		AgentID: "writer-a", Version: "4", Reason: "supervised pilot for the finance team",
		Subject: domain.PilotSubject{Kind: domain.PilotPrincipal, ID: "1001"},
	}
)

// studioPilots answers a fresh add: the release exists unquarantined, the subject has no
// open membership, and the agent holds none.
func studioPilots(rows map[string][][]any, allowed domain.StudioAction) (*StudioPilots, *pilotTx) {
	defaults := map[string][][]any{
		qPilotVersion: {{false}},
		qPilotCount:   {{int64(0)}},
		qPilotInsert:  {{pilotBegda}},
	}
	for name, value := range rows {
		defaults[name] = value
	}
	tx := &pilotTx{queryFake: newQueryFake(defaults)}
	return &StudioPilots{
		DB: pilotDB{tx: tx}, MaxMembers: 3,
		Authorizer: studioAuthorizerFunc(func(_ context.Context, actor domain.StudioActor, action domain.StudioAction, agentID string) error {
			if action != allowed || actor != pilotActor || agentID != "writer-a" {
				return domain.ErrForbidden
			}
			return nil
		}),
	}, tx
}

func TestStudioPilotAddRecordsTheMembershipAndItsAuthor(t *testing.T) {
	pilots, tx := studioPilots(nil, domain.StudioPublish)
	member, err := pilots.Add(context.Background(), pilotActor, pilotChange)
	if err != nil {
		t.Fatal(err)
	}
	if member.Version != "4" || member.Subject != pilotChange.Subject || member.AddedBy != pilotActor.Principal ||
		!member.ValidFrom.Equal(pilotBegda) || !member.ValidTo.IsZero() || member.TenantID != 7 {
		t.Fatalf("member = %+v", member)
	}
	// The per-agent lock comes first, so the open check, the cap, and the write cannot race.
	want := []string{qPilotLock, qPilotVersion, qPilotOpen, qPilotCount, qPilotInsert, qPilotEvent}
	if !slices.Equal(tx.calls, want) || tx.commits != 1 {
		t.Fatalf("calls = %v, commits = %d", tx.calls, tx.commits)
	}
	requireArgs(t, tx.queryFake, qPilotLock, "agent_pilot:7:writer-a")
	requireArgs(t, tx.queryFake, qPilotVersion, int64(7), "writer-a", "4")
	requireArgs(t, tx.queryFake, qPilotOpen, int64(7), "writer-a", "principal", "1001")
	requireArgs(t, tx.queryFake, qPilotInsert, int64(7), "writer-a", "principal", "1001", "4", pilotChange.Reason, "human", "42")
	requireArgs(t, tx.queryFake, qPilotEvent, int64(7), "writer-a", "PILOT_ADD",
		"principal 1001 on version 4: supervised pilot for the finance team", "human", "42")
}

func TestStudioPilotChangesNeedPublish(t *testing.T) {
	pilots, tx := studioPilots(nil, domain.StudioView)
	if _, err := pilots.Add(context.Background(), pilotActor, pilotChange); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("add with view only = %v, want forbidden", err)
	}
	if _, err := pilots.Remove(context.Background(), pilotActor, pilotChange); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("remove with view only = %v, want forbidden", err)
	}
	if len(tx.calls) != 0 {
		t.Fatalf("an unauthorized change touched the database: %v", tx.calls)
	}
}

func TestStudioPilotAddValidatesEveryField(t *testing.T) {
	pilots, tx := studioPilots(nil, domain.StudioPublish)
	cases := map[string]func(*domain.PilotChange){
		"no agent":             func(change *domain.PilotChange) { change.AgentID = " " },
		"long agent":           func(change *domain.PilotChange) { change.AgentID = strings.Repeat("a", 81) },
		"no version":           func(change *domain.PilotChange) { change.Version = "" },
		"unknown kind":         func(change *domain.PilotChange) { change.Subject.Kind = "team" },
		"principal not a user": func(change *domain.PilotChange) { change.Subject.ID = "ada" },
		"principal not canon":  func(change *domain.PilotChange) { change.Subject.ID = "01001" },
		"principal not >0":     func(change *domain.PilotChange) { change.Subject.ID = "0" },
		"empty position": func(change *domain.PilotChange) {
			change.Subject = domain.PilotSubject{Kind: domain.PilotPosition, ID: "  "}
		},
		"long position": func(change *domain.PilotChange) {
			change.Subject = domain.PilotSubject{Kind: domain.PilotPosition, ID: strings.Repeat("p", 201)}
		},
		"no reason":   func(change *domain.PilotChange) { change.Reason = " " },
		"long reason": func(change *domain.PilotChange) { change.Reason = strings.Repeat("é", 401) },
	}
	for name, mutate := range cases {
		change := pilotChange
		mutate(&change)
		if _, err := pilots.Add(context.Background(), pilotActor, change); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: err = %v, want validation", name, err)
		}
	}
	change := pilotChange
	change.Subject.Kind, change.Version, change.Reason = "team", "", ""
	_, err := pilots.Add(context.Background(), pilotActor, change)
	for _, field := range []string{"version", "subject_kind", "reason"} {
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("err = %v, want every invalid field named, including %s", err, field)
		}
	}
	if len(tx.calls) != 0 {
		t.Fatalf("an invalid change touched the database: %v", tx.calls)
	}
}

func TestStudioPilotAddTrimsAPositionReference(t *testing.T) {
	pilots, tx := studioPilots(nil, domain.StudioPublish)
	change := pilotChange
	change.Subject, change.Reason = domain.PilotSubject{Kind: domain.PilotPosition, ID: "  emea/sales-lead "}, "  pilot  "
	member, err := pilots.Add(context.Background(), pilotActor, change)
	if err != nil || member.Subject.ID != "emea/sales-lead" || member.Reason != "pilot" {
		t.Fatalf("member = %+v, err = %v", member, err)
	}
	requireArgs(t, tx.queryFake, qPilotInsert, int64(7), "writer-a", "position", "emea/sales-lead", "4", "pilot", "human", "42")
}

func TestStudioPilotEventDetailFitsItsColumn(t *testing.T) {
	pilots, tx := studioPilots(nil, domain.StudioPublish)
	change := pilotChange
	change.Subject = domain.PilotSubject{Kind: domain.PilotPosition, ID: strings.Repeat("p", 200)}
	change.Reason = strings.Repeat("é", 400)
	if _, err := pilots.Add(context.Background(), pilotActor, change); err != nil {
		t.Fatal(err)
	}
	detail, _ := tx.args[qPilotEvent][3].(string)
	if runes := len([]rune(detail)); runes != 400 || !strings.HasPrefix(detail, "position ppp") {
		t.Fatalf("detail has %d characters: %q", runes, detail)
	}
}

func TestStudioPilotAddRefusesAnUnknownOrQuarantinedRelease(t *testing.T) {
	pilots, tx := studioPilots(map[string][][]any{qPilotVersion: nil}, domain.StudioPublish)
	if _, err := pilots.Add(context.Background(), pilotActor, pilotChange); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown release = %v, want not found", err)
	}
	if tx.args[qPilotInsert] != nil || tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("an unknown release was piloted: commits = %d, rollbacks = %d", tx.commits, tx.rollbacks)
	}
	pilots, tx = studioPilots(map[string][][]any{qPilotVersion: {{true}}}, domain.StudioPublish)
	if _, err := pilots.Add(context.Background(), pilotActor, pilotChange); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("quarantined release = %v, want conflict", err)
	}
	if tx.args[qPilotInsert] != nil || tx.commits != 0 {
		t.Fatal("a quarantined release was piloted")
	}
}

func TestStudioPilotAddIsIdempotentForTheSameRelease(t *testing.T) {
	open := [][]any{{"principal", "1001", "4", "first reason", "human", "9", pilotBegda, nil}}
	pilots, tx := studioPilots(map[string][][]any{qPilotOpen: open}, domain.StudioPublish)
	member, err := pilots.Add(context.Background(), pilotActor, pilotChange)
	if err != nil || member.Reason != "first reason" || member.AddedBy.ID != "9" || !member.ValidFrom.Equal(pilotBegda) {
		t.Fatalf("replay = %+v, err = %v", member, err)
	}
	if tx.args[qPilotInsert] != nil || tx.args[qPilotEvent] != nil || tx.commits != 0 {
		t.Fatal("a replayed add wrote a second membership or event")
	}
	open[0][2] = "3"
	pilots, tx = studioPilots(map[string][][]any{qPilotOpen: open}, domain.StudioPublish)
	if _, err = pilots.Add(context.Background(), pilotActor, pilotChange); !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "version 3") {
		t.Fatalf("another release = %v, want a conflict naming version 3", err)
	}
	if tx.args[qPilotInsert] != nil {
		t.Fatal("a subject was put in two pilots of one agent")
	}
}

func TestStudioPilotAddEnforcesTheMemberCap(t *testing.T) {
	pilots, tx := studioPilots(map[string][][]any{qPilotCount: {{int64(3)}}}, domain.StudioPublish)
	if _, err := pilots.Add(context.Background(), pilotActor, pilotChange); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("full pilot = %v, want conflict", err)
	}
	requireArgs(t, tx.queryFake, qPilotCount, int64(7), "writer-a")
	if tx.args[qPilotInsert] != nil {
		t.Fatal("a member was added above the cap")
	}
	pilots, _ = studioPilots(nil, domain.StudioPublish)
	pilots.MaxMembers = 0
	if _, err := pilots.Add(context.Background(), pilotActor, pilotChange); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("zero cap = %v, want every add refused", err)
	}
	pilots, _ = studioPilots(nil, domain.StudioPublish)
	pilots.MaxMembers = -1
	if _, err := pilots.Add(context.Background(), pilotActor, pilotChange); err == nil {
		t.Fatal("a negative cap must fail construction")
	}
}

func TestStudioPilotAddRollsBackWhenTheEventFails(t *testing.T) {
	pilots, tx := studioPilots(nil, domain.StudioPublish)
	tx.err[qPilotEvent] = errors.New("connection reset")
	if _, err := pilots.Add(context.Background(), pilotActor, pilotChange); err == nil {
		t.Fatal("an unaudited membership must not be added")
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("commits = %d, rollbacks = %d", tx.commits, tx.rollbacks)
	}
}

func TestStudioPilotRemoveEndsTheOpenMembership(t *testing.T) {
	ended := pilotBegda.Add(time.Hour)
	pilots, tx := studioPilots(map[string][][]any{
		qPilotEnd: {{"principal", "1001", "4", "supervised pilot", "human", "9", pilotBegda, ended}},
	}, domain.StudioPublish)
	change := pilotChange
	change.Reason = "pilot finished"
	member, err := pilots.Remove(context.Background(), pilotActor, change)
	if err != nil || !member.ValidTo.Equal(ended) || member.Reason != "supervised pilot" || member.AddedBy.ID != "9" {
		t.Fatalf("removed = %+v, err = %v", member, err)
	}
	want := []string{qPilotLock, qPilotDrop, qPilotEnd, qPilotEvent}
	if !slices.Equal(tx.calls, want) || tx.commits != 1 {
		t.Fatalf("calls = %v, commits = %d", tx.calls, tx.commits)
	}
	requireArgs(t, tx.queryFake, qPilotEnd, int64(7), "writer-a", "principal", "1001", "4")
	requireArgs(t, tx.queryFake, qPilotEvent, int64(7), "writer-a", "PILOT_REMOVE", "principal 1001 on version 4: pilot finished", "human", "42")
}

func TestStudioPilotRemoveDropsAMembershipThatNeverTookEffect(t *testing.T) {
	pilots, tx := studioPilots(map[string][][]any{
		qPilotDrop: {{"principal", "1001", "4", "supervised pilot", "human", "9", pilotBegda, pilotBegda}},
	}, domain.StudioPublish)
	member, err := pilots.Remove(context.Background(), pilotActor, pilotChange)
	if err != nil || !member.ValidTo.Equal(member.ValidFrom) {
		t.Fatalf("removed = %+v, err = %v", member, err)
	}
	if tx.args[qPilotEnd] != nil || tx.commits != 1 {
		t.Fatalf("calls = %v, commits = %d", tx.calls, tx.commits)
	}
}

func TestStudioPilotRemoveOfNoOpenMembershipIsNotFound(t *testing.T) {
	pilots, tx := studioPilots(nil, domain.StudioPublish)
	if _, err := pilots.Remove(context.Background(), pilotActor, pilotChange); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
	if tx.args[qPilotEvent] != nil || tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("a removal of nothing was recorded: commits = %d", tx.commits)
	}
}

func TestStudioPilotListNeedsViewAndReadsTheActorsTenant(t *testing.T) {
	pilots, tx := studioPilots(map[string][][]any{qPilotList: {
		{"position", "emea/sales-lead", "5", "regional pilot", "service", "installer", pilotBegda, nil},
		{"principal", "1001", "4", "supervised pilot", "human", "42", pilotBegda, nil},
	}}, domain.StudioView)
	members, err := pilots.List(context.Background(), pilotActor, "writer-a")
	if err != nil || len(members) != 2 || members[0].Subject.Kind != domain.PilotPosition || members[0].AddedBy.Kind != domain.PrincipalService ||
		members[1].Version != "4" || members[1].TenantID != 7 {
		t.Fatalf("members = %+v, err = %v", members, err)
	}
	requireArgs(t, tx.queryFake, qPilotList, int64(7), "writer-a")
	pilots, tx = studioPilots(nil, domain.StudioPublish)
	if _, err = pilots.List(context.Background(), pilotActor, "writer-a"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("list without view = %v, want forbidden", err)
	}
	if _, err = pilots.List(context.Background(), pilotActor, " "); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("list without an agent = %v, want validation", err)
	}
	if len(tx.calls) != 0 {
		t.Fatalf("a refused list read memberships: %v", tx.calls)
	}
}
