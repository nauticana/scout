package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/scope"
)

var scheduledAt = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func TestScheduledPublishRecordsAPendingActivationInsteadOfDeploying(t *testing.T) {
	service, query := publishService()
	service.Now = func() time.Time { return scheduledAt }
	effective := scheduledAt.Add(72 * time.Hour)
	query.rows[qScheduleDeployment] = [][]any{{"3", nil}}
	query.rows[qScheduleInsert] = [][]any{activationRow(41, "4", strings.Repeat("a", 64), 0, "3", "", effective, "pending", 0)}

	release, err := service.Publish(context.Background(), humanActor(8, 9), domain.AgentPublishRequest{
		AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2, EffectiveAt: effective,
	})
	if err != nil {
		t.Fatal(err)
	}
	if release.Active || release.Activation == nil || release.Activation.ID != 41 || release.Activation.State != domain.ActivationPending {
		t.Fatalf("release = %+v", release)
	}
	if _, deployed := query.args[qStudioDeployVersion]; deployed {
		t.Fatal("a scheduled publish must not deploy")
	}
	insert := query.args[qScheduleInsert]
	if insert[0] != int64(8) || insert[2] != "1" || insert[4] != 0 || insert[5] != "3" || insert[6] != nil ||
		!insert[7].(time.Time).Equal(effective) || insert[8] != "human" || insert[9] != "9" {
		t.Fatalf("schedule args = %v", insert)
	}
	if audit := query.args[qStudioAudit]; !strings.Contains(audit[3].(string), "scheduled for 2026-10-04T09:00:00Z") || query.commits != 1 {
		t.Fatalf("audit = %v, commits = %d", audit, query.commits)
	}
}

func TestPublishAtOrBeforeNowDeploysImmediately(t *testing.T) {
	service, query := publishService()
	service.Now = func() time.Time { return scheduledAt }
	release, err := service.Publish(context.Background(), humanActor(8, 9), domain.AgentPublishRequest{
		AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2, EffectiveAt: scheduledAt.Add(-time.Minute),
	})
	if err != nil || !release.Active || release.Activation != nil {
		t.Fatalf("release = %+v, err = %v", release, err)
	}
	if _, scheduled := query.args[qScheduleInsert]; scheduled {
		t.Fatal("a past effective time must activate now")
	}
}

func TestScheduledPublishRefusals(t *testing.T) {
	cases := map[string]struct {
		pending, deployment [][]any
		canary              int
		lead                time.Duration
		want                error
	}{
		"second pending activation":   {pending: [][]any{{int64(7)}}, lead: time.Hour, want: domain.ErrConflict},
		"canary without stable":       {canary: 20, lead: time.Hour, want: domain.ErrValidation},
		"canary out of range":         {deployment: [][]any{{"3", nil}}, canary: 101, lead: time.Hour, want: domain.ErrValidation},
		"effective time too far away": {lead: MaxActivationLead + time.Hour, want: domain.ErrValidation},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			service, query := publishService()
			service.Now = func() time.Time { return scheduledAt }
			query.rows[qSchedulePending], query.rows[qScheduleDeployment] = tc.pending, tc.deployment
			_, err := service.Publish(context.Background(), humanActor(8, 9), domain.AgentPublishRequest{
				AgentID: "writer-a", ExpectedDraftRevision: 4, ExpectedPromptProfileRevision: 2,
				CanaryPercentage: tc.canary, EffectiveAt: scheduledAt.Add(tc.lead),
			})
			if !errors.Is(err, tc.want) || query.commits != 0 {
				t.Fatalf("error = %v, commits = %d", err, query.commits)
			}
			if _, inserted := query.args[qScheduleInsert]; inserted {
				t.Fatal("a refused schedule must not be recorded")
			}
		})
	}
}

func TestActivationActivatesStableWithACompareAndSwap(t *testing.T) {
	definition, digest := frozenDefinition(t)
	db := newScriptedDB()
	db.script(qActivationDue, [][]any{{int64(41)}})
	db.script(qActivationClaim, [][]any{activationRow(41, "4", digest, 0, "3", "", scheduledAt, "pending", 0)})
	db.script(qActivationVersion, [][]any{{definition, digest}})
	db.script(qActivationStable, [][]any{{"4"}})
	db.script(qActivationDone, [][]any{{int64(41)}})
	var seen domain.PublishCandidate
	activations := activationService(db, guardFunc(func(_ context.Context, candidate domain.PublishCandidate) error {
		seen = candidate
		return nil
	}))

	report, err := activations.ActivateDue(context.Background(), activationSettings())
	if err != nil || report.Activated != 1 {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	if swap := db.last(qActivationStable); swap[0] != "4" || swap[1] != int64(8) || swap[3] != "3" || swap[4] != nil {
		t.Fatalf("swap args = %v", swap)
	}
	if seen.Actor.Principal.ID != "9" || seen.Actor.TenantID != 8 || seen.Definition.DefinitionDigest != digest || !seen.Activation {
		t.Fatalf("guard candidate = %+v", seen)
	}
	if audit := db.last(qStudioAudit); audit[2] != "ACTIVATE" || audit[5] != "9" || db.commits != 1 {
		t.Fatalf("audit = %v, commits = %d", audit, db.commits)
	}
	if db.called(qStudioDeployVersion) || db.called(qActivationRefuse) {
		t.Fatalf("unexpected writes: %v", db.names())
	}
}

func TestActivationOfAFirstReleaseInsertsTheDeploymentOnlyIfNoneExists(t *testing.T) {
	definition, digest := frozenDefinition(t)
	db := newScriptedDB()
	db.script(qActivationDue, [][]any{{int64(41)}})
	db.script(qActivationClaim, [][]any{activationRow(41, "1", digest, 0, "", "", scheduledAt, "pending", 0)})
	db.script(qActivationVersion, [][]any{{definition, digest}})
	db.script(qActivationDeployment, [][]any{{"2", nil, int64(0)}})
	db.script(qActivationRefuse, [][]any{{int64(41)}})

	report, err := activationService(db).ActivateDue(context.Background(), activationSettings())
	if err != nil || report.Refused != 1 || !db.called(qActivationFirstStable) {
		t.Fatalf("report = %+v, err = %v, queries = %v", report, err, db.names())
	}
	if refusal := db.last(qActivationRefuse); refusal[0] != string(domain.ActivationDeploymentChanged) {
		t.Fatalf("refusal = %v", refusal)
	}
}

func TestActivationIsIdempotentWhenTheDeploymentAlreadyServesTheRelease(t *testing.T) {
	definition, digest := frozenDefinition(t)
	db := newScriptedDB()
	db.script(qActivationDue, [][]any{{int64(41)}})
	db.script(qActivationClaim, [][]any{activationRow(41, "4", digest, 25, "3", "", scheduledAt, "pending", 0)})
	db.script(qActivationVersion, [][]any{{definition, digest}})
	db.script(qActivationDeployment, [][]any{{"3", "4", int64(25)}})
	db.script(qActivationDone, [][]any{{int64(41)}})

	report, err := activationService(db).ActivateDue(context.Background(), activationSettings())
	if err != nil || report.Activated != 1 {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	if canary := db.last(qActivationCanary); canary[0] != "4" || canary[1] != 25 || canary[4] != "3" {
		t.Fatalf("canary swap = %v", canary)
	}
	if done := db.last(qActivationDone); done[0] != "the deployment already served it" {
		t.Fatalf("done = %v", done)
	}
}

func TestActivationRefusesWhatAPublishWouldRefuse(t *testing.T) {
	definition, digest := frozenDefinition(t)
	deny := fake.PrincipalAuthorizerFunc(func(context.Context, domain.Principal, string, string, string) (domain.AuthorizationGrant, error) {
		return domain.AuthorizationGrant{}, nil
	})
	cases := map[string]struct {
		scheduledDigest string
		archived        bool
		roles           contract.PrincipalAuthorizer
		guard           error
		want            domain.ReleaseActivationReason
	}{
		"digest changed":     {scheduledDigest: strings.Repeat("b", 64), want: domain.ActivationDigestMismatch},
		"authority revoked":  {roles: deny, want: domain.ActivationNotAuthorized},
		"agent archived":     {archived: true, want: domain.ActivationAgentArchived},
		"guard refuses":      {guard: domain.ErrContractFailed, want: domain.ActivationGuardRefused},
		"deployment changed": {want: domain.ActivationDeploymentChanged},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			scheduled := digest
			if tc.scheduledDigest != "" {
				scheduled = tc.scheduledDigest
			}
			db := newScriptedDB()
			db.script(qActivationDue, [][]any{{int64(41)}})
			db.script(qActivationClaim, [][]any{activationRow(41, "4", scheduled, 0, "3", "", scheduledAt, "pending", 0)})
			db.script(qActivationVersion, [][]any{{definition, digest}})
			db.script(qActivationDeployment, [][]any{{"5", nil, int64(0)}})
			db.script(qActivationRefuse, [][]any{{int64(41)}})
			if tc.archived {
				db.script(qActivationAgentState, [][]any{{"archived"}})
			}
			activations := activationService(db, guardFunc(func(context.Context, domain.PublishCandidate) error { return tc.guard }))
			if tc.roles != nil {
				activations.Studio.Roles = tc.roles
			}
			report, err := activations.ActivateDue(context.Background(), activationSettings())
			if err != nil || report.Refused != 1 {
				t.Fatalf("report = %+v, err = %v", report, err)
			}
			refusal := db.last(qActivationRefuse)
			if refusal[0] != string(tc.want) || refusal[2] != 0 || db.commits != 1 {
				t.Fatalf("refusal = %v, commits = %d", refusal, db.commits)
			}
			if audit := db.last(qStudioAudit); audit[2] != "ACTIVATION_REFUSED" {
				t.Fatalf("audit = %v", audit)
			}
			if db.called(qActivationDone) {
				t.Fatal("a refused activation must not be marked activated")
			}
		})
	}
}

func TestActivationReadsTheAgentStateUnderTheDraftLockBeforeTheSwap(t *testing.T) {
	definition, digest := frozenDefinition(t)
	db := newScriptedDB()
	db.script(qActivationDue, [][]any{{int64(41)}})
	db.script(qActivationClaim, [][]any{activationRow(41, "4", digest, 0, "3", "", scheduledAt, "pending", 0)})
	db.script(qActivationVersion, [][]any{{definition, digest}})
	db.script(qActivationAgentState, [][]any{{"archived"}})
	db.script(qActivationRefuse, [][]any{{int64(41)}})
	// An earlier, unlocked read still saw the agent active; only the locked read decides.
	db.script(qStudioGetDraft, [][]any{draftRow(false)})
	guarded := false
	activations := activationService(db, guardFunc(func(context.Context, domain.PublishCandidate) error { guarded = true; return nil }))

	report, err := activations.ActivateDue(context.Background(), activationSettings())
	if err != nil || report.Refused != 1 || !guarded {
		t.Fatalf("report = %+v, err = %v, guarded = %v", report, err, guarded)
	}
	if refusal := db.last(qActivationRefuse); refusal[0] != string(domain.ActivationAgentArchived) {
		t.Fatalf("refusal = %v", refusal)
	}
	if db.called(qActivationStable) || db.called(qActivationDone) {
		t.Fatalf("an archived agent's deployment must not move: %v", db.names())
	}
	order := strings.Join(db.names(), ",")
	if lock, state := strings.Index(order, qActivationLockDraft), strings.Index(order, qActivationAgentState); lock < 0 || state < lock ||
		state > strings.Index(order, qActivationRefuse) {
		t.Fatalf("the state must be read after the draft lock, inside the claim: %s", order)
	}
	if lock := db.last(qActivationLockDraft); lock[0] != int64(8) || lock[1] != "writer-a" {
		t.Fatalf("lock args = %v", lock)
	}

	db = newScriptedDB()
	db.script(qActivationDue, [][]any{{int64(41)}})
	db.script(qActivationClaim, [][]any{activationRow(41, "4", digest, 0, "3", "", scheduledAt, "pending", 0)})
	db.script(qActivationVersion, [][]any{{definition, digest}})
	db.script(qActivationLockDraft, nil)
	db.script(qActivationRefuse, [][]any{{int64(41)}})
	if report, err = activationService(db).ActivateDue(context.Background(), activationSettings()); err != nil || report.Refused != 1 {
		t.Fatalf("missing agent: report = %+v, err = %v", report, err)
	}
	if refusal := db.last(qActivationRefuse); refusal[0] != string(domain.ActivationVersionMissing) || db.called(qActivationStable) {
		t.Fatalf("missing agent: refusal = %v, queries = %v", refusal, db.names())
	}
}

func TestActivationGuardsOnlyTheSettingsTheReleaseDigestCovers(t *testing.T) {
	published := []domain.EffectiveResource{{ResourceKind: domain.ResourceBudget, ResourceID: "budget", Value: []byte(`{"tokens":1000}`)}}
	altered := []domain.EffectiveResource{{ResourceKind: domain.ResourceBudget, ResourceID: "budget", Value: []byte(`{"tokens":900000}`)}}
	definition, digest := frozenDefinitionWithSettings(t, scope.SettingsDigest(published))
	cases := map[string]struct {
		settings [][]any
		refused  bool
	}{
		"frozen settings match":   {settings: [][]any{settingsRow(t, published)}},
		"frozen settings altered": {settings: [][]any{settingsRow(t, altered)}, refused: true},
		"frozen settings missing": {refused: true},
		"frozen settings corrupt": {settings: [][]any{{"a:writer-a", "{", scheduledAt, int64(9)}}, refused: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := newScriptedDB()
			db.script(qActivationDue, [][]any{{int64(41)}})
			db.script(qActivationClaim, [][]any{activationRow(41, "4", digest, 0, "3", "", scheduledAt, "pending", 0)})
			db.script(qActivationVersion, [][]any{{definition, digest}})
			db.script(qStudioGetSettings, tc.settings)
			db.script(qActivationStable, [][]any{{"4"}})
			db.script(qActivationDone, [][]any{{int64(41)}})
			db.script(qActivationRefuse, [][]any{{int64(41)}})
			var seen *domain.PublishCandidate
			activations := activationService(db, guardFunc(func(_ context.Context, candidate domain.PublishCandidate) error {
				seen = &candidate
				return nil
			}))
			report, err := activations.ActivateDue(context.Background(), activationSettings())
			if err != nil {
				t.Fatal(err)
			}
			if !tc.refused {
				if report.Activated != 1 || seen == nil || seen.Settings.AgentVersion != "4" || len(seen.Settings.Resources) != 1 ||
					string(seen.Settings.Resources[0].Value) != `{"tokens":1000}` {
					t.Fatalf("report = %+v, guarded = %+v", report, seen)
				}
				return
			}
			if report.Refused != 1 || seen != nil || db.called(qActivationStable) {
				t.Fatalf("report = %+v, guarded = %v, queries = %v", report, seen != nil, db.names())
			}
			if refusal := db.last(qActivationRefuse); refusal[0] != string(domain.ActivationDigestMismatch) {
				t.Fatalf("refusal = %v", refusal)
			}
		})
	}
}

func TestActivationInterruptedByWorkerShutdownIsNotCharged(t *testing.T) {
	definition, digest := frozenDefinition(t)
	db := newScriptedDB()
	db.script(qActivationDue, [][]any{{int64(41)}, {int64(42)}})
	db.script(qActivationClaim, [][]any{activationRow(41, "4", digest, 0, "3", "", scheduledAt, "pending", 2)})
	db.script(qActivationVersion, [][]any{{definition, digest}})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	activations := activationService(db, guardFunc(func(guardCtx context.Context, _ domain.PublishCandidate) error {
		stop()
		return guardCtx.Err()
	}))

	report, err := activations.ActivateDue(ctx, activationSettings())
	if !errors.Is(err, context.Canceled) || report != (domain.ReleaseActivationReport{}) || db.rollbacks == 0 {
		t.Fatalf("report = %+v, err = %v, rollbacks = %d", report, err, db.rollbacks)
	}
	claims := 0
	for _, name := range db.names() {
		if name == qActivationClaim {
			claims++
		}
	}
	if claims != 1 || db.called(qActivationDefer) || db.called(qActivationRefuse) || db.called(qActivationStable) || db.commits != 0 {
		t.Fatalf("a shutdown must neither charge an attempt nor settle: %v", db.names())
	}
}

func TestActivationTimeoutIsChargedAsAnAttempt(t *testing.T) {
	definition, digest := frozenDefinition(t)
	db := newScriptedDB()
	db.script(qActivationDue, [][]any{{int64(41)}})
	db.script(qActivationClaim, [][]any{activationRow(41, "4", digest, 0, "3", "", scheduledAt, "pending", 0)})
	db.script(qActivationVersion, [][]any{{definition, digest}})
	db.script(qActivationDefer, [][]any{{int64(1)}})
	activations := activationService(db, guardFunc(func(guardCtx context.Context, _ domain.PublishCandidate) error {
		<-guardCtx.Done()
		return guardCtx.Err()
	}))
	activations.Timeout = 10 * time.Millisecond

	report, err := activations.ActivateDue(context.Background(), activationSettings())
	if !errors.Is(err, context.DeadlineExceeded) || report.Deferred != 1 || !db.called(qActivationDefer) {
		t.Fatalf("report = %+v, err = %v, queries = %v", report, err, db.names())
	}
}

func TestActivationRetriesTransientFailuresAFixedNumberOfTimes(t *testing.T) {
	definition, digest := frozenDefinition(t)
	unavailable := errors.New("evaluation store unavailable")
	run := func(attempts int) (*scriptedDB, domain.ReleaseActivationReport, error) {
		db := newScriptedDB()
		db.script(qActivationDue, [][]any{{int64(41)}})
		db.script(qActivationClaim, [][]any{activationRow(41, "4", digest, 0, "3", "", scheduledAt, "pending", attempts)})
		db.script(qActivationVersion, [][]any{{definition, digest}})
		db.script(qActivationDefer, [][]any{{int64(attempts + 1)}})
		db.script(qActivationRefuse, [][]any{{int64(41)}})
		report, err := activationService(db, guardFunc(func(context.Context, domain.PublishCandidate) error { return unavailable })).
			ActivateDue(context.Background(), activationSettings())
		return db, report, err
	}

	db, report, err := run(1)
	if !errors.Is(err, unavailable) || report.Deferred != 1 || db.rollbacks == 0 {
		t.Fatalf("report = %+v, err = %v, rollbacks = %d", report, err, db.rollbacks)
	}
	if deferral := db.last(qActivationDefer); deferral[0] != int64(120) || strings.Contains(deferral[1].(string), "unavailable") {
		t.Fatalf("deferral = %v; delay is attempts x poll period and the stored detail stays generic", deferral)
	}
	if db.called(qActivationRefuse) || db.called(qActivationStable) {
		t.Fatalf("a transient failure must neither refuse nor deploy: %v", db.names())
	}

	db, report, err = run(2)
	if !errors.Is(err, unavailable) || report.Refused != 1 || db.called(qActivationDefer) {
		t.Fatalf("report = %+v, err = %v, queries = %v", report, err, db.names())
	}
	if refusal := db.last(qActivationRefuse); refusal[0] != string(domain.ActivationAttemptsExhausted) || refusal[2] != 1 {
		t.Fatalf("refusal = %v", refusal)
	}
}

func TestActivationSkipsWhatAnotherWorkerClaimed(t *testing.T) {
	db := newScriptedDB()
	db.script(qActivationDue, [][]any{{int64(41)}})
	report, err := activationService(db).ActivateDue(context.Background(), activationSettings())
	if err != nil || report != (domain.ReleaseActivationReport{}) || db.called(qActivationVersion) {
		t.Fatalf("report = %+v, err = %v, queries = %v", report, err, db.names())
	}
	if due := db.last(qActivationDue); due[0] != 2 {
		t.Fatalf("due args = %v, want the batch size", due)
	}
}

func TestActivateDueRejectsUnboundedSettings(t *testing.T) {
	activations := activationService(newScriptedDB())
	for _, settings := range []domain.ReleaseActivationSettings{
		{Batch: 0, PollPeriod: time.Minute, MaxAttempts: 3},
		{Batch: 2, PollPeriod: 0, MaxAttempts: 3},
		{Batch: 2, PollPeriod: time.Minute, MaxAttempts: 0},
	} {
		if _, err := activations.ActivateDue(context.Background(), settings); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("settings %+v: error = %v", settings, err)
		}
	}
}

func TestCancelWithdrawsOnlyAPendingActivation(t *testing.T) {
	db := newScriptedDB()
	cancelled := activationRow(41, "4", strings.Repeat("a", 64), 0, "3", "", scheduledAt, "cancelled", 0)
	cancelled[17], cancelled[18] = "human", "9"
	db.script(qActivationCancel, [][]any{cancelled})
	activations := activationService(db)

	activation, err := activations.Cancel(context.Background(), humanActor(8, 9), domain.ReleaseActivationCancel{AgentID: "writer-a", ActivationID: 41, Reason: " campaign moved "})
	if err != nil || activation.State != domain.ActivationCancelled || activation.DecidedBy == nil || activation.DecidedBy.ID != "9" {
		t.Fatalf("activation = %+v, err = %v", activation, err)
	}
	if args := db.last(qActivationCancel); args[2] != "campaign moved" || args[3] != int64(8) || args[5] != int64(41) || db.commits != 1 {
		t.Fatalf("cancel args = %v, commits = %d", args, db.commits)
	}
	if audit := db.last(qStudioAudit); audit[2] != "ACTIVATION_CANCEL" {
		t.Fatalf("audit = %v", audit)
	}

	db = newScriptedDB()
	db.script(qActivationGet, [][]any{activationRow(41, "4", strings.Repeat("a", 64), 0, "3", "", scheduledAt, "activated", 0)})
	if _, err = activationService(db).Cancel(context.Background(), humanActor(8, 9), domain.ReleaseActivationCancel{AgentID: "writer-a", ActivationID: 41}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("cancel of an activated activation: %v", err)
	}
	if _, err = activationService(newScriptedDB()).Cancel(context.Background(), humanActor(8, 9), domain.ReleaseActivationCancel{AgentID: "writer-a", ActivationID: 41}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cancel of a missing activation: %v", err)
	}
	if _, err = activations.Cancel(context.Background(), humanActor(8, 9), domain.ReleaseActivationCancel{AgentID: "writer-a"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("cancel without an id: %v", err)
	}
}

func TestActivationRoutesAuthorizeTheirStudioAction(t *testing.T) {
	db := newScriptedDB()
	db.script(qActivationList, [][]any{activationRow(41, "4", strings.Repeat("a", 64), 0, "3", "", scheduledAt, "pending", 0)})
	activations := activationService(db)
	var actions []string
	activations.Studio.Roles = fake.PrincipalAuthorizerFunc(func(_ context.Context, _ domain.Principal, _, action, value string) (domain.AuthorizationGrant, error) {
		actions = append(actions, action+" "+value)
		return domain.AuthorizationGrant{Allowed: action == string(domain.StudioView)}, nil
	})
	items, err := activations.Activations(context.Background(), humanActor(8, 9), "writer-a")
	if err != nil || len(items) != 1 || items[0].BaseStable != "3" || items[0].ScheduledBy.ID != "9" {
		t.Fatalf("items = %+v, err = %v", items, err)
	}
	if args := db.last(qActivationList); args[0] != int64(8) || args[1] != "writer-a" {
		t.Fatalf("list args = %v", args)
	}
	_, err = activations.Cancel(context.Background(), humanActor(8, 9), domain.ReleaseActivationCancel{AgentID: "writer-a", ActivationID: 41})
	if !errors.Is(err, domain.ErrForbidden) || db.called(qActivationCancel) {
		t.Fatalf("cancel without PUBLISH: %v", err)
	}
	if strings.Join(actions, ",") != "VIEW agent:writer-a,PUBLISH agent:writer-a" {
		t.Fatalf("authorized = %v", actions)
	}
}

// frozenDefinition is a published release as stored: its JSON and its digest.
func frozenDefinition(t *testing.T) (string, string) {
	t.Helper()
	return frozenDefinitionWithSettings(t, "")
}

func frozenDefinitionWithSettings(t *testing.T, settingsDigest string) (string, string) {
	t.Helper()
	compiler := &PromptCompiler{}
	compiled, err := compiler.Compile("en-US", []domain.PromptSectionSource{layered(1, 1, baseLayer("write", ""))})
	if err != nil {
		t.Fatal(err)
	}
	definition := domain.AgentDefinition{AgentID: "writer-a", AgentTypeID: "writer", Version: "4", Enabled: true,
		Languages: []domain.CompiledPrompt{compiled}, SettingsDigest: settingsDigest}
	if definition.DefinitionDigest, err = compiler.DefinitionDigest(definition); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded), definition.DefinitionDigest
}

func activationRow(id int64, version, digest string, canary int, baseStable, baseCanary string, effective time.Time, state string, attempts int) []any {
	return []any{id, int64(8), "writer-a", version, digest, int64(canary), nullableString(baseStable), nullableString(baseCanary),
		effective, state, "human", "9", scheduledAt.Add(-time.Hour), int64(attempts), nil, nil, nil, nil, nil, nil}
}

// settingsRow is a qStudioGetSettings row whose payload freezes resources.
func settingsRow(t *testing.T, resources []domain.EffectiveResource) []any {
	t.Helper()
	payload, err := json.Marshal(resources)
	if err != nil {
		t.Fatal(err)
	}
	return []any{"a:writer-a", string(payload), scheduledAt, int64(9)}
}

func activationSettings() domain.ReleaseActivationSettings {
	return domain.ReleaseActivationSettings{Batch: 2, PollPeriod: time.Minute, MaxAttempts: 3}
}

func activationService(db *scriptedDB, guards ...contract.PublishGuard) *ReleaseActivations {
	if _, ok := db.rows[qStudioGetDraft]; !ok {
		db.script(qStudioGetDraft, [][]any{draftRow(false)})
	}
	if _, ok := db.rows[qActivationLockDraft]; !ok {
		db.script(qActivationLockDraft, [][]any{{int64(4)}})
	}
	if _, ok := db.rows[qActivationAgentState]; !ok {
		db.script(qActivationAgentState, [][]any{{"active"}})
	}
	studio := &StudioService{Principals: studioPrincipals, Roles: studioRoles, DB: db, Guards: guards,
		Sources: unusedSources{}, Assembler: unusedAssembler{}, Compiler: &PromptCompiler{}}
	return &ReleaseActivations{DB: db, Studio: studio}
}

type scriptedCall struct {
	name string
	args []any
}

// scriptedDB answers each named query with its scripted rows and records every call.
type scriptedDB struct {
	keelport.DatabaseRepository
	rows      map[string][][]any
	calls     []scriptedCall
	commits   int
	rollbacks int
}

func newScriptedDB() *scriptedDB { return &scriptedDB{rows: map[string][][]any{}} }

func (db *scriptedDB) script(name string, rows [][]any) { db.rows[name] = rows }

func (db *scriptedDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return scriptedTx{db}
}

func (db *scriptedDB) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return scriptedTx{db}, nil
}

func (db *scriptedDB) called(name string) bool { return db.last(name) != nil }

func (db *scriptedDB) last(name string) []any {
	for i := len(db.calls) - 1; i >= 0; i-- {
		if db.calls[i].name == name {
			if db.calls[i].args == nil {
				return []any{}
			}
			return db.calls[i].args
		}
	}
	return nil
}

func (db *scriptedDB) names() []string {
	names := make([]string, 0, len(db.calls))
	for _, call := range db.calls {
		names = append(names, call.name)
	}
	return names
}

type scriptedTx struct{ db *scriptedDB }

func (tx scriptedTx) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	tx.db.calls = append(tx.db.calls, scriptedCall{name: name, args: args})
	return &keelmodel.QueryResult{Rows: tx.db.rows[name]}, nil
}

func (scriptedTx) GenID() int64                      { return 0 }
func (tx scriptedTx) Commit(context.Context) error   { tx.db.commits++; return nil }
func (tx scriptedTx) Rollback(context.Context) error { tx.db.rollbacks++; return nil }
