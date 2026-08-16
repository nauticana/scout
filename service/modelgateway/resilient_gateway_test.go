package modelgateway

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func testDeadlines() StreamDeadlines {
	return StreamDeadlines{FirstToken: time.Second, Idle: 2 * time.Second, Total: 10 * time.Second}
}

// scriptedStream serves frames and errors from a script; a nil frame blocks until ctx ends.
type scriptedStream struct {
	mu      sync.Mutex
	frames  []domain.ModelChunk
	errs    []error
	index   int
	closed  int
	advance func()
}

func (stream *scriptedStream) Receive(ctx context.Context) (domain.ModelChunk, error) {
	stream.mu.Lock()
	index := stream.index
	stream.index++
	stream.mu.Unlock()
	if stream.advance != nil {
		stream.advance()
	}
	if index < len(stream.errs) && stream.errs[index] != nil {
		return domain.ModelChunk{}, stream.errs[index]
	}
	if index >= len(stream.frames) {
		<-ctx.Done()
		return domain.ModelChunk{}, ctx.Err()
	}
	return stream.frames[index], nil
}

func (stream *scriptedStream) Close() error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.closed++
	return nil
}

func (stream *scriptedStream) closes() int {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.closed
}

func TestResilientGatewayValidatesConfig(t *testing.T) {
	inner := &fake.ModelGateway{}
	tests := []struct {
		name      string
		deadlines StreamDeadlines
		retries   int
		backoff   time.Duration
	}{
		{name: "zero first token", deadlines: StreamDeadlines{Idle: time.Second, Total: time.Second}},
		{name: "zero idle", deadlines: StreamDeadlines{FirstToken: time.Second, Total: time.Second}},
		{name: "total below first token", deadlines: StreamDeadlines{FirstToken: 2 * time.Second, Idle: time.Second, Total: time.Second}},
		{name: "retry without backoff", deadlines: testDeadlines(), retries: 1},
		{name: "negative retries", deadlines: testDeadlines(), retries: -1, backoff: time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewResilientGateway(inner, test.deadlines, test.retries, test.backoff); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	if _, err := NewResilientGateway(nil, testDeadlines(), 0, 0); err == nil {
		t.Fatal("expected inner gateway error")
	}
}

func TestResilientGatewayRetriesBeforeFirstTokenOnly(t *testing.T) {
	clock := routerNow
	attempts := 0
	streams := []*scriptedStream{
		{errs: []error{errors.New("provider reset")}},
		{frames: []domain.ModelChunk{{Sequence: 1, Payload: []byte("a")}}, errs: []error{nil, errors.New("provider reset")}},
	}
	inner := &fake.ModelGateway{StreamFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
		stream := streams[attempts]
		attempts++
		return stream, nil
	}}
	var slept []time.Duration
	gateway, err := NewResilientGateway(inner, testDeadlines(), 2, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	gateway.Now = func() time.Time { return clock }
	gateway.Rand = func() float64 { return 1 }
	gateway.Sleep = func(_ context.Context, delay time.Duration) error {
		slept = append(slept, delay)
		return nil
	}

	stream, err := gateway.Stream(context.Background(), fullSelection(), validModelRequest())
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := stream.Receive(context.Background())
	if err != nil || string(chunk.Payload) != "a" {
		t.Fatalf("first frame = %+v %v", chunk, err)
	}
	if attempts != 2 || len(slept) != 1 || slept[0] != 10*time.Millisecond {
		t.Fatalf("attempts = %d, slept = %v", attempts, slept)
	}

	// The second frame fails after the first token: no restart, an interrupted completion instead.
	chunk, err = stream.Receive(context.Background())
	if attempts != 2 {
		t.Fatalf("retry after first token: attempts = %d", attempts)
	}
	if chunk.FinishReason != domain.FinishReasonInterrupted {
		t.Fatalf("chunk = %+v", chunk)
	}
	if err == nil || errorClass(err) != ErrorClassProvider {
		t.Fatalf("error = %v", err)
	}
	if _, err := stream.Receive(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("closed stream = %v", err)
	}
	if streams[1].closes() != 1 || stream.Close() != nil {
		t.Fatalf("closes = %d", streams[1].closes())
	}
}

func TestResilientGatewayDeadlines(t *testing.T) {
	// Short real durations: the injected clock decides which budget binds, the
	// timer only has to fire once.
	deadlines := StreamDeadlines{FirstToken: 40 * time.Millisecond, Idle: 60 * time.Millisecond, Total: 150 * time.Millisecond}
	tests := []struct {
		name      string
		frames    []domain.ModelChunk
		advance   time.Duration
		wantKind  DeadlineKind
		wantClass string
		wantFirst bool
	}{
		{name: "first token deadline", wantKind: DeadlineFirstToken, wantClass: ErrorClassFirstToken},
		{name: "idle deadline", frames: []domain.ModelChunk{{Sequence: 1, Payload: []byte("a")}},
			wantKind: DeadlineIdle, wantClass: ErrorClassIdle, wantFirst: true},
		{name: "total deadline", frames: []domain.ModelChunk{{Sequence: 1, Payload: []byte("a")}}, advance: 100 * time.Millisecond,
			wantKind: DeadlineTotal, wantClass: ErrorClassDeadline, wantFirst: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			clock := routerNow
			script := &scriptedStream{frames: test.frames}
			script.advance = func() {
				mu.Lock()
				defer mu.Unlock()
				clock = clock.Add(test.advance)
			}
			inner := &fake.ModelGateway{StreamFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
				return script, nil
			}}
			gateway, err := NewResilientGateway(inner, deadlines, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			gateway.Now = func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return clock
			}

			stream, err := gateway.Stream(context.Background(), fullSelection(), validModelRequest())
			if err != nil {
				t.Fatal(err)
			}
			var chunk domain.ModelChunk
			for range len(test.frames) + 1 {
				chunk, err = stream.Receive(context.Background())
			}
			var deadline *StreamDeadlineError
			if !errors.As(err, &deadline) || deadline.Kind != test.wantKind {
				t.Fatalf("error = %v", err)
			}
			if errorClass(err) != test.wantClass || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("class = %q, error = %v", errorClass(err), err)
			}
			if (chunk.FinishReason == domain.FinishReasonInterrupted) != test.wantFirst {
				t.Fatalf("chunk = %+v", chunk)
			}
			if script.closes() != 1 {
				t.Fatalf("closes = %d", script.closes())
			}
		})
	}
}

func TestResilientGatewayDrainCancelsWithPartialCompletion(t *testing.T) {
	clock := routerNow
	script := &scriptedStream{frames: []domain.ModelChunk{{Sequence: 1, Payload: []byte("a")}}}
	script.advance = func() { clock = clock.Add(400 * time.Millisecond) }
	inner := &fake.ModelGateway{StreamFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
		return script, nil
	}}
	routes := &fake.CapacitySnapshotSource{}
	gateway, err := NewResilientGateway(inner, testDeadlines(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	gateway.Now = func() time.Time { return clock }
	gateway.Routes = routes

	stream, err := gateway.Stream(context.Background(), fullSelection(), validModelRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	routes.Items = []domain.CapacitySnapshot{{Provider: "provider", Model: "model", Region: "eu", RouteID: "eu-1",
		Draining: true, DrainDeadline: clock.Add(100 * time.Millisecond)}}
	chunk, err := stream.Receive(context.Background())
	var deadline *StreamDeadlineError
	if !errors.As(err, &deadline) || deadline.Kind != DeadlineDrain || chunk.FinishReason != domain.FinishReasonInterrupted {
		t.Fatalf("chunk = %+v, error = %v", chunk, err)
	}
	if errorClass(err) != ErrorClassDrained {
		t.Fatalf("class = %q", errorClass(err))
	}

	// A draining route admits nothing new.
	if _, err := gateway.Stream(context.Background(), fullSelection(), validModelRequest()); !errors.Is(err, domain.ErrNoRoute) {
		t.Fatalf("drained admission = %v", err)
	}
	if _, err := gateway.Generate(context.Background(), fullSelection(), validModelRequest()); !errors.Is(err, domain.ErrNoRoute) {
		t.Fatalf("drained generate = %v", err)
	}
}

func TestResilientGatewayOpenDeadlineDoesNotWaitForProvider(t *testing.T) {
	release := make(chan struct{})
	late := &scriptedStream{}
	inner := &fake.ModelGateway{StreamFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
		<-release
		return late, nil
	}}
	deadlines := StreamDeadlines{FirstToken: 20 * time.Millisecond, Idle: time.Second, Total: time.Second}
	gateway, err := NewResilientGateway(inner, deadlines, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = gateway.Stream(context.Background(), fullSelection(), validModelRequest())
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("open blocked for %s", elapsed)
	}
	var deadline *StreamDeadlineError
	if !errors.As(err, &deadline) || deadline.Kind != DeadlineFirstToken {
		t.Fatalf("error = %v", err)
	}
	close(release)
	limit := time.Now().Add(time.Second)
	for late.closes() == 0 && time.Now().Before(limit) {
		time.Sleep(time.Millisecond)
	}
	if late.closes() != 1 {
		t.Fatalf("late stream closes = %d", late.closes())
	}
}

func TestResilientGatewayGenerateRetriesWithinTotalDeadline(t *testing.T) {
	clock := routerNow
	calls := 0
	inner := &fake.ModelGateway{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
		calls++
		if calls < 3 {
			return domain.ModelResult{}, errors.New("provider reset")
		}
		return domain.ModelResult{Output: []byte("ok")}, nil
	}}
	gateway, err := NewResilientGateway(inner, testDeadlines(), 3, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	gateway.Now = func() time.Time { return clock }
	gateway.Rand = func() float64 { return 1 }
	gateway.Sleep = func(_ context.Context, delay time.Duration) error {
		clock = clock.Add(delay)
		return nil
	}
	result, err := gateway.Generate(context.Background(), fullSelection(), validModelRequest())
	if err != nil || string(result.Output) != "ok" || calls != 3 {
		t.Fatalf("result = %+v, calls = %d, err = %v", result, calls, err)
	}

	// A policy rejection is never retried.
	calls = 0
	inner.GenerateFunc = func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
		calls++
		return domain.ModelResult{}, domain.ErrBudgetExceeded
	}
	if _, err = gateway.Generate(context.Background(), fullSelection(), validModelRequest()); !errors.Is(err, domain.ErrBudgetExceeded) || calls != 1 {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}
