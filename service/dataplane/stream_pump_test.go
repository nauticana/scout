package dataplane

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
	"github.com/nauticana/scout/internal/stage"
)

type publishRecorder struct {
	frames []domain.TurnReply
	fail   error
}

func (recorder *publishRecorder) Publish(_ context.Context, reply domain.TurnReply) error {
	if recorder.fail != nil {
		return recorder.fail
	}
	recorder.frames = append(recorder.frames, reply)
	return nil
}

func chunkStream(chunks ...domain.ModelChunk) *fake.ModelStream {
	index := 0
	return &fake.ModelStream{
		ReceiveFunc: func(context.Context) (domain.ModelChunk, error) {
			if index >= len(chunks) {
				return domain.ModelChunk{}, io.EOF
			}
			chunk := chunks[index]
			index++
			return chunk, nil
		},
		CloseFunc: func() error { return nil },
	}
}

var pumpTurn = domain.TurnRequest{
	TenantContext:  domain.TenantContext{TenantID: 1},
	RequestID:      "r",
	ConversationID: "c",
}

func TestStreamPumpGuardsAndPublishesEveryChunk(t *testing.T) {
	recorder := &publishRecorder{}
	guarded := 0
	pump := &StreamPump{
		Guardrails: &fake.GuardrailEnforcer{AfterModelChunkFunc: func(_ context.Context, _ domain.GuardrailConfig, _ domain.GuardrailSubject, chunk domain.ModelChunk) (domain.ModelChunk, error) {
			guarded++
			chunk.Payload = append([]byte("ok:"), chunk.Payload...)
			return chunk, nil
		}},
		Publisher: recorder,
	}
	usage, err := pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream(
		domain.ModelChunk{Payload: []byte("a"), Usage: domain.Usage{OutputTokens: 1}},
		domain.ModelChunk{Payload: []byte("b"), FinishReason: "stop", Usage: domain.Usage{OutputTokens: 1}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if usage.OutputTokens != 2 || guarded != 2 || len(recorder.frames) != 2 {
		t.Fatalf("usage=%+v guarded=%d frames=%d", usage, guarded, len(recorder.frames))
	}
	if string(recorder.frames[0].Payload) != "ok:a" || recorder.frames[0].Final {
		t.Fatalf("first frame = %+v", recorder.frames[0])
	}
	last := recorder.frames[1]
	if !last.Final || last.Sequence != 1 || last.AgentVersion != "v1" || last.ReplyRoute != "route" {
		t.Fatalf("final frame = %+v", last)
	}
}

func TestStreamPumpSynthesizesFinalOnBareEOF(t *testing.T) {
	recorder := &publishRecorder{}
	pump := &StreamPump{Guardrails: &fake.GuardrailEnforcer{}, Publisher: recorder}
	if _, err := pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.frames) != 1 || !recorder.frames[0].Final {
		t.Fatalf("frames = %+v", recorder.frames)
	}
}

func TestStreamPumpTruncatesAtTokenBudget(t *testing.T) {
	recorder := &publishRecorder{}
	closed := false
	stream := &fake.ModelStream{
		ReceiveFunc: func(context.Context) (domain.ModelChunk, error) {
			return domain.ModelChunk{Payload: []byte("x"), Usage: domain.Usage{OutputTokens: 5}}, nil
		},
		CloseFunc: func() error { closed = true; return nil },
	}
	pump := &StreamPump{Guardrails: &fake.GuardrailEnforcer{}, Publisher: recorder, MaxOutputTokens: 5}
	usage, err := pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, stream)
	if err != nil {
		t.Fatal(err)
	}
	if usage.OutputTokens != 5 || !closed {
		t.Fatalf("usage=%+v closed=%v", usage, closed)
	}
	last := recorder.frames[len(recorder.frames)-1]
	if !last.Final || last.ErrorCode != ErrorCodeTruncated {
		t.Fatalf("final = %+v", last)
	}
}

func TestStreamPumpDropsChunkThatOvershootsTokenBudget(t *testing.T) {
	recorder := &publishRecorder{}
	pump := &StreamPump{Guardrails: &fake.GuardrailEnforcer{}, Publisher: recorder, MaxOutputTokens: 5}
	usage, err := pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream(
		domain.ModelChunk{Payload: []byte("too much"), Usage: domain.Usage{OutputTokens: 6}},
	))
	if err != nil || usage.OutputTokens != 6 {
		t.Fatalf("usage=%+v error=%v", usage, err)
	}
	if len(recorder.frames) != 1 || len(recorder.frames[0].Payload) != 0 || recorder.frames[0].ErrorCode != ErrorCodeTruncated {
		t.Fatalf("frames = %+v", recorder.frames)
	}
}

func TestStreamPumpStageAttribution(t *testing.T) {
	guardrailErr := errors.New("blocked")
	pump := &StreamPump{
		Guardrails: &fake.GuardrailEnforcer{AfterModelChunkFunc: func(_ context.Context, _ domain.GuardrailConfig, _ domain.GuardrailSubject, _ domain.ModelChunk) (domain.ModelChunk, error) {
			return domain.ModelChunk{}, guardrailErr
		}},
		Publisher: &publishRecorder{},
	}
	_, err := pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream(domain.ModelChunk{Payload: []byte("a")}))
	var stageErr *stage.Error
	if !errors.As(err, &stageErr) || stageErr.Stage != domain.StageGuardrail || !errors.Is(err, guardrailErr) {
		t.Fatalf("guardrail stage = %v", err)
	}

	modelErr := errors.New("provider down")
	pump = &StreamPump{Guardrails: &fake.GuardrailEnforcer{}, Publisher: &publishRecorder{}}
	stream := &fake.ModelStream{
		ReceiveFunc: func(context.Context) (domain.ModelChunk, error) { return domain.ModelChunk{}, modelErr },
		CloseFunc:   func() error { return nil },
	}
	_, err = pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, stream)
	if !errors.As(err, &stageErr) || stageErr.Stage != domain.StageModel {
		t.Fatalf("model stage = %v", err)
	}

	publishErr := errors.New("broker down")
	pump = &StreamPump{Guardrails: &fake.GuardrailEnforcer{}, Publisher: &publishRecorder{fail: publishErr}}
	_, err = pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream(domain.ModelChunk{Payload: []byte("a")}))
	if !errors.As(err, &stageErr) || stageErr.Stage != domain.StagePublish {
		t.Fatalf("publish stage = %v", err)
	}
}

func TestStreamPumpGuardrailFailurePublishesNoPayload(t *testing.T) {
	recorder := &publishRecorder{}
	pump := &StreamPump{
		Guardrails: &fake.GuardrailEnforcer{AfterModelChunkFunc: func(_ context.Context, _ domain.GuardrailConfig, _ domain.GuardrailSubject, _ domain.ModelChunk) (domain.ModelChunk, error) {
			return domain.ModelChunk{}, errors.New("blocked")
		}},
		Publisher: recorder,
	}
	pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream(domain.ModelChunk{Payload: []byte("secret")}))
	for _, published := range recorder.frames {
		if len(published.Payload) != 0 {
			t.Fatalf("raw payload escaped: %+v", published)
		}
	}
}

// stepClock returns start + n*step on the nth call so TTFT and TPOT are exact.
func stepClock(start time.Time, step time.Duration) func() time.Time {
	calls := 0
	return func() time.Time {
		now := start.Add(time.Duration(calls) * step)
		calls++
		return now
	}
}

func TestStreamPumpObservesTTFTAndTPOT(t *testing.T) {
	var observed []domain.Observation
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	turn := pumpTurn
	turn.TenantContext.Tier = "gold"
	turn.TenantContext.PriorityClass = "interactive"
	turn.TenantContext.Region = "eu"
	pump := &StreamPump{
		Guardrails: &fake.GuardrailEnforcer{},
		Publisher:  &publishRecorder{},
		Observer:   &fake.ObservationRecorder{RecordObservationFunc: func(_ context.Context, o domain.Observation) { observed = append(observed, o) }},
		Now:        stepClock(start, 10*time.Millisecond),
	}
	// Clock calls: Begin(0ms), frame1(10ms), frame2(20ms), frame3(30ms), End(40ms).
	_, err := pump.Run(context.Background(), turn, "route", "v3", domain.GuardrailConfig{}, chunkStream(
		domain.ModelChunk{Payload: []byte("a"), Usage: domain.Usage{OutputTokens: 2}},
		domain.ModelChunk{Payload: []byte("b"), Usage: domain.Usage{OutputTokens: 2}},
		domain.ModelChunk{Payload: []byte("c"), FinishReason: "stop", Usage: domain.Usage{OutputTokens: 1}},
	))
	if err != nil || len(observed) != 1 {
		t.Fatalf("observed=%d err=%v", len(observed), err)
	}
	got := observed[0]
	if got.Stage != domain.StageModel || got.Component != StreamPumpComponent || got.Outcome != domain.OutcomeOK || got.ErrorClass != "" {
		t.Fatalf("observation = %+v", got)
	}
	if got.TenantID != 1 || got.TenantTier != "gold" || got.PriorityClass != "interactive" || got.Region != "eu" || got.Versions.Agent != "v3" {
		t.Fatalf("attribution = %+v", got)
	}
	// TTFT = 10ms; TPOT = (30ms-10ms)/(5-1) = 5ms; duration = 40ms.
	if got.TimeToFirst != 10*time.Millisecond || got.TimePerOutput != 5*time.Millisecond || got.Duration != 40*time.Millisecond || got.Usage.OutputTokens != 5 {
		t.Fatalf("timing = ttft %s tpot %s duration %s usage %+v", got.TimeToFirst, got.TimePerOutput, got.Duration, got.Usage)
	}
}

func TestStreamPumpObservesCanceledAndFailedStages(t *testing.T) {
	var observed []domain.Observation
	observer := &fake.ObservationRecorder{RecordObservationFunc: func(_ context.Context, o domain.Observation) { observed = append(observed, o) }}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &fake.ModelStream{
		ReceiveFunc: func(ctx context.Context) (domain.ModelChunk, error) {
			cancel()
			return domain.ModelChunk{}, ctx.Err()
		},
		CloseFunc: func() error { return nil },
	}
	pump := &StreamPump{Guardrails: &fake.GuardrailEnforcer{}, Publisher: &publishRecorder{}, Observer: observer}
	if _, err := pump.Run(ctx, pumpTurn, "route", "v1", domain.GuardrailConfig{}, stream); err == nil {
		t.Fatal("expected cancellation error")
	}
	if len(observed) != 1 || observed[0].Outcome != domain.OutcomeCanceled || observed[0].ErrorClass != "canceled" || observed[0].TimeToFirst != 0 {
		t.Fatalf("canceled observation = %+v", observed)
	}

	observed = nil
	pump = &StreamPump{
		Guardrails: &fake.GuardrailEnforcer{AfterModelChunkFunc: func(context.Context, domain.GuardrailConfig, domain.GuardrailSubject, domain.ModelChunk) (domain.ModelChunk, error) {
			return domain.ModelChunk{}, domain.ErrForbidden
		}},
		Publisher: &publishRecorder{},
		Observer:  observer,
	}
	pump.Run(context.Background(), pumpTurn, "route", "v1", domain.GuardrailConfig{}, chunkStream(domain.ModelChunk{Payload: []byte("a")}))
	if len(observed) != 1 || observed[0].Stage != domain.StageGuardrail || observed[0].Outcome != domain.OutcomeError || observed[0].ErrorClass != "forbidden" {
		t.Fatalf("guardrail observation = %+v", observed)
	}
}
