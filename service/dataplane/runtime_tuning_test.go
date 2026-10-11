package dataplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nauticana/keel/cache"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/isolation"
	agentruntime "github.com/nauticana/scout/service/runtime"
)

func TestBaseDataPlaneHandsItsTuningSettingsToTheComponentsItBuilds(t *testing.T) {
	plane := composablePlane()
	plane.Settings.CostWindow, plane.Settings.ReplyRetention = 2*time.Hour, 3*time.Minute
	plane.Settings.ReplyPollInterval, plane.Settings.ReplyRecordReadInterval = 50*time.Millisecond, 7*time.Second
	plane.Settings.CancelPollInterval, plane.Settings.CancelMaxReadFailures = 250*time.Millisecond, 5
	plane.Settings.SessionRevisionMemory, plane.Settings.ResultCardMaxBytes, plane.Settings.RunPageMax = 99, 1024, 40
	if err := plane.Compose(); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	if hub := plane.ReplyHub; hub.Retention != 3*time.Minute || hub.PollInterval != 50*time.Millisecond || hub.RecordReadInterval != 7*time.Second {
		t.Fatalf("reply hub = %+v", hub)
	}
	if cancels := plane.Cancels; cancels.PollInterval != 250*time.Millisecond || cancels.MaxReadFailures != 5 {
		t.Fatalf("canceller = %+v", cancels)
	}
	sessions := plane.Sessions.(*SessionCoordinator)
	if store := sessions.Store.(*DurableSessionStore); sessions.RevisionMemory != 99 || store.MaxResultCardBytes != 1024 {
		t.Fatalf("revision memory = %d, result card bytes = %d", sessions.RevisionMemory, store.MaxResultCardBytes)
	}
	if runs := plane.Runs.(*agentruntime.AgentRunStore); runs.MaxPage != 40 {
		t.Fatalf("run page = %d", runs.MaxPage)
	}
	if costs := plane.Governor.(*isolation.ExecutionGovernor).Costs.(*isolation.WindowedCostBreaker); costs.Window != 2*time.Hour {
		t.Fatalf("cost window = %s", costs.Window)
	}

	defaulted := composablePlane()
	if err := defaulted.Compose(); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	t.Cleanup(func() { _ = defaulted.Close() })
	if costs := defaulted.Governor.(*isolation.ExecutionGovernor).Costs.(*isolation.WindowedCostBreaker); costs.Window != defaultCostBreakerWindow {
		t.Fatalf("a zero cost window must take the default, got %s", costs.Window)
	}
}

func TestBaseDataPlaneRefusesNegativeTuningSettings(t *testing.T) {
	for name, mutate := range map[string]func(*BaseDataPlane){
		"cost window":       func(plane *BaseDataPlane) { plane.Settings.CostWindow = -time.Second },
		"reply retention":   func(plane *BaseDataPlane) { plane.Settings.ReplyRetention = -time.Second },
		"reply poll":        func(plane *BaseDataPlane) { plane.Settings.ReplyPollInterval = -time.Millisecond },
		"record read":       func(plane *BaseDataPlane) { plane.Settings.ReplyRecordReadInterval = -time.Second },
		"cancel poll":       func(plane *BaseDataPlane) { plane.Settings.CancelPollInterval = -time.Millisecond },
		"cancel failures":   func(plane *BaseDataPlane) { plane.Settings.CancelMaxReadFailures = -1 },
		"revision memory":   func(plane *BaseDataPlane) { plane.Settings.SessionRevisionMemory = -1 },
		"result card bytes": func(plane *BaseDataPlane) { plane.Settings.ResultCardMaxBytes = -1 },
		"run page":          func(plane *BaseDataPlane) { plane.Settings.RunPageMax = -1 },
		"call revisions":    func(plane *BaseDataPlane) { plane.Limits.MaxCallRevisions = -1 },
	} {
		plane := composablePlane()
		mutate(plane)
		if err := plane.Compose(); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: Compose = %v, want ErrValidation", name, err)
		}
	}
}

func TestCacheReplyHubRefusesNegativePeriods(t *testing.T) {
	for name, hub := range map[string]*CacheReplyHub{
		"retention":   {Cache: cache.NewMemoryCacheService(), Retention: -time.Second},
		"poll":        {Cache: cache.NewMemoryCacheService(), PollInterval: -time.Second},
		"record read": {Cache: cache.NewMemoryCacheService(), RecordReadInterval: -time.Second},
	} {
		if err := hub.Publish(context.Background(), replyFrame(0, "x", false)); err == nil {
			t.Errorf("%s: Publish accepted a negative period", name)
		}
		if _, err := hub.SubscribeFrom(context.Background(), 7, "request-1", 0); err == nil {
			t.Errorf("%s: SubscribeFrom accepted a negative period", name)
		}
	}
}

func TestCacheReplyHubThrottlesRecordReadsByItsInterval(t *testing.T) {
	reads := func(interval time.Duration) int64 {
		var finds atomic.Int64
		hub := &CacheReplyHub{Cache: cache.NewMemoryCacheService(), PollInterval: 2 * time.Millisecond, RecordReadInterval: interval,
			Records: &fake.TurnRecordStore{FindFunc: func(context.Context, int64, string) (int64, string, []byte, error) {
				finds.Add(1)
				return 1, "running", nil, nil
			}}}
		defer hub.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer cancel()
		subscription, err := hub.Subscribe(ctx, 7, "request-1")
		if err != nil {
			t.Fatal(err)
		}
		defer subscription.Close()
		if _, err = subscription.Receive(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Receive = %v, want the deadline", err)
		}
		return finds.Load()
	}
	if got := reads(time.Hour); got != 1 {
		t.Fatalf("an hour-long interval reads the record once, got %d", got)
	}
	if got := reads(time.Millisecond); got < 3 {
		t.Fatalf("a millisecond interval reads the record on most polls, got %d", got)
	}
	if got := reads(0); got != 1 {
		t.Fatalf("zero takes the 5s default and reads once, got %d", got)
	}
}

// countingCancelTable counts the cancellation reads a watcher makes.
type countingCancelTable struct {
	*cancelTable
	reads atomic.Int64
}

func (table *countingCancelTable) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return table
}

func (table *countingCancelTable) Query(ctx context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if name == qCancelRead {
		table.reads.Add(1)
	}
	return table.cancelTable.Query(ctx, name, args...)
}

func TestTableTurnCancellerStopsAfterItsConfiguredReadFailures(t *testing.T) {
	for _, tc := range []struct{ configured, want int64 }{{0, defaultCancelReadFailures}, {5, 5}} {
		table := &countingCancelTable{cancelTable: &cancelTable{live: true}}
		watcher := &TableTurnCanceller{DB: table, PollInterval: 2 * time.Millisecond, MaxReadFailures: int(tc.configured)}
		turnCtx, release, err := watcher.Watch(context.Background(), 7, "request-1")
		if err != nil {
			t.Fatal(err)
		}
		table.mu.Lock()
		table.readErr = errors.New("connection refused")
		table.mu.Unlock()
		if cause := awaitCause(t, turnCtx); cause == nil || errors.Is(cause, domain.ErrTurnCanceled) {
			t.Fatalf("configured %d: cause = %v", tc.configured, cause)
		}
		release()
		// One successful read at pickup, then the failures that stopped the turn.
		if got := table.reads.Load() - 1; got != tc.want {
			t.Fatalf("configured %d: failed reads = %d, want %d", tc.configured, got, tc.want)
		}
	}
	for name, watcher := range map[string]*TableTurnCanceller{
		"poll":     {DB: &cancelTable{live: true}, PollInterval: -time.Second},
		"failures": {DB: &cancelTable{live: true}, MaxReadFailures: -1},
	} {
		if _, _, err := watcher.Watch(context.Background(), 7, "request-1"); err == nil {
			t.Errorf("%s: Watch accepted a negative setting", name)
		}
	}
}

func TestStudioPauseListsLiveTurnsInItsConfiguredBatch(t *testing.T) {
	canceller := cancellerFunc(func(context.Context, int64, string, string) error { return nil })
	request := domain.AgentPauseRequest{AgentID: "writer-a", Policy: domain.PauseCancel, Reason: "incident"}
	for _, tc := range []struct{ batch, want int }{{0, defaultPauseBatch}, {2, 2}} {
		query := &queueQueryFake{sequence: map[string][][][]any{qPauseLiveTurns: {{{"r-1"}, {"r-2"}}, {{"r-3"}}}}}
		pause := &StudioPause{DB: queueDBFake{query: query}, Studio: &enabledStudio{}, Canceller: canceller, Batch: tc.batch}
		result, err := pause.Pause(context.Background(), usageActor, request)
		if err != nil {
			t.Fatalf("batch %d: %v", tc.batch, err)
		}
		if args := query.firstArgs(qPauseLiveTurns); args[2] != tc.want {
			t.Fatalf("batch %d: listing limit = %v, want %d", tc.batch, args[2], tc.want)
		}
		// A full batch asks again; the default batch is not filled by two turns.
		if wantCancelled := map[int]int{0: 2, 2: 3}[tc.batch]; result.Cancelled != wantCancelled {
			t.Fatalf("batch %d: cancelled %d, want %d", tc.batch, result.Cancelled, wantCancelled)
		}
	}
	pause := &StudioPause{DB: queueDBFake{query: &queueQueryFake{}}, Studio: &enabledStudio{}, Canceller: canceller, Batch: -1}
	if _, err := pause.Pause(context.Background(), usageActor, request); err == nil {
		t.Fatal("a negative batch must be refused")
	}
}

func TestStudioUsageAndFeedbackReportLimitsAreConfigurable(t *testing.T) {
	line := []any{"4", "", "", "", "USD", int64(2), int64(1), int64(1), int64(1), int64(0), int64(1), "", ""}
	query := &queueQueryFake{rows: map[string][][]any{qStudioUsage: {line, line, line}}}
	usage := &StudioUsage{DB: queueDBFake{query: query}, Authorizer: allowStudio{domain.StudioView}, MaxWindow: 24 * time.Hour, MaxLines: 2}
	if _, err := usage.Usage(context.Background(), usageActor, "writer-a", usageFrom, usageFrom.Add(25*time.Hour)); !errors.Is(err, domain.ErrValidation) ||
		!strings.Contains(err.Error(), "1 days") {
		t.Fatalf("a window over MaxWindow: %v", err)
	}
	if _, err := usage.Usage(context.Background(), usageActor, "writer-a", usageFrom, usageFrom.Add(time.Hour)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("three lines over MaxLines 2: %v", err)
	}
	if args := query.firstArgs(qStudioUsage); args[4] != 3 {
		t.Fatalf("usage limit = %v, want MaxLines+1", args[4])
	}
	for name, invalid := range map[string]*StudioUsage{
		"window": {DB: queueDBFake{query: query}, Authorizer: allowStudio{domain.StudioView}, MaxWindow: -time.Hour},
		"lines":  {DB: queueDBFake{query: query}, Authorizer: allowStudio{domain.StudioView}, MaxLines: -1},
	} {
		if _, err := invalid.Usage(context.Background(), usageActor, "writer-a", usageFrom, usageFrom.Add(time.Hour)); err == nil || errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: a negative limit is a composition error, got %v", name, err)
		}
	}

	feedback := feedbackService(&queueQueryFake{rows: map[string][][]any{}}, true)
	if _, err := feedback.Feedback(context.Background(), usageActor, "writer-a", usageFrom, usageFrom.Add(DefaultUsageWindow)); err != nil {
		t.Fatalf("zero MaxWindow takes the default: %v", err)
	}
	feedback.MaxWindow = time.Hour
	if _, err := feedback.Feedback(context.Background(), usageActor, "writer-a", usageFrom, usageFrom.Add(2*time.Hour)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a feedback window over MaxWindow: %v", err)
	}
	feedback.MaxWindow = -time.Hour
	if _, err := feedback.Feedback(context.Background(), usageActor, "writer-a", usageFrom, usageFrom.Add(time.Minute)); err == nil {
		t.Fatal("a negative feedback window must be refused")
	}
}

func TestToolLoopRevisionLimitIsConfigurable(t *testing.T) {
	rounds := func(limit int) int {
		harness, desk := revisionHarness(t)
		harness.limits.MaxCallRevisions = limit
		if _, err := harness.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrApprovalPending) {
			t.Fatal(err)
		}
		var err error
		round := 0
		for ; round <= defaultCallRevisions+1 && !errors.Is(err, domain.ErrExecutionLimit); round++ {
			parked := harness.calls[len(harness.calls)-1]
			desk.edits[parked.IdempotencyKey] = revisionOf(int64(100+round), fmt.Sprintf(`{"qty":%d}`, round))
			_, err = harness.executor().Execute(context.Background(), loopInput())
		}
		if !errors.Is(err, domain.ErrExecutionLimit) {
			t.Fatalf("limit %d: %v", limit, err)
		}
		return round
	}
	if got := rounds(0); got != defaultCallRevisions+1 {
		t.Fatalf("zero takes the default: refused at round %d, want %d", got, defaultCallRevisions+1)
	}
	if got := rounds(2); got != 3 {
		t.Fatalf("limit 2: refused at round %d, want 3", got)
	}
	harness, _ := revisionHarness(t)
	invalid := *harness.executor()
	invalid.Limits.MaxCallRevisions = -1
	if _, err := NewToolLoopExecutor(invalid); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative revision limit: %v", err)
	}
}

func TestWorkItemStoreDepthIsConfigurable(t *testing.T) {
	item := domain.WorkItem{
		TenantID: 7, Assignee: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "a"},
		Requester: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "b"}, RequestID: "r-1",
		Input: domain.ObjectRef{URI: "scout://in", Digest: strings.Repeat("a", 64)},
	}
	assign := func(store *TableWorkItemStore, depth int) error {
		item.Depth = depth
		_, err := store.Assign(context.Background(), item)
		return err
	}
	query := &queueQueryFake{rows: map[string][][]any{qWorkAssign: {{int64(5)}}}}
	defaulted := &TableWorkItemStore{DB: queueDBFake{query: query}}
	if err := assign(defaulted, defaultWorkItemDepth); err != nil {
		t.Fatalf("depth at the default: %v", err)
	}
	if err := assign(defaulted, defaultWorkItemDepth+1); !errors.Is(err, domain.ErrDelegationDepth) {
		t.Fatalf("depth over the default: %v", err)
	}
	narrowed := &TableWorkItemStore{DB: queueDBFake{query: query}, MaxDepth: 2}
	if err := assign(narrowed, 3); !errors.Is(err, domain.ErrDelegationDepth) {
		t.Fatalf("depth over MaxDepth: %v", err)
	}
	if _, err := narrowed.Ancestors(context.Background(), 7, 5); err != nil {
		t.Fatal(err)
	}
	// Depths 0 through 2 make a chain of three, root included.
	if args := query.firstArgs(qWorkAncestors); args[2] != 3 {
		t.Fatalf("ancestor walk limit = %v, want MaxDepth+1", args[2])
	}
	if err := assign(&TableWorkItemStore{DB: queueDBFake{query: query}, MaxDepth: -1}, 0); err == nil || errors.Is(err, domain.ErrDelegationDepth) {
		t.Fatalf("a negative MaxDepth is a composition error, got %v", err)
	}
}
